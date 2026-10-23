package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/netconst"
	pkg "github.com/AitorConS/jerboa/internal/package"
)

// MkfsFunc creates an exec.Cmd that runs mkfs to package binaryPath into imgPath.
// manifest is the Nanos manifest string to pass on stdin.
// Defined here so callers can satisfy it without importing internal/tools.
type MkfsFunc func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd

// BuildConfig holds the parameters for building a unikernel image.
type BuildConfig struct {
	Platform string
	Packages []pkg.Reference
	// Name is the image name (e.g. "hello").
	Name string
	// Tag is the image tag (default "latest" if empty).
	Tag string
	// BinaryPath is the path to the static ELF binary to package.
	BinaryPath string
	// ProgramPath, when non-empty, is the in-image guest path at which the
	// program is placed and from which it is executed (e.g.
	// "usr/local/postgresql/bin/postgres"). The Nanos manifest's program field,
	// argv[0], and /proc/self/exe all resolve to this path, so binaries that
	// locate their installation prefix relative to their own executable (e.g.
	// postgres) and binaries with $ORIGIN-relative RPATHs resolve correctly.
	// Empty places the program flat at /program (the default for compiled and
	// interpreted-runtime builds, where the layout is irrelevant).
	ProgramPath string
	// MkfsRun invokes mkfs to produce the disk image.
	// Use internal/tools.ResolveMkfs to obtain a platform-appropriate func.
	MkfsRun MkfsFunc
	// Memory is the default VM memory string (e.g. "256M").
	Memory string
	// CPUs is the default number of virtual CPUs.
	CPUs int
	// PkgFiles is a list of package files to include in the image.
	// Each entry carries both the host path (on the build machine) and the
	// guest path (inside the Nanos image). For jerboa packages, GuestPath is
	// typically filepath.Base(HostPath). For ops packages, GuestPath
	// preserves the sysroot/ hierarchy (e.g. "lib/x86_64-linux-gnu/libc.so").
	PkgFiles []pkg.File
	// Entrypoint is the script or file to pass as the first argument to the
	// runtime binary (e.g. "hi.js" for Node.js). Empty for compiled languages.
	// Emitted with a leading "/" (image-root-relative).
	Entrypoint string
	// Args holds additional argv elements appended after Entrypoint (if set).
	// Used by lang="raw" builds to pass arguments to the resolved program
	// (e.g. ["-jar", "/app.jar"]). Each element is emitted as-is — use
	// absolute in-image paths for file arguments.
	Args []string
	// Env holds runtime environment variables to bake into the image manifest.
	// Sourced from ops package.manifest Env fields and language driver output.
	Env map[string]string
	// Port is the service port declared for the image. It is metadata only:
	// network config is injected at run time (fw_cfg / boot args), not baked
	// into the manifest.
	Port int
	// Ports holds default host:guest port-publish specs (from [run] ports).
	// Stored in the image manifest and applied at run time when the VM joins a
	// network and no -p flag is given.
	Ports []string
	// DiskSize is the minimum boot disk size (e.g. "512M", "1G"), leaving free
	// space for runtime writes. It is recorded as Config.MinDiskSize and
	// applied when a VM starts; the stored image is not padded with zeros.
	DiskSize string
	// Layout is LayoutStandard (BIOS-bootable) or LayoutCompact (root
	// filesystem only, for hypervisors that load the kernel directly).
	Layout string
	// Output is where mkfs subprocess output is written. Nil defaults to os.Stderr.
	Output io.Writer
	// UUID is the root filesystem UUID (32 hex digits) written to the manifest
	// for mkfs. Build fills it from the image contents (see ContentUUID), so
	// identical inputs produce a byte-identical disk.
	UUID string
	// Created is the image creation time; zero means now. Builds pass the
	// time from SOURCE_DATE_EPOCH to make the manifest reproducible too.
	Created time.Time
	// Toolset identifies the kernel toolset MkfsRun uses. When it is set and
	// the builder has a Recorder, the build is recorded as an image recipe.
	Toolset *Toolset
}

// Builder produces unikernel images from ELF binaries and stores them.
type Builder struct {
	store    *Store
	recorder Recorder
}

// NewBuilder returns a Builder that stores images in store.
func NewBuilder(store *Store) *Builder {
	return &Builder{store: store}
}

// WithRecorder returns the builder with r told about every build whose
// config names its Toolset.
func (b *Builder) WithRecorder(r Recorder) *Builder {
	b.recorder = r
	return b
}

// Toolset identifies the kernel toolset an image is built with: its version
// and the boot and kernel images mkfs reads. An image recipe records them so
// any host can assemble the same disk.
type Toolset struct {
	Version string
	Boot    string
	Kernel  string
}

// BuildRecord describes a finished build to a Recorder. It is only valid
// during the Record call: the build's temporary inputs are removed after it.
type BuildRecord struct {
	// Manifest is the stored manifest, with its disk digest.
	Manifest Manifest
	// Config is the final build config, including injected files and the UUID.
	Config BuildConfig
	// Files lists every host file in the image, the program included.
	Files []pkg.File
	// RenderManifest renders the mkfs manifest with each host path replaced
	// by hostRef(path).
	RenderManifest func(hostRef func(string) string) string
}

// Recorder keeps a record of a build, such as its image recipe, and returns
// the digest to remember with the image tag.
type Recorder interface {
	Record(ctx context.Context, rec BuildRecord) (string, error)
}

// Build packages binaryPath into a disk image and registers it in the store.
func (b *Builder) Build(ctx context.Context, cfg BuildConfig) (Manifest, error) {
	if err := validateBuildConfig(cfg); err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}
	if cfg.Tag == "" {
		cfg.Tag = "latest"
	}
	if cfg.Memory == "" {
		cfg.Memory = "256M"
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if err := checkELF(cfg.BinaryPath); err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}
	minDisk, err := ParseDiskSize(cfg.DiskSize)
	if err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}

	platform, err := pkg.ValidateImage(cfg.BinaryPath, cfg.ProgramPath, cfg.PkgFiles, cfg.Platform)
	if err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}
	architecture := strings.TrimPrefix(platform, "linux/")
	if err := pkg.ValidateReferences(cfg.Packages, platform); err != nil {
		return Manifest{}, err
	}

	tmp, err := os.CreateTemp("", "jerboa-build-*.img")
	if err != nil {
		return Manifest{}, fmt.Errorf("build: create temp image: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return Manifest{}, fmt.Errorf("build: close temp: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }()

	// Bake /etc/resolv.conf so the guest's libc resolver points at the daemon's
	// guest DNS server, letting VMs resolve each other by name on their network.
	// Without this the guest has no nameserver and every lookup fails.
	// cleanup is always non-nil; defer it before the error check so a temp file
	// created just before a WriteString/Close failure is still removed.
	resolvCleanup, resolvErr := injectResolvConf(&cfg)
	defer resolvCleanup()
	if resolvErr != nil {
		return Manifest{}, fmt.Errorf("build: %w", resolvErr)
	}

	// Include daemon-injected files in the final path/collision check.
	validationProgram := cfg.ProgramPath
	if validationProgram == "" {
		validationProgram = "program"
	}
	finalFiles := append(append([]pkg.File{}, cfg.PkgFiles...), pkg.File{HostPath: cfg.BinaryPath, GuestPath: validationProgram})
	if err := pkg.ValidateFiles(finalFiles, platform); err != nil {
		return Manifest{}, err
	}

	uuid, err := ContentUUID(cfg)
	if err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}
	cfg.UUID = uuid
	manifest := BuildManifest(cfg)
	if err := runMkfs(ctx, cfg.MkfsRun, tmpPath, cfg.BinaryPath, manifest, cfg.Output); err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}
	if err := checkImageLayout(tmpPath, cfg.Layout); err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}

	stat, err := os.Stat(tmpPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("build: stat image: %w", err)
	}

	m := Manifest{
		SchemaVersion: SchemaVersion,
		Architecture:  architecture,
		Platform:      platform,
		Packages:      cfg.Packages,
		Name:          cfg.Name,
		Tag:           cfg.Tag,
		Created:       created(cfg.Created),
		Config: Config{
			Memory:      cfg.Memory,
			CPUs:        cfg.CPUs,
			Ports:       cfg.Ports,
			MinDiskSize: minDisk,
		},
		DiskSize: stat.Size(),
		Layout:   cfg.Layout,
	}

	// PutMove hashes the image once and renames it into the store instead of
	// copying — the deferred os.Remove above becomes a no-op on success.
	m, err = b.store.PutMove(cfg.Name, cfg.Tag, m, tmpPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("build: store: %w", err)
	}
	if b.recorder != nil && cfg.Toolset != nil {
		m = b.record(ctx, m, cfg)
	}
	return m, nil
}

// record stores the build's recipe. The image itself is already built and
// stored, so a failure only means it cannot be pushed until it is rebuilt.
func (b *Builder) record(ctx context.Context, m Manifest, cfg BuildConfig) Manifest {
	program := cfg.ProgramPath
	if program == "" {
		program = "program"
	}
	files := append(append([]pkg.File{}, cfg.PkgFiles...), pkg.File{HostPath: cfg.BinaryPath, GuestPath: program})
	digest, err := b.recorder.Record(ctx, BuildRecord{
		Manifest: m,
		Config:   cfg,
		Files:    files,
		RenderManifest: func(hostRef func(string) string) string {
			return buildManifest(cfg, hostRef)
		},
	})
	if err != nil {
		slog.Warn("build: image recipe not recorded; the image cannot be pushed", "image", m.Ref(), "err", err)
		return m
	}
	if err := b.store.SetRecipe(m.Ref(), digest); err != nil {
		slog.Warn("build: image recipe not linked to its tag", "image", m.Ref(), "err", err)
		return m
	}
	m.Recipe = digest
	return m
}

// resolvConf is the /etc/resolv.conf baked into every image. It is the same on
// every build host so images are reproducible and portable: Linux and macOS
// networks serve DNS at netconst.DNSAnycastIP, and macOS user-mode (slirp)
// networking answers there too from VMM releases that alias it. 10.0.2.3 is
// the fallback for older VMMs, whose slirp DNS only answers at that address.
var resolvConf = "nameserver " + netconst.DNSAnycastIP + "\nnameserver 10.0.2.3\noptions timeout:1\n"

// injectResolvConf writes a temporary /etc/resolv.conf pointing at the daemon's
// guest DNS server and appends it to cfg.PkgFiles so mkfs bakes it into the
// image. It returns a cleanup func (always non-nil) to remove the temp file.
// If a package already ships /etc/resolv.conf it is left untouched.
func injectResolvConf(cfg *BuildConfig) (cleanup func(), err error) {
	cleanup = func() {}
	for _, f := range cfg.PkgFiles {
		if strings.TrimPrefix(filepath.ToSlash(f.GuestPath), "/") == "etc/resolv.conf" {
			return cleanup, nil
		}
	}
	tmp, err := os.CreateTemp("", "jerboa-resolv-*.conf")
	if err != nil {
		return cleanup, fmt.Errorf("create resolv.conf: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup = func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.WriteString(resolvConf); err != nil {
		_ = tmp.Close()
		return cleanup, fmt.Errorf("write resolv.conf: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup, fmt.Errorf("close resolv.conf: %w", err)
	}
	cfg.PkgFiles = append(cfg.PkgFiles, pkg.File{HostPath: tmpPath, GuestPath: "etc/resolv.conf"})
	return cleanup, nil
}

func validateBuildConfig(cfg BuildConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("name is required")
	}
	if cfg.BinaryPath == "" {
		return fmt.Errorf("binary path is required")
	}
	if cfg.MkfsRun == nil {
		return fmt.Errorf("MkfsRun is required")
	}
	if cfg.Layout != LayoutStandard && cfg.Layout != LayoutCompact {
		return fmt.Errorf("invalid image layout %q", cfg.Layout)
	}
	return nil
}

func checkELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open binary %s: %w", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			_ = err // best effort
		}
	}()
	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil {
		return fmt.Errorf("read binary %s: %w", path, err)
	}
	if magic[0] != 0x7f || magic[1] != 'E' || magic[2] != 'L' || magic[3] != 'F' {
		return fmt.Errorf("%s is not an ELF binary", path)
	}
	return nil
}

func runMkfs(ctx context.Context, mkfsRun MkfsFunc, imgPath, binaryPath string, manifest string, output io.Writer) error {
	cmd := mkfsRun(ctx, imgPath, binaryPath, manifest)
	if output != nil {
		cmd.Stdout = output
		cmd.Stderr = output
	} else {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs: %w", err)
	}
	return nil
}

// BuildManifest constructs a Nanos manifest that includes the main program and
// any additional package files. Guest paths with directory separators (e.g.
// "lib/x86_64-linux-gnu/libc.so.6" from ops sysroot packages) are serialized
// as nested nodes — the Nanos manifest parser treats '/' as an unknown
// discriminator and rejects flat slash-separated keys.
// cfg.Entrypoint, if non-empty, is emitted as arguments:(0:/program 1:/<entrypoint> ...)
// so that the runtime interpreter (e.g. node, python) receives its own path as
// argv[0] and the script path as argv[1] on startup. cfg.Args, if non-empty,
// is appended after the entrypoint (or starting at argv[1] if cfg.Entrypoint
// is empty) — used by lang="raw" builds to pass arguments such as
// ["-jar", "/app.jar"] to the resolved program. The Nanos tuple parser
// reads '(' as a tuple of name:value pairs — not a bare value list — so
// arguments must use integer-string keys rather than e.g. ("/<entrypoint>").
// cfg.Env entries are emitted as environment:(KEY:val ...) sorted by key.
// Static network config is not baked into the manifest: the daemon injects the
// assigned TAP IP at run time (QEMU fw_cfg or Firecracker boot args).
func BuildManifest(cfg BuildConfig) string {
	return buildManifest(cfg, func(host string) string { return host })
}

// ContentUUID derives the image's filesystem UUID from what the image holds:
// the manifest with every host path replaced by the SHA-256 of that file's
// contents. Host paths are temporary build-context locations and never enter
// it, so the same program, files, arguments, environment and layout give the
// same UUID on any machine, and mkfs then writes a byte-identical disk.
func ContentUUID(cfg BuildConfig) (string, error) {
	cfg.UUID = ""
	digests := make(map[string]string)
	var firstErr error
	text := buildManifest(cfg, func(host string) string {
		if d, ok := digests[host]; ok {
			return d
		}
		// Shares the digest cache that package and collision checks fill, so
		// a build hashes each file once.
		d, err := pkg.FileDigest(host)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("content uuid: %w", err)
		}
		digests[host] = d
		return d
	})
	if firstErr != nil {
		return "", firstErr
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16]), nil
}

// created returns t in UTC, or the current time when t is zero.
func created(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

// buildManifest renders the Nanos manifest; hostRef maps each absolute host
// path to the value written for it.
func buildManifest(cfg BuildConfig, hostRef func(string) string) string {
	absBin, _ := filepath.Abs(cfg.BinaryPath)

	// progGuest is the slash-separated, root-relative path at which the program
	// lives in the image. Default "program" (→ /program). A ProgramPath override
	// runs the binary from its real package location so self-locating programs
	// and $ORIGIN-relative RPATHs resolve.
	progGuest := "program"
	if cfg.ProgramPath != "" {
		// Reject inputs that normalize to the image root (e.g. "/", ".", "bin/..");
		// an empty guest path would emit program:/ and place the binary at the root
		// instead of an executable path. Fall back to the flat /program layout.
		// path.Clean (not filepath.Clean) keeps this OS-independent: the guest path
		// is always slash-separated, and filepath.Clean would mangle "//" on Windows.
		if cleaned := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(cfg.ProgramPath)), "/"); cleaned != "" {
			progGuest = cleaned
		}
	}
	progRef := "/" + progGuest

	root := newManifestNode()

	for _, f := range cfg.PkgFiles {
		guestPath := f.GuestPath
		if guestPath == "" {
			guestPath = filepath.Base(f.HostPath)
		}
		if f.IsDir {
			insertManifestDir(root, filepath.ToSlash(guestPath))
		} else {
			abs, _ := filepath.Abs(f.HostPath)
			insertManifestFile(root, filepath.ToSlash(guestPath), abs)
		}
	}

	// Place the program node last so it is authoritative when ProgramPath
	// coincides with a package file already present at that path (the raw/ops
	// case, where the same binary is also streamed among the package files).
	insertManifestFile(root, progGuest, absBin)

	var b strings.Builder
	b.WriteString("(\n    children:(\n")
	writeManifestChildren(&b, root, "        ", hostRef)
	fmt.Fprintf(&b, "    )\n    program:%s\n", manifestValue(progRef))
	var argv []string
	if cfg.Entrypoint != "" {
		argv = append(argv, "/"+filepath.ToSlash(cfg.Entrypoint))
	}
	argv = append(argv, cfg.Args...)
	if len(argv) > 0 {
		fmt.Fprintf(&b, "    arguments:(0:%s", manifestValue(progRef))
		for i, a := range argv {
			fmt.Fprintf(&b, " %d:%s", i+1, manifestValue(a))
		}
		b.WriteString(")\n")
	}
	b.WriteString("    environment:(")
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(manifestName(k))
			b.WriteByte(':')
			b.WriteString(manifestValue(cfg.Env[k]))
			b.WriteByte(' ')
		}
	}
	b.WriteString(")\n")
	// Consumed by mkfs (like imagesize): omit the boot code and boot filesystem.
	if cfg.Layout == LayoutCompact {
		b.WriteString("    compact:true\n")
	}
	// Consumed by mkfs: a fixed root filesystem UUID. An older mkfs keeps it as
	// unused metadata and picks a random UUID, which still boots.
	if cfg.UUID != "" {
		fmt.Fprintf(&b, "    uuid:%s\n", cfg.UUID)
	}
	// Static network config is injected at run time from the daemon-assigned
	// TAP IP — via QEMU fw_cfg (opt/uni/network → net_inject) or Firecracker
	// boot args (en1.ipaddr=…). The old build-time "network:(ip:10.0.2.15…)"
	// section was a SLIRP-era constant that init_network_iface never read (it
	// looks for an "en1"/root tuple, not a "network" child), so it is gone.
	b.WriteString(")")
	return b.String()
}

// BuildVolumeManifest constructs a Nanos manifest containing only a filesystem
// tree (no program, no boot/kernel), suitable for seeding a data volume with
// mkfs. Each entry in files is placed at its guest path; the resulting children
// tree becomes the volume's root filesystem when mkfs writes it without boot or
// kernel images. Used to pre-populate a volume with, e.g., an initialized
// database data directory so it persists across VM lifecycles.
func BuildVolumeManifest(files []pkg.File) string {
	root := newManifestNode()
	for _, f := range files {
		guestPath := f.GuestPath
		if guestPath == "" {
			guestPath = filepath.Base(f.HostPath)
		}
		if f.IsDir {
			insertManifestDir(root, filepath.ToSlash(guestPath))
		} else {
			abs, _ := filepath.Abs(f.HostPath)
			insertManifestFile(root, filepath.ToSlash(guestPath), abs)
		}
	}

	var b strings.Builder
	b.WriteString("(\n    children:(\n")
	writeManifestChildren(&b, root, "        ", func(host string) string { return host })
	b.WriteString("    )\n)")
	return b.String()
}

// manifestNode is a node in the Nanos manifest filesystem tree.
type manifestNode struct {
	hostPath string
	children map[string]*manifestNode
}

func newManifestNode() *manifestNode {
	return &manifestNode{children: make(map[string]*manifestNode)}
}

// insertManifestFile inserts a file at the given slash-separated guest path.
func insertManifestFile(node *manifestNode, guestPath, hostPath string) {
	parts := strings.FieldsFunc(guestPath, func(r rune) bool { return r == '/' })
	cur := node
	for i, part := range parts {
		if i == len(parts)-1 {
			cur.children[part] = &manifestNode{hostPath: hostPath}
		} else {
			if cur.children[part] == nil {
				cur.children[part] = newManifestNode()
			}
			cur = cur.children[part]
		}
	}
}

// insertManifestDir ensures a directory node exists at the given slash-separated
// guest path. Called for empty directories from package sysroots so mkfs creates
// them in the TFS image even when they contain no files.
func insertManifestDir(node *manifestNode, guestPath string) {
	parts := strings.FieldsFunc(guestPath, func(r rune) bool { return r == '/' })
	cur := node
	for _, part := range parts {
		if cur.children[part] == nil {
			cur.children[part] = newManifestNode()
		}
		cur = cur.children[part]
	}
}

// manifestNameTerminals are the characters the Nanos tuple parser treats as
// name terminators (whitespace, parens, brackets, colons, pipes). Package
// trees can contain filenames with such characters (e.g. "Lorem ipsum.txt",
// "script (dev).tmpl"), which would otherwise truncate the parsed name and
// derail the parser — so those names must be quoted.
const manifestNameTerminals = " \t\n\r()[]:|/\"\\"

// manifestName returns name formatted for use as a manifest tuple key, quoting
// it when it contains characters the tuple parser would treat as terminators.
// The parser's quoted-string reader only recognizes "\" as an escape for the
// following literal character, so only '"' and '\' need escaping.
func manifestName(name string) string {
	if !strings.ContainsAny(name, manifestNameTerminals) {
		return name
	}
	return quoteManifestString(name)
}

// manifestValueTerminals are value terminators in the Nanos tuple parser.
// Values have a smaller terminal set than names: only whitespace and parens/brackets.
// Unlike names, values may contain ':', '/', '|' unquoted.
const manifestValueTerminals = " \t\n\r()[]"

// quoteManifestString always quotes s, escaping quotes and backslashes, so no
// file name can end the string early and add keys of its own.
func quoteManifestString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// manifestValue returns v formatted for use as a manifest tuple value, quoting
// it when it contains characters the tuple parser would treat as terminators.
func manifestValue(v string) string {
	if v != "" && !strings.ContainsAny(v, manifestValueTerminals+"\"\\") {
		return v
	}
	return quoteManifestString(v)
}

// writeManifestChildren serializes the children of node into b at the given indent level.
// Directory nodes wrap their entries in a nested children:(...) scope — the Nanos manifest
// parser only descends into a directory when it finds a children: key inside the node.
func writeManifestChildren(b *strings.Builder, node *manifestNode, indent string, hostRef func(string) string) {
	keys := make([]string, 0, len(node.children))
	for k := range node.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		child := node.children[key]
		b.WriteString(indent)
		b.WriteString(manifestName(key))
		b.WriteString(":")
		if child.hostPath != "" {
			// Quoted: host paths can contain spaces or parens (e.g. extracted
			// package files like "Lorem ipsum.txt" or "script (dev).tmpl"),
			// which the tuple parser would otherwise treat as value terminators.
			b.WriteString(`(contents:(host:`)
			b.WriteString(quoteManifestString(filepath.ToSlash(hostRef(child.hostPath))))
			b.WriteString(`))` + "\n")
		} else {
			inner := indent + "    "
			b.WriteString("(\n")
			b.WriteString(inner)
			b.WriteString("children:(\n")
			writeManifestChildren(b, child, inner+"    ", hostRef)
			b.WriteString(inner)
			b.WriteString(")\n")
			b.WriteString(indent)
			b.WriteString(")\n")
		}
	}
}

// ParseDiskSize parses a disk size the way mkfs does: an integer (decimal, or
// 0x hexadecimal) optionally followed by k, m or g (any case, binary units)
// and an optional "B" or "iB". An empty string is zero.
func ParseDiskSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, nil
	}
	i := 0
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		i = 2
		for i < len(t) && strings.ContainsRune("0123456789abcdefABCDEF", rune(t[i])) {
			i++
		}
	} else {
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
		}
	}
	n, err := strconv.ParseInt(t[:i], 0, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid disk size %q", s)
	}
	unit := strings.ToLower(t[i:])
	unit = strings.TrimSuffix(strings.TrimSuffix(unit, "ib"), "b")
	mul := map[string]int64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30}[unit]
	if mul == 0 || n > math.MaxInt64/mul {
		return 0, fmt.Errorf("invalid disk size %q", s)
	}
	return n * mul, nil
}
