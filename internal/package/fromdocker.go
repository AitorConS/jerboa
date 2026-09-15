package pkg

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// maxSymlinkHops bounds symlink resolution so a cyclic or maliciously deep
// symlink chain in an exported container filesystem cannot loop forever.
const maxSymlinkHops = 40

// defaultLibDirs are the conventional shared-library search directories probed,
// in order, when resolving a DT_NEEDED soname that is not itself a path. They are
// container-absolute so they index directly into the exported filesystem.
var defaultLibDirs = []string{
	"/lib/x86_64-linux-gnu",
	"/usr/lib/x86_64-linux-gnu",
	"/lib64",
	"/usr/lib64",
	"/lib",
	"/usr/lib",
	"/lib/aarch64-linux-gnu",
	"/usr/lib/aarch64-linux-gnu",
	"/usr/local/lib",
}

// FromDocker extracts the program binary at containerPath from a Docker image,
// together with the shared-library closure it needs to run, and returns them as
// File entries whose GuestPath is each file's real absolute path inside the
// container (leading slash trimmed — e.g. "usr/local/bin/redis-server",
// "lib64/ld-linux-x86-64.so.2"). The program binary is always the first entry.
//
// Preserving those paths is essential: a dynamically linked ELF names its
// interpreter (PT_INTERP, e.g. /lib64/ld-linux-x86-64.so.2) and its libraries
// (DT_NEEDED, resolved under /lib, /usr/lib, …) by absolute path, so the files
// must land at exactly those paths in the image. The previous implementation
// packaged every file under its basename at the image root, which made preflight
// reject the image ("interpreter … is not in the image") for essentially every
// real Docker binary.
//
// Extraction reads the container's exported filesystem (docker export) instead of
// running cat/ldd inside a temporary container, so it works on scratch and
// distroless images that ship no shell or coreutils, and it never creates
// a symlink on the host — symlinks are followed logically against an in-memory
// index, which matters on Windows where creating symlinks needs privileges.
//
// The returned cleanup removes the staging directory that the Files' HostPaths
// live in; call it once the Files have been packaged.
func FromDocker(image, containerPath string, extraLibs []string, platforms ...string) (files []File, cleanup func(), err error) {
	cfs, cleanupExport, err := exportContainerFS(image, platforms...)
	if err != nil {
		return nil, nil, err
	}
	defer cleanupExport()

	closure, err := cfs.elfClosure(containerPath)
	if err != nil {
		return nil, nil, fmt.Errorf("from-docker: %w", err)
	}
	for _, l := range extraLibs {
		if rp, rerr := cfs.resolve(l); rerr == nil {
			closure = appendUnique(closure, rp)
		} else {
			return nil, nil, fmt.Errorf("from-docker: extra lib %q: %w", l, rerr)
		}
	}

	tmpDir, err := os.MkdirTemp("", "jerboa-pkg-from-docker-*")
	if err != nil {
		return nil, nil, fmt.Errorf("from-docker: temp dir: %w", err)
	}
	stagingCleanup := func() { _ = os.RemoveAll(tmpDir) }

	files = make([]File, 0, len(closure))
	for i, real := range closure {
		resolved, rerr := cfs.resolve(real)
		if rerr != nil {
			stagingCleanup()
			return nil, nil, rerr
		}
		data, rerr := cfs.readFile(resolved)
		if rerr != nil {
			stagingCleanup()
			return nil, nil, fmt.Errorf("from-docker: read %s: %w", real, rerr)
		}
		// Host temp names are indexed to stay unique even when two closure files
		// share a basename (e.g. libc.so.6 present under multiple dirs).
		host := filepath.Join(tmpDir, fmt.Sprintf("%02d_%s", i, path.Base(real)))
		if werr := os.WriteFile(host, data, 0o755); werr != nil {
			stagingCleanup()
			return nil, nil, fmt.Errorf("from-docker: stage %s: %w", real, werr)
		}
		files = append(files, File{
			HostPath:  host,
			GuestPath: strings.TrimPrefix(real, "/"),
		})
	}
	return files, stagingCleanup, nil
}

// containerFS is an in-memory index of a container's root filesystem exported as
// a tar. It lets us resolve absolute paths and symlinks and read file contents
// without materializing the (possibly symlink-laden) tree on the host.
type containerFS struct {
	root    string
	tarPath string
	hosts   map[string]string
	index   map[string]cfsEntry // clean absolute path ("/usr/bin/x") -> entry
}

type cfsEntry struct {
	digest   [32]byte
	typeflag byte
	linkname string // symlink/hardlink target as recorded in the tar
}

// exportContainerFS creates a throwaway container from image, exports its merged
// filesystem to a temp tar, and indexes every entry. The returned cleanup removes
// the temp tar. The container is removed before returning.
func exportContainerFS(image string, platforms ...string) (*containerFS, func(), error) {
	if err := ensureDockerImage(image); err != nil {
		return nil, nil, err
	}

	args := []string{"create"}
	if len(platforms) > 0 {
		if err := ValidatePlatform(platforms[0]); err != nil {
			return nil, nil, err
		}
		args = append(args, "--platform", platforms[0])
	}
	args = append(args, image)
	out, err := exec.Command("docker", args...).Output() //nolint:noctx // interactive CLI call
	if err != nil {
		return nil, nil, fmt.Errorf("from-docker: docker create %s: %w", image, dockerErr(err))
	}
	cid := strings.TrimSpace(string(out))
	if cid == "" {
		return nil, nil, fmt.Errorf("from-docker: docker create %s returned no container id", image)
	}
	removeContainer := func() { _ = exec.Command("docker", "rm", "-f", cid).Run() } //nolint:noctx // cleanup

	tarFile, err := os.CreateTemp("", "jerboa-docker-export-*.tar")
	if err != nil {
		removeContainer()
		return nil, nil, fmt.Errorf("from-docker: temp tar: %w", err)
	}
	tarPath := tarFile.Name()
	_ = tarFile.Close()
	cleanup := func() { _ = os.Remove(tarPath) }

	if err := exec.Command("docker", "export", "-o", tarPath, cid).Run(); err != nil { //nolint:noctx // interactive CLI call
		removeContainer()
		cleanup()
		return nil, nil, fmt.Errorf("from-docker: docker export %s: %w", image, dockerErr(err))
	}
	removeContainer()

	index, err := indexTar(tarPath)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("from-docker: index export: %w", err)
	}
	return &containerFS{tarPath: tarPath, index: index}, cleanup, nil
}

// indexTar reads every header in the tar at path and records its type and link
// target, keyed by clean absolute path. Contents are not buffered — readFile
// re-scans the tar for the one file it needs, so the whole rootfs is never held
// in memory at once.
func indexTar(tarPath string) (map[string]cfsEntry, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("open export tar: %w", err)
	}
	defer func() { _ = f.Close() }()

	index := make(map[string]cfsEntry)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		entry := cfsEntry{typeflag: hdr.Typeflag, linkname: hdr.Linkname}
		if hdr.Typeflag == tar.TypeReg {
			h := sha256.New()
			if _, err := io.Copy(h, tr); err != nil {
				return nil, fmt.Errorf("container filesystem operation: %w", err)
			}
			copy(entry.digest[:], h.Sum(nil))
		}
		name := absClean(hdr.Name)
		if old, ok := index[name]; ok && old != entry {
			return nil, fmt.Errorf("export collision at %s", name)
		}
		index[name] = entry
	}
	return index, nil
}

// resolve maps a container path to the clean absolute path of the regular file it
// ultimately refers to. It walks the path one component at a time — like the
// kernel — so a symlink anywhere along the way is followed, not just at the leaf.
// This matters because real images use directory symlinks heavily (Debian's
// usrmerge makes /lib, /lib64, /bin symlinks into /usr), so a library at
// /lib/x86_64-linux-gnu/libc.so.6 physically lives under /usr/lib. It errors when
// a component is missing or when the target is not a regular file.
func (c *containerFS) resolve(p string) (string, error) {
	// Explicit local mappings overlay the source filesystem, including usrmerge
	// directory aliases. They describe the final guest path of this input file.
	if _, ok := c.hosts[absClean(p)]; ok && c.index[absClean(p)].typeflag == tar.TypeReg {
		return absClean(p), nil
	}

	work := splitAbs(p)
	var out []string // components resolved so far (below root)
	hops := 0

	for i := 0; i < len(work); i++ {
		part := work[i]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		cur := "/" + strings.Join(append(append([]string{}, out...), part), "/")
		entry, ok := c.entry(cur)
		if !ok {
			// An unrecorded intermediate component is treated as a plain directory
			// (some exports omit directory entries); only a missing leaf is fatal.
			if i == len(work)-1 {
				return "", fmt.Errorf("%q not found in image", cur)
			}
			out = append(out, part)
			continue
		}

		switch entry.typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			hops++
			if hops > maxSymlinkHops {
				return "", fmt.Errorf("%q: too many symlink hops (cycle?)", p)
			}
			var base []string
			if strings.HasPrefix(entry.linkname, "/") || entry.typeflag == tar.TypeLink {
				base = splitAbs(entry.linkname)
			} else {
				base = append(append([]string{}, out...), splitAbs(entry.linkname)...)
			}
			// Re-walk from the (absolute) link target with the remaining components
			// appended, since target components may themselves be symlinks.
			work = append(base, work[i+1:]...)
			out = out[:0]
			i = -1
		case tar.TypeReg:
			if i != len(work)-1 {
				return "", fmt.Errorf("%q: %q is not a directory", p, cur)
			}
			out = append(out, part)
		default: // directory (or other) — descend
			out = append(out, part)
		}
	}

	final := "/" + strings.Join(out, "/")
	entry, ok := c.entry(final)
	if !ok {
		return "", fmt.Errorf("%q not found in image", final)
	}
	if entry.typeflag != tar.TypeReg {
		return "", fmt.Errorf("%q is not a regular file", final)
	}
	return final, nil
}

// splitAbs splits an absolute-ized, cleaned path into its non-empty components.
func splitAbs(p string) []string {
	cleaned := strings.TrimPrefix(filepath.ToSlash(p), "/")
	if cleaned == "" {
		return nil
	}
	return strings.Split(cleaned, "/")
}

// readFile returns the contents of the regular file at the clean absolute path
// real (as returned by resolve). It scans the tar for that single entry.
func (c *containerFS) readFile(real string) ([]byte, error) {
	if c.hosts != nil {
		if host, ok := c.hosts[real]; ok {
			data, err := os.ReadFile(host)
			if err != nil {
				return nil, fmt.Errorf("read container file %s: %w", real, err)
			}
			return data, nil
		}
		if c.root != "" {
			data, err := os.ReadFile(filepath.Join(c.root, real))
			if err != nil {
				return nil, fmt.Errorf("read container root file %s: %w", real, err)
			}
			return data, nil
		}
		return nil, fmt.Errorf("missing %s", real)
	}
	f, err := os.Open(c.tarPath)
	if err != nil {
		return nil, fmt.Errorf("open export tar: %w", err)
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("scan tar: %w", err)
		}
		if absClean(hdr.Name) != real {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxExtractedBytes))
		if err != nil {
			return nil, fmt.Errorf("read %q from export: %w", real, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%q not present in export", real)
}

// elfClosure returns the resolved absolute paths making up the runtime closure of
// the binary at binPath: the binary itself (first), its ELF interpreter, and the
// transitive DT_NEEDED shared libraries. A statically linked binary (no
// interpreter, no needed libraries) yields just itself. Non-ELF targets and
// missing or incompatible dependencies are rejected. Requested guest paths
// are retained even when the underlying content is reached through a symlink.
func (c *containerFS) elfClosure(binPath string) ([]string, error) {
	start := absClean(binPath)
	result := []string{}
	seen := map[string]bool{}
	type pending struct {
		guest     string
		inherited []string
	}
	queue := []pending{{guest: start}}
	platform := ""
	for len(queue) > 0 {
		item := queue[0]
		cur := item.guest
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		real, err := c.resolve(cur)
		if err != nil {
			return nil, err
		}
		data, err := c.readFile(real)
		if err != nil {
			return nil, err
		}
		info, err := readELFInfo(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cur, err)
		}
		if platform == "" {
			platform = info.platform
		}
		if info.platform != platform {
			return nil, fmt.Errorf("mixed architectures: %s is %s, want %s", cur, info.platform, platform)
		}
		result = append(result, cur)
		if info.interp != "" {
			queue = append(queue, pending{guest: info.interp})
		}
		inherited := append(expandOrigin(info.rpath, path.Dir(cur)), item.inherited...)
		searchDirs := append(append([]string{}, inherited...), expandOrigin(info.runpath, path.Dir(cur))...)
		for _, dir := range defaultLibDirs {
			if platform == "linux/arm64" && strings.Contains(dir, "x86_64") || platform == "linux/amd64" && strings.Contains(dir, "aarch64") {
				continue
			}
			searchDirs = append(searchDirs, dir)
		}
		for _, soname := range info.needed {
			candidate := ""
			if strings.Contains(soname, "/") {
				candidate = absClean(soname)
			} else {
				for _, dir := range searchDirs {
					p := path.Join(dir, soname)
					if _, err := c.resolve(p); err == nil {
						candidate = p
						break
					}
				}
			}
			if candidate == "" {
				return nil, fmt.Errorf("%s: missing dependency %s", cur, soname)
			}
			queue = append(queue, pending{guest: candidate, inherited: inherited})
		}
	}
	return result, nil
}

// elfInfo is the subset of ELF dynamic metadata needed to walk a binary's
// shared-library closure.
type elfInfo struct {
	platform string
	interp   string   // PT_INTERP (dynamic linker), empty for a static binary
	needed   []string // DT_NEEDED sonames
	runpath  []string // DT_RUNPATH applies to direct dependencies only
	rpath    []string // DT_RPATH is inherited by descendants
}

// readELFInfo parses ELF dynamic metadata from an in-memory image. It returns an
// error for non-ELF data so callers can treat that as "no dependencies".
func readELFInfo(data []byte) (*elfInfo, error) {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse elf: %w", err)
	}
	defer func() { _ = f.Close() }()

	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
		return nil, fmt.Errorf("requires little-endian ELF64")
	}
	if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return nil, fmt.Errorf("unsupported ELF ABI %s", f.OSABI)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return nil, fmt.Errorf("ELF is not executable or a shared library")
	}
	info := &elfInfo{}
	switch f.Machine {
	case elf.EM_AARCH64:
		info.platform = "linux/arm64"
	case elf.EM_X86_64:
		info.platform = "linux/amd64"
	default:
		return nil, fmt.Errorf("unsupported ELF architecture %s", f.Machine)
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		if p.Filesz == 0 || p.Filesz > 4096 {
			return nil, fmt.Errorf("invalid PT_INTERP size")
		}
		buf := make([]byte, p.Filesz)
		if _, err := p.ReadAt(buf, 0); err != nil {
			return nil, fmt.Errorf("container filesystem operation: %w", err)
		}
		if buf[len(buf)-1] != 0 {
			return nil, fmt.Errorf("unterminated PT_INTERP")
		}
		info.interp = strings.TrimRight(string(buf), "\x00")
		if !strings.HasPrefix(info.interp, "/") {
			return nil, fmt.Errorf("interpreter must be absolute")
		}
	}
	if f.SectionByType(elf.SHT_DYNAMIC) != nil {
		needed, err := f.DynString(elf.DT_NEEDED)
		if err != nil {
			return nil, fmt.Errorf("invalid dynamic dependencies: %w", err)
		}
		info.needed = needed
	}
	if rp, err := f.DynString(elf.DT_RUNPATH); err == nil && len(rp) > 0 {
		info.runpath = splitLibPath(rp)
	} else if rp, err := f.DynString(elf.DT_RPATH); err == nil && len(rp) > 0 {
		info.rpath = splitLibPath(rp)
	}
	return info, nil
}

// splitLibPath splits colon-separated RPATH/RUNPATH values (each element may hold
// several dirs) into individual directory entries.
func splitLibPath(entries []string) []string {
	var dirs []string
	for _, e := range entries {
		for _, d := range strings.Split(e, ":") {
			if d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	return dirs
}

// expandOrigin substitutes $ORIGIN (and ${ORIGIN}) in RPATH/RUNPATH entries with
// the directory of the referring binary, as the dynamic linker does, so packages
// that ship libraries beside their binary (e.g. via -rpath '$ORIGIN/../lib')
// resolve correctly.
func expandOrigin(dirs []string, origin string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		d = strings.ReplaceAll(d, "${ORIGIN}", origin)
		d = strings.ReplaceAll(d, "$ORIGIN", origin)
		out = append(out, absClean(d))
	}
	return out
}

// ensureDockerImage makes sure image is available locally, pulling it if a cheap
// inspect fails. docker create/export need the image present but do not always
// pull it themselves.
func ensureDockerImage(image string) error {
	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil { //nolint:noctx // interactive CLI call
		return nil
	}
	if err := exec.Command("docker", "pull", image).Run(); err != nil { //nolint:noctx // interactive CLI call
		return fmt.Errorf("from-docker: pull %s: %w", image, dockerErr(err))
	}
	return nil
}

// dockerErr enriches a failed docker exec with its stderr, which os/exec hides
// behind a bare "exit status N".
func dockerErr(err error) error {
	var ee *exec.ExitError
	if ok := errors.As(err, &ee); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// absClean returns p as a cleaned, slash-rooted absolute path ("/a/b"), so index
// keys and lookups agree regardless of leading "./" or missing leading slash in
// tar entry names.
func absClean(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func appendUnique(s []string, v string) []string {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}

func (c *containerFS) entry(p string) (cfsEntry, bool) {
	if e, ok := c.index[p]; ok {
		return e, true
	}
	if c.root == "" {
		return cfsEntry{}, false
	}
	host := filepath.Join(c.root, p)
	info, err := os.Lstat(host)
	if err != nil {
		return cfsEntry{}, false
	}
	e := cfsEntry{typeflag: tar.TypeReg}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		e.typeflag = tar.TypeSymlink
		e.linkname, err = os.Readlink(host)
		if err != nil {
			return cfsEntry{}, false
		}
	case info.IsDir():
		e.typeflag = tar.TypeDir
	case !info.Mode().IsRegular():
		return cfsEntry{}, false
	}
	return e, true
}
