// Command jerboa-determinism builds a fixed, synthetic catalog of images with
// the real image builder and a given mkfs, and prints each image's disk digest
// as JSON. CI runs it on every host platform and requires identical output:
// image recipes are assembled by clients with their own mkfs, which only works
// while one toolset version produces byte-identical disks everywhere.
//
// Every input is generated from fixed seeds, so the catalog needs no compiler,
// package manager or network beyond the kernel files.
package main

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/image"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/recipe"
	"github.com/AitorConS/jerboa/internal/release"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jerboa-determinism:", err)
		os.Exit(1)
	}
}

func run() error {
	mkfs := flag.String("mkfs", "", "mkfs binary to test (required)")
	boot := flag.String("boot", "", "x86 boot.img (default: from the stable release)")
	kernel := flag.String("kernel", "", "x86 kernel.img (default: from the stable release)")
	out := flag.String("out", "", "write the digests JSON here as well as to stdout")
	verbose := flag.Bool("v", false, "show mkfs output")
	keep := flag.Bool("keep", false, "keep the generated files")
	flag.Parse()
	if *mkfs == "" {
		return fmt.Errorf("-mkfs is required")
	}
	ctx := context.Background()
	work, err := os.MkdirTemp("", "jerboa-determinism-*")
	if err != nil {
		return fmt.Errorf("work dir: %w", err)
	}
	defer func() {
		if *keep {
			fmt.Fprintln(os.Stderr, "kept", work)
			return
		}
		_ = os.RemoveAll(work)
	}()

	if *boot == "" || *kernel == "" {
		if *boot, *kernel, err = stableKernel(ctx, work); err != nil {
			return err
		}
	}
	mkfsRun := func(ctx context.Context, imgPath, _ string, manifest string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, *mkfs, "-b", *boot, "-k", *kernel, imgPath) //nolint:gosec // the mkfs under test is chosen by the caller
		cmd.Stdin = strings.NewReader(manifest)
		return cmd
	}

	digests := map[string]string{}
	for _, spec := range catalog() {
		dir := filepath.Join(work, spec.name)
		cfg, err := spec.materialize(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", spec.name, err)
		}
		cfg.MkfsRun = mkfsRun
		cfg.Output = io.Discard
		if *verbose {
			cfg.Output = os.Stderr
		}
		cfg.Toolset = &image.Toolset{Version: "0.0.0", Boot: *boot, Kernel: *kernel}
		blobs, err := cas.New(filepath.Join(dir, "blobs"))
		if err != nil {
			return fmt.Errorf("blob store: %w", err)
		}
		// Building twice catches nondeterminism within one host as well.
		var got []image.Manifest
		for i := range 2 {
			store, err := image.NewStore(filepath.Join(dir, fmt.Sprintf("store-%d", i)))
			if err != nil {
				return fmt.Errorf("image store: %w", err)
			}
			m, err := image.NewBuilder(store).WithRecorder(recipe.Recorder{Store: blobs}).Build(ctx, cfg)
			if err != nil {
				return fmt.Errorf("%s: %w", spec.name, err)
			}
			got = append(got, m)
		}
		if got[0].DiskDigest != got[1].DiskDigest || got[0].Recipe != got[1].Recipe {
			return fmt.Errorf("%s: two builds on this host differ", spec.name)
		}
		// The recorded recipe must rebuild the same disk from the blob store.
		r, err := recipe.Load(blobs, got[0].Recipe)
		if err != nil {
			return fmt.Errorf("%s: %w", spec.name, err)
		}
		if err := recipe.Assemble(ctx, r, blobs, *mkfs, filepath.Join(dir, "assembled.img")); err != nil {
			return fmt.Errorf("%s: assemble from recipe: %w", spec.name, err)
		}
		digests[spec.name] = got[0].DiskDigest
		digests[spec.name+".recipe"] = got[0].Recipe
	}

	data, err := json.MarshalIndent(digests, "", "  ")
	if err != nil {
		return fmt.Errorf("encode digests: %w", err)
	}
	data = append(data, '\n')
	fmt.Fprintf(os.Stderr, "built and reassembled %d images on %s/%s\n", len(catalog()), runtime.GOOS, runtime.GOARCH)
	if *out != "" {
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *out, err)
		}
	}
	if _, err := os.Stdout.Write(data); err != nil {
		return fmt.Errorf("write digests: %w", err)
	}
	return nil
}

// stableKernel downloads the x86 boot.img and kernel.img of the stable channel,
// so every host feeds mkfs the same bytes.
func stableKernel(ctx context.Context, dir string) (string, string, error) {
	cl, err := release.Default()
	if err != nil {
		return "", "", err
	}
	m, err := cl.FetchManifest(ctx, release.ChannelStable)
	if err != nil {
		return "", "", err
	}
	k, ok := m.Component(release.ComponentKernel)
	if !ok {
		return "", "", fmt.Errorf("stable manifest has no kernel")
	}
	paths := map[string]string{}
	for _, name := range []string{"boot.img", "kernel.img"} {
		a, ok := k.Files[name]
		if !ok {
			return "", "", fmt.Errorf("stable kernel has no %s", name)
		}
		paths[name] = filepath.Join(dir, name)
		if err := cl.DownloadArtifact(ctx, a, paths[name]); err != nil {
			return "", "", err
		}
	}
	return paths["boot.img"], paths["kernel.img"], nil
}

// imageSpec is one catalog entry: the files it holds and how it is configured.
type imageSpec struct {
	name   string
	layout string
	files  func(r *rand.Rand, add func(guest string, data []byte, exec bool), addDir func(guest string))
	config func(cfg *image.BuildConfig)
}

func catalog() []imageSpec {
	return []imageSpec{
		{name: "small", layout: image.LayoutCompact, files: func(r *rand.Rand, add func(string, []byte, bool), _ func(string)) {
			add("etc/app.conf", []byte("listen = 8080\n"), false)
			add("usr/share/app/README", randomText(r, 2048), false)
		}},
		// Thousands of small files in deep directories: the shape of a Node or
		// Python dependency tree, which stresses tuple encoding order.
		{name: "tree", layout: image.LayoutCompact, files: func(r *rand.Rand, add func(string, []byte, bool), addDir func(string)) {
			for i := range 4000 {
				depth := 1 + r.IntN(12)
				parts := make([]string, depth)
				for d := range parts {
					parts[d] = fmt.Sprintf("d%02d_%s", r.IntN(6), name(r))
				}
				size := r.IntN(64 << 10)
				if r.IntN(10) == 0 {
					size = 0
				}
				add(strings.Join(parts, "/")+fmt.Sprintf("/f%04d.%s", i, ext(r)), randomText(r, size), r.IntN(8) == 0)
			}
			for i := range 20 {
				addDir(fmt.Sprintf("var/empty/e%02d", i))
			}
		}},
		// A large file with incompressible and zero regions, like a runtime binary.
		{name: "large", layout: image.LayoutCompact, files: func(r *rand.Rand, add func(string, []byte, bool), _ func(string)) {
			big := make([]byte, 48<<20)
			for off := 0; off < len(big); off += 1 << 20 {
				if r.IntN(4) != 0 {
					fillRandom(r, big[off:off+1<<20])
				}
			}
			add("usr/local/bin/runtime", big, true)
			for i := range 300 {
				add(fmt.Sprintf("usr/local/lib/mod%03d.so", i), randomText(r, 1+r.IntN(16<<10)), true)
			}
		}},
		// A BIOS-bootable image with every manifest feature a build can set.
		{name: "standard", layout: image.LayoutStandard, files: func(r *rand.Rand, add func(string, []byte, bool), _ func(string)) {
			add("app/server.js", randomText(r, 4096), false)
			add("app/lib/util.js", randomText(r, 1024), false)
		}, config: func(cfg *image.BuildConfig) {
			cfg.Entrypoint = "app/server.js"
			cfg.Args = []string{"--port", "8080", "--name=déterminisme"}
			cfg.Env = map[string]string{"NODE_ENV": "production", "B": "2", "A": "1", "EMPTY": ""}
			cfg.Ports = []string{"8080:8080"}
			cfg.DiskSize = "256M"
			cfg.Memory = "512M"
			cfg.CPUs = 2
		}},
	}
}

func (s imageSpec) materialize(dir string) (image.BuildConfig, error) {
	// A seeded PCG, not crypto/rand: the inputs must repeat exactly on every host.
	seed := uint64(len(s.name))<<32 | uint64(s.name[0])
	r := rand.New(rand.NewPCG(0x6a6572626f61, seed)) //nolint:gosec // deterministic test data by design
	root := filepath.Join(dir, "files")
	var files []pkg.File
	var ferr error
	add := func(guest string, data []byte, exec bool) {
		if ferr != nil {
			return
		}
		host := filepath.Join(root, filepath.FromSlash(guest))
		if ferr = os.MkdirAll(filepath.Dir(host), 0o755); ferr != nil {
			return
		}
		mode := os.FileMode(0o644)
		if exec {
			mode = 0o755
		}
		if ferr = os.WriteFile(host, data, mode); ferr != nil {
			return
		}
		files = append(files, pkg.File{HostPath: host, GuestPath: guest})
	}
	addDir := func(guest string) { files = append(files, pkg.File{GuestPath: guest, IsDir: true}) }
	s.files(r, add, addDir)
	if ferr != nil {
		return image.BuildConfig{}, fmt.Errorf("write catalog files: %w", ferr)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].GuestPath < files[j].GuestPath })

	program := filepath.Join(dir, "program")
	if err := os.WriteFile(program, staticELF(), 0o755); err != nil {
		return image.BuildConfig{}, fmt.Errorf("write program: %w", err)
	}
	cfg := image.BuildConfig{
		Name:       s.name,
		Tag:        "determinism",
		Platform:   "linux/amd64",
		BinaryPath: program,
		PkgFiles:   files,
		Layout:     s.layout,
		Created:    time.Unix(1704067200, 0).UTC(),
	}
	if s.config != nil {
		s.config(&cfg)
	}
	return cfg, nil
}

func name(r *rand.Rand) string {
	// Lowercase only: the files are written to the build host first, and
	// macOS file systems are case-insensitive.
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789_-"
	b := make([]byte, 3+r.IntN(10))
	for i := range b {
		b[i] = letters[r.IntN(len(letters))]
	}
	return string(b)
}

func ext(r *rand.Rand) string {
	return []string{"js", "py", "json", "pyc", "so", "txt", "map", "d.ts"}[r.IntN(8)]
}

func randomText(r *rand.Rand, n int) []byte {
	words := []string{"const", "return", "import", "function", "self", "value", "error", "async", "\n", "    "}
	b := make([]byte, 0, n)
	for len(b) < n {
		b = append(b, words[r.IntN(len(words))]...)
		b = append(b, ' ')
	}
	return b[:n]
}

func fillRandom(r *rand.Rand, b []byte) {
	for i := 0; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.Uint64())
	}
}

// staticELF returns a minimal static x86-64 ELF executable that the image
// builder accepts as a program; it is never run.
func staticELF() []byte {
	b := make([]byte, 2048)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	le := binary.LittleEndian
	le.PutUint16(b[16:], uint16(elf.ET_EXEC))
	le.PutUint16(b[18:], uint16(elf.EM_X86_64))
	le.PutUint32(b[20:], 1)
	le.PutUint64(b[32:], 64)  // program header offset
	le.PutUint64(b[40:], 128) // section header offset
	le.PutUint16(b[52:], 64)
	le.PutUint16(b[54:], 56)
	le.PutUint16(b[58:], 64)
	le.PutUint16(b[60:], 2)
	// Section 1: an empty string table.
	le.PutUint32(b[128+64+4:], uint32(elf.SHT_STRTAB))
	le.PutUint64(b[128+64+24:], 512)
	le.PutUint64(b[128+64+32:], 1)
	return b
}
