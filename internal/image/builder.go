package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pkg "github.com/AitorConS/unikernel-engine/internal/package"
)

// MkfsFunc creates an exec.Cmd that runs mkfs to package binaryPath into imgPath.
// manifest is the Nanos manifest string to pass on stdin.
// Defined here so callers can satisfy it without importing internal/tools.
type MkfsFunc func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd

// BuildConfig holds the parameters for building a unikernel image.
type BuildConfig struct {
	// Name is the image name (e.g. "hello").
	Name string
	// Tag is the image tag (default "latest" if empty).
	Tag string
	// BinaryPath is the path to the static ELF binary to package.
	BinaryPath string
	// MkfsRun invokes mkfs to produce the disk image.
	// Use internal/tools.ResolveMkfs to obtain a platform-appropriate func.
	MkfsRun MkfsFunc
	// Memory is the default VM memory string (e.g. "256M").
	Memory string
	// CPUs is the default number of virtual CPUs.
	CPUs int
	// PkgFiles is a list of package files to include in the image.
	// Each entry carries both the host path (on the build machine) and the
	// guest path (inside the Nanos image). For uni packages, GuestPath is
	// typically filepath.Base(HostPath). For ops packages, GuestPath
	// preserves the sysroot/ hierarchy (e.g. "lib/x86_64-linux-gnu/libc.so").
	PkgFiles []pkg.File
}

// Builder produces unikernel images from ELF binaries and stores them.
type Builder struct {
	store *Store
}

// NewBuilder returns a Builder that stores images in store.
func NewBuilder(store *Store) *Builder {
	return &Builder{store: store}
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

	tmp, err := os.CreateTemp("", "uni-build-*.img")
	if err != nil {
		return Manifest{}, fmt.Errorf("build: create temp image: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return Manifest{}, fmt.Errorf("build: close temp: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }()

	manifest := BuildManifest(cfg.BinaryPath, cfg.PkgFiles)
	if err := runMkfs(ctx, cfg.MkfsRun, tmpPath, cfg.BinaryPath, manifest); err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}

	stat, err := os.Stat(tmpPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("build: stat image: %w", err)
	}
	digest, err := fileSHA256(tmpPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("build: %w", err)
	}

	m := Manifest{
		SchemaVersion: SchemaVersion,
		Name:          cfg.Name,
		Tag:           cfg.Tag,
		Created:       time.Now().UTC(),
		Config: Config{
			Memory: cfg.Memory,
			CPUs:   cfg.CPUs,
		},
		DiskDigest: digest,
		DiskSize:   stat.Size(),
	}

	if err := b.store.Put(cfg.Name, cfg.Tag, m, tmpPath); err != nil {
		return Manifest{}, fmt.Errorf("build: store: %w", err)
	}
	return m, nil
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

func runMkfs(ctx context.Context, mkfsRun MkfsFunc, imgPath, binaryPath string, manifest string) error {
	cmd := mkfsRun(ctx, imgPath, binaryPath, manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs: %w", err)
	}
	return nil
}

// BuildManifest constructs a Nanos manifest that includes the main program and
// any additional package files. Guest paths with directory separators (e.g.
// "lib/x86_64-linux-gnu/libc.so.6" from ops sysroot packages) are serialised
// as nested nodes — the Nanos manifest parser treats '/' as an unknown
// discriminator and rejects flat slash-separated keys.
func BuildManifest(binaryPath string, pkgFiles []pkg.File) string {
	absBin, _ := filepath.Abs(binaryPath)

	root := newManifestNode()
	root.children["program"] = &manifestNode{hostPath: absBin}

	for _, f := range pkgFiles {
		abs, _ := filepath.Abs(f.HostPath)
		guestPath := f.GuestPath
		if guestPath == "" {
			guestPath = filepath.Base(f.HostPath)
		}
		insertManifestFile(root, filepath.ToSlash(guestPath), abs)
	}

	var b strings.Builder
	b.WriteString("(\n    children:(\n")
	writeManifestChildren(&b, root, "        ")
	b.WriteString("    )\n    program:/program\n    environment:()\n)")
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

// writeManifestChildren serialises the children of node into b at the given indent level.
func writeManifestChildren(b *strings.Builder, node *manifestNode, indent string) {
	keys := make([]string, 0, len(node.children))
	for k := range node.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		child := node.children[key]
		b.WriteString(indent)
		b.WriteString(key)
		b.WriteString(":")
		if child.hostPath != "" {
			b.WriteString("(contents:(host:")
			b.WriteString(child.hostPath)
			b.WriteString("))\n")
		} else {
			b.WriteString("(\n")
			writeManifestChildren(b, child, indent+"    ")
			b.WriteString(indent)
			b.WriteString(")\n")
		}
	}
}
