package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Provenance records reproducible source identity, never host paths or image environment.
type Provenance struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference,omitempty"`
	ImageID   string `json:"image_id,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

type Reference struct {
	Source   string `json:"source"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	SHA256   string `json:"sha256"`
	// ContentDigest identifies the package's files independent of its
	// archive (see ContentDigest). Absent in images built before it existed.
	ContentDigest string      `json:"content_digest,omitempty"`
	Provenance    *Provenance `json:"provenance,omitempty"`
}

var ErrPlatformMismatch = errors.New("platform mismatch")

// ErrContentMismatch reports that a package's files do not match the content
// digest its metadata records.
var ErrContentMismatch = errors.New("package content digest mismatch")

func ValidatePlatform(p string) error {
	if p != "linux/arm64" && p != "linux/amd64" {
		return fmt.Errorf("unsupported platform %q (want linux/arm64 or linux/amd64)", p)
	}
	return nil
}

func ELFPlatform(filename string) (string, error) {
	f, err := os.Open(filename) //nolint:gosec // caller-chosen program path
	if err != nil {
		return "", fmt.Errorf("platform operation: %w", err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	if format := hostExecutableFormat(head[:n]); format != "" {
		return "", fmt.Errorf("%s is a %s executable, not a Linux ELF binary: unikernel programs must be built for Linux "+
			"(for Go: CGO_ENABLED=0 GOOS=linux go build, or pass the source directory to jerboa build to cross-compile it)",
			filepath.Base(filename), format)
	}
	info, err := readELFInfoAt(f)
	if err != nil {
		return "", err
	}
	return info.platform, nil
}

// hostExecutableFormat names the desktop executable formats most often passed
// by mistake: a program compiled for the macOS or Windows host instead of Linux.
func hostExecutableFormat(data []byte) string {
	if len(data) >= 4 {
		switch binary.BigEndian.Uint32(data) {
		case 0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe, 0xcafebabe:
			return "macOS Mach-O"
		}
	}
	if len(data) >= 2 && data[0] == 'M' && data[1] == 'Z' {
		return "Windows PE"
	}
	return ""
}

// ValidateFiles validates the exact guest tree, including data collisions and every ELF.
func ValidateFiles(files []File, platform string) error {
	if err := ValidatePlatform(platform); err != nil {
		return err
	}
	// Contents are compared by digest, so no file is held in memory; digests
	// are cached per file identity and reused across checks (see fileDigest).
	seen := map[string][32]byte{}
	dirs := map[string]bool{}
	for _, f := range files {
		gp := guestPathOf(f)
		for _, c := range strings.Split(filepath.ToSlash(gp), "/") {
			if c == ".." {
				return fmt.Errorf("invalid guest path %q", gp)
			}
		}
		gp = tarEntryName(gp)
		if gp == "" {
			return fmt.Errorf("empty guest path")
		}
		if f.IsDir {
			dirs[gp] = true
			continue
		}
		sum, err := fileDigest(f.HostPath)
		if err != nil {
			return fmt.Errorf("platform operation: %w", err)
		}
		if old, ok := seen[gp]; ok && old != sum {
			return fmt.Errorf("different content collides at /%s", gp)
		}
		seen[gp] = sum
		if err := checkELFPlatform(f.HostPath, gp, platform); err != nil {
			return err
		}
	}
	for gp := range seen {
		if dirs[gp] {
			return fmt.Errorf("file/directory collision at /%s", gp)
		}
		for parent := path.Dir(gp); parent != "."; parent = path.Dir(parent) {
			if _, ok := seen[parent]; ok {
				return fmt.Errorf("file/directory collision at /%s", parent)
			}
		}
	}
	return nil
}

// checkELFPlatform rejects an ELF at hostPath built for another platform.
// Files that are not ELF are accepted.
func checkELFPlatform(hostPath, guestPath, platform string) error {
	f, err := os.Open(hostPath) //nolint:gosec // package store and build context paths
	if err != nil {
		return fmt.Errorf("platform operation: %w", err)
	}
	defer func() { _ = f.Close() }()
	magic := make([]byte, 4)
	if n, _ := io.ReadFull(f, magic); n < 4 || !bytes.Equal(magic, []byte{0x7f, 'E', 'L', 'F'}) {
		return nil
	}
	info, err := readELFInfoAt(f)
	if err != nil {
		return fmt.Errorf("/%s: %w", guestPath, err)
	}
	if info.platform != platform {
		return fmt.Errorf("%w: /%s: platform %s conflicts with %s", ErrPlatformMismatch, guestPath, info.platform, platform)
	}
	return nil
}

// ApplyContextPrecedence resolves the one kind of guest-path collision that has
// an obvious reading: a build-context file — the project's own source — shadows
// a package file at the same path, the way a COPY shadows what the base image
// left there. The shadowed package entry is dropped, so a project that ships its
// own README.md on top of a package carrying one still builds, with the
// project's copy in the image.
//
// Collisions with no such ordering — two package files, or two context files, at
// one guest path — are left in place for ValidateFiles to reject: there the
// caller really has asked for two different things at one path.
func ApplyContextPrecedence(files []File) []File {
	shadowed := map[string]bool{}
	for _, f := range files {
		if !f.FromContext || f.IsDir {
			continue
		}
		if gp := tarEntryName(guestPathOf(f)); gp != "" {
			shadowed[gp] = true
		}
	}
	if len(shadowed) == 0 {
		return files
	}
	out := make([]File, 0, len(files))
	for _, f := range files {
		if !f.FromContext && !f.IsDir && shadowed[tarEntryName(guestPathOf(f))] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// guestPathOf returns the path a File is placed at inside the image, before
// normalization: its GuestPath, or the host basename when none was given.
func guestPathOf(f File) string {
	if f.GuestPath != "" {
		return f.GuestPath
	}
	return filepath.Base(f.HostPath)
}

// ValidateImage reuses the import resolver against only the files going onto disk.
func ValidateImage(binary, program string, files []File, platform string) (string, error) {
	actual, err := ELFPlatform(binary)
	if err != nil {
		return "", err
	}
	if platform == "" {
		platform = actual
	}
	if platform != actual {
		return "", fmt.Errorf("program platform %s conflicts with requested %s", actual, platform)
	}
	if program == "" {
		program = "program"
	}
	all := append(append([]File{}, files...), File{HostPath: binary, GuestPath: program})
	if err := ValidateFiles(all, platform); err != nil {
		return "", err
	}
	c := &containerFS{index: map[string]cfsEntry{}, hosts: map[string]string{}}
	for _, f := range all {
		if !f.IsDir {
			gp := guestPathOf(f)
			c.index[absClean(gp)] = cfsEntry{typeflag: '0'}
			c.hosts[absClean(gp)] = f.HostPath
		}
	}
	_, err = c.elfClosure(program)
	return platform, err
}

func (s *Store) ForPlatform(platform string) (*Store, error) {
	if err := ValidatePlatform(platform); err != nil {
		return nil, err
	}
	return &Store{root: s.root, platform: platform}, nil
}

// Verify checks legacy content in place; it never rewrites the original package.
func (s *Store) Verify(p Package, platform string) ([]File, Reference, error) {
	selected := &Store{root: s.root}
	if p.Platform != "" {
		var err error
		selected, err = s.ForPlatform(p.Platform)
		if err != nil {
			return nil, Reference{}, err
		}
	}
	if p.Platform != "" && p.Platform != platform {
		return nil, Reference{}, fmt.Errorf("package platform %s conflicts with %s", p.Platform, platform)
	}
	// Digests saved by an earlier build are reused for files that have not
	// changed since, so an unchanged package is checked without being read.
	pkgDir := selected.PackageDir(p.Name, p.Version)
	SeedDigests(pkgDir)
	archivePath := filepath.Join(pkgDir, "files.tar.gz")
	sum, err := fileDigest(archivePath)
	if err != nil {
		return nil, Reference{}, fmt.Errorf("platform operation: %w", err)
	}
	hash := hex.EncodeToString(sum[:])
	if p.SHA256 != "" && !strings.EqualFold(hash, p.SHA256) {
		return nil, Reference{}, ErrChecksumMismatch
	}
	if err := selected.Extract(p); err != nil {
		return nil, Reference{}, err
	}
	files, err := selected.ExtractedFileList(p.Name, p.Version)
	if err != nil {
		return nil, Reference{}, err
	}
	if !archiveVerified(pkgDir, hash, files) {
		if err := verifyExtractedArchive(archivePath, files); err != nil {
			return nil, Reference{}, err
		}
	}
	if p.ProgramPath != "" {
		for i, f := range files {
			if tarEntryName(f.GuestPath) == tarEntryName(p.ProgramPath) {
				files[0], files[i] = files[i], files[0]
				break
			}
		}
	}
	if err := ValidateFiles(files, platform); err != nil {
		return nil, Reference{}, err
	}
	_ = SaveDigests(pkgDir, append(append([]File{}, files...), File{HostPath: archivePath}), hash)
	found := false
	for _, f := range files {
		if !f.IsDir {
			if _, err := ELFPlatform(f.HostPath); err == nil {
				found = true
			}
		}
	}
	if p.Platform == "" && !found {
		return nil, Reference{}, fmt.Errorf("legacy package %s has no ELF to verify its platform", p.Name)
	}
	content, err := ContentDigest(files)
	if err != nil {
		return nil, Reference{}, err
	}
	if p.ContentDigest != "" && content != p.ContentDigest {
		return nil, Reference{}, fmt.Errorf("package %s:%s: %w (content %s, want %s)", p.Name, p.Version, ErrContentMismatch, content, p.ContentDigest)
	}
	return files, Reference{Source: "jerboa", Name: p.Name, Version: p.Version, Platform: platform, SHA256: hash, ContentDigest: content, Provenance: p.Provenance}, nil
}

// ValidateReferences bounds metadata to concrete, portable package identities.
func ValidateReferences(refs []Reference, platform string) error {
	for _, r := range refs {
		if r.Source != "jerboa" && r.Source != "ops" {
			return fmt.Errorf("unknown package source %q", r.Source)
		}
		if r.Platform != platform {
			return fmt.Errorf("package %s platform %s conflicts with image %s", r.Name, r.Platform, platform)
		}
		if r.Version == "" || r.Version == "latest" {
			return fmt.Errorf("package %s requires a concrete version", r.Name)
		}
		if r.Source == "jerboa" {
			if err := validatePackageRef(r.Name, r.Version); err != nil {
				return err
			}
		} else {
			id, err := ParseOpsIdentifier(r.Name + ":" + r.Version)
			if err != nil {
				return err
			}
			if err := validateOpsRef(id.Namespace, id.Name, id.Version); err != nil {
				return err
			}
		}
		hash, err := hex.DecodeString(r.SHA256)
		if err != nil || len(hash) != 32 {
			return fmt.Errorf("package %s requires SHA-256", r.Name)
		}
		if r.ContentDigest != "" && !ValidContentDigest(r.ContentDigest) {
			return fmt.Errorf("package %s has an invalid content digest", r.Name)
		}
		if p := r.Provenance; p != nil {
			switch p.Kind {
			case "local":
				if p.Reference != "" || p.ImageID != "" || p.Digest != "" {
					return fmt.Errorf("local provenance must not contain host or Docker paths")
				}
			case "docker":
				if p.Reference == "" || !strings.HasPrefix(p.ImageID, "sha256:") || strings.HasPrefix(p.Reference, "/") || strings.Contains(p.Reference, "://") {
					return fmt.Errorf("invalid Docker provenance")
				}
			default:
				return fmt.Errorf("unknown provenance kind %q", p.Kind)
			}
		}
	}
	return nil
}

// Verify the cached extraction still matches the archive whose hash is recorded.
func verifyExtractedArchive(archivePath string, files []File) error {
	af, err := os.Open(archivePath) //nolint:gosec // package store path
	if err != nil {
		return fmt.Errorf("platform operation: %w", err)
	}
	defer func() { _ = af.Close() }()
	gz, err := gzip.NewReader(af)
	if err != nil {
		return fmt.Errorf("platform operation: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	expected := map[string][32]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("platform operation: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			return fmt.Errorf("platform operation: %w", err)
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		name := tarEntryName(hdr.Name)
		if old, ok := expected[name]; ok && old != sum {
			return fmt.Errorf("archive collision at %s", name)
		}
		expected[name] = sum
	}
	for _, f := range files {
		if f.IsDir {
			continue
		}
		got, err := fileDigest(f.HostPath)
		if err != nil {
			return fmt.Errorf("platform operation: %w", err)
		}
		name := tarEntryName(f.GuestPath)
		want, ok := expected[name]
		if !ok || got != want {
			return fmt.Errorf("cached package content differs from archive at %s", name)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		return fmt.Errorf("cached package extraction is incomplete")
	}
	return nil
}
