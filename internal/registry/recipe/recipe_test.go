package recipe

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/image"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/stretchr/testify/require"
)

// fakeMkfs writes a "disk" that depends only on what a real mkfs consumes:
// the manifest without host paths, then the bytes of every referenced file,
// boot and kernel included, in manifest order.
const fakeMkfs = `#!/bin/sh
for last; do :; done
manifest=$(cat)
printf '%s' "$manifest" | sed 's/host:"[^"]*"/host:X/g' > "$last"
cat "$2" "$4" >> "$last"
printf '%s' "$manifest" | grep -o 'host:"[^"]*"' | sed 's/^host:"//; s/"$//' | while IFS= read -r p; do cat "$p" >> "$last"; done
`

func staticELF() []byte {
	b := make([]byte, 512)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(b[18:], uint16(elf.EM_X86_64))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}

type fixture struct {
	mkfs  string
	store *cas.Store
	img   *image.Store
	r     *Recipe
	m     image.Manifest
}

func build(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	mkfs := filepath.Join(dir, "mkfs")
	require.NoError(t, os.WriteFile(mkfs, []byte(fakeMkfs), 0o755))
	write := func(name, data string) string {
		p := filepath.Join(dir, "in", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(data), 0o644))
		return p
	}
	program := filepath.Join(dir, "in", "program")
	require.NoError(t, os.MkdirAll(filepath.Dir(program), 0o755))
	require.NoError(t, os.WriteFile(program, staticELF(), 0o755))

	store, err := cas.New(filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	img, err := image.NewStore(filepath.Join(dir, "images"))
	require.NoError(t, err)
	cfg := image.BuildConfig{
		Name: "app", Tag: "v1", Platform: "linux/amd64", BinaryPath: program,
		Layout: image.LayoutStandard,
		PkgFiles: []pkg.File{
			{HostPath: write("server.js", "console.log(1)"), GuestPath: "app/server.js"},
			{HostPath: write("dup.js", "console.log(1)"), GuestPath: "app/copy.js"},
			{HostPath: write("with space (1).txt", "spaces"), GuestPath: "app/with space (1).txt"},
			{GuestPath: "var/empty", IsDir: true},
		},
		Env: map[string]string{"B": "2", "A": "1"},
		MkfsRun: func(ctx context.Context, out, _ string, manifest string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, mkfs, "-b", write("boot.img", "BOOT"), "-k", write("kernel.img", "KERNEL"), out)
			cmd.Stdin = strings.NewReader(manifest)
			return cmd
		},
		Toolset: &image.Toolset{Version: "0.2.3", Boot: filepath.Join(dir, "in", "boot.img"), Kernel: filepath.Join(dir, "in", "kernel.img")},
	}
	// Toolset inputs must exist before the build records them.
	write("boot.img", "BOOT")
	write("kernel.img", "KERNEL")
	m, err := image.NewBuilder(img).WithRecorder(Recorder{Store: store}).Build(context.Background(), cfg)
	require.NoError(t, err)
	require.NotEmpty(t, m.Recipe)
	r, err := Load(store, m.Recipe)
	require.NoError(t, err)
	return fixture{mkfs: mkfs, store: store, img: img, r: r, m: m}
}

func TestBuildRecordsRecipeThatAssemblesTheSameDisk(t *testing.T) {
	f := build(t)
	require.Equal(t, f.m.DiskDigest, f.r.DiskDigest)
	require.Equal(t, "v0.2.3", f.r.Toolset.Version)
	require.NotContains(t, f.r.Manifest, "/in/")
	// Identical files are one blob: program, two js files sharing content,
	// the spaced file, the injected resolv.conf, boot and kernel.
	require.Len(t, f.r.Blobs, 6)

	// The tag remembers its recipe.
	got, _, err := f.img.Get("app:v1")
	require.NoError(t, err)
	require.Equal(t, f.m.Recipe, got.Recipe)

	out := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, Assemble(context.Background(), f.r, f.store, f.mkfs, out))

	// A blob that changed in the store is caught by the disk digest.
	p, _ := f.store.Path(f.r.Toolset.Kernel)
	require.NoError(t, os.Chmod(p, 0o644))
	require.NoError(t, os.WriteFile(p, []byte("KERNEX"), 0o644))
	require.ErrorContains(t, Assemble(context.Background(), f.r, f.store, f.mkfs, out), "does not match")
	_, err = os.Stat(out)
	require.True(t, os.IsNotExist(err))
}

func TestParseRequiresCanonicalValidRecipe(t *testing.T) {
	f := build(t)
	data, err := f.r.Encode()
	require.NoError(t, err)
	_, err = Parse(data)
	require.NoError(t, err)

	_, err = Parse(append([]byte(" "), data...))
	require.ErrorContains(t, err, "canonical")

	mutate := func(fn func(r *Recipe)) error {
		c := *f.r
		c.Blobs = append([]Blob(nil), f.r.Blobs...)
		fn(&c)
		return c.Validate()
	}
	require.ErrorContains(t, mutate(func(r *Recipe) { r.Blobs = r.Blobs[1:] }), "not listed")
	require.ErrorContains(t, mutate(func(r *Recipe) {
		r.Blobs = append(r.Blobs, Blob{Digest: "sha256:" + strings.Repeat("f", 64), Size: 1})
	}), "not used")
	require.ErrorContains(t, mutate(func(r *Recipe) { r.Blobs[0], r.Blobs[1] = r.Blobs[1], r.Blobs[0] }), "sorted")
	require.ErrorContains(t, mutate(func(r *Recipe) { r.Manifest += `x:(contents:(host:"/etc/passwd"))` }), "host path")
	require.ErrorContains(t, mutate(func(r *Recipe) { r.Toolset.Version = "latest" }), "toolset version")
	require.ErrorContains(t, mutate(func(r *Recipe) { r.Architecture = "arm64" }), "architecture")
}

func TestAssembleNeedsEveryBlob(t *testing.T) {
	f := build(t)
	p, _ := f.store.Path(f.r.Blobs[0].Digest)
	require.NoError(t, os.Remove(p))
	err := Assemble(context.Background(), f.r, f.store, f.mkfs, filepath.Join(t.TempDir(), "d"))
	require.ErrorContains(t, err, "not in the store")
}
