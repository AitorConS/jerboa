package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pkg "github.com/AitorConS/jerboa/internal/package"
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
	// DiskSize is the minimum image file size passed to mkfs (e.g. "512M", "1G").
	// When non-empty, emitted as imagesize in the Nanos manifest so mkfs pads
	// the image to at least that size, leaving free space for runtime writes.
	DiskSize string
	// Output is where mkfs subprocess output is written. Nil defaults to os.Stderr.
	Output io.Writer
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

	tmp, err := os.CreateTemp("", "jerboa-build-*.img")
	if err != nil {
		return Manifest{}, fmt.Errorf("build: create temp image: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return Manifest{}, fmt.Errorf("build: close temp: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }()

	manifest := BuildManifest(cfg)
	if err := runMkfs(ctx, cfg.MkfsRun, tmpPath, cfg.BinaryPath, manifest, cfg.Output); err != nil {
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
	absBin, _ := filepath.Abs(cfg.BinaryPath)

	root := newManifestNode()
	root.children["program"] = &manifestNode{hostPath: absBin}

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

	var b strings.Builder
	b.WriteString("(\n    children:(\n")
	writeManifestChildren(&b, root, "        ")
	b.WriteString("    )\n    program:/program\n")
	var argv []string
	if cfg.Entrypoint != "" {
		argv = append(argv, "/"+filepath.ToSlash(cfg.Entrypoint))
	}
	argv = append(argv, cfg.Args...)
	if len(argv) > 0 {
		b.WriteString("    arguments:(0:/program")
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
	if cfg.DiskSize != "" {
		fmt.Fprintf(&b, "    imagesize:%s\n", cfg.DiskSize)
	}
	// Static network config is injected at run time from the daemon-assigned
	// TAP IP — via QEMU fw_cfg (opt/uni/network → net_inject) or Firecracker
	// boot args (en1.ipaddr=…). The old build-time "network:(ip:10.0.2.15…)"
	// section was a SLIRP-era constant that init_network_iface never read (it
	// looks for an "en1"/root tuple, not a "network" child), so it is gone.
	b.WriteString(")")
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
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range name {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// manifestValueTerminals are value terminators in the Nanos tuple parser.
// Values have a smaller terminal set than names: only whitespace and parens/brackets.
// Unlike names, values may contain ':', '/', '|' unquoted.
const manifestValueTerminals = " \t\n\r()[]"

// manifestValue returns v formatted for use as a manifest tuple value, quoting
// it when it contains characters the tuple parser would treat as terminators.
func manifestValue(v string) string {
	if !strings.ContainsAny(v, manifestValueTerminals+"\"\\") {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// writeManifestChildren serializes the children of node into b at the given indent level.
// Directory nodes wrap their entries in a nested children:(...) scope — the Nanos manifest
// parser only descends into a directory when it finds a children: key inside the node.
func writeManifestChildren(b *strings.Builder, node *manifestNode, indent string) {
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
			b.WriteString(`(contents:(host:"`)
			b.WriteString(filepath.ToSlash(child.hostPath))
			b.WriteString(`"))` + "\n")
		} else {
			inner := indent + "    "
			b.WriteString("(\n")
			b.WriteString(inner)
			b.WriteString("children:(\n")
			writeManifestChildren(b, child, inner+"    ")
			b.WriteString(inner)
			b.WriteString(")\n")
			b.WriteString(indent)
			b.WriteString(")\n")
		}
	}
}
