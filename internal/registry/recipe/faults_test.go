package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recipe is the only description of an image a registry hands out, and its
// blobs sit on a disk that can fail. These tests damage the recipe, the blobs
// and the assembly, and hold the package to its promise: Assemble leaves the
// recipe's disk at the destination, or nothing.

func scriptMkfs(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mkfs")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nfor last; do :; done\n"+body+"\n"), 0o755))
	return path
}

// Whatever mkfs does wrong, no file is left at the destination: a caller
// that finds a disk there may take it for the image.
func TestFaults_AssembleLeavesNoDiskWhenMkfsMisbehaves(t *testing.T) {
	f := build(t)
	cases := map[string]string{
		"fails before writing":    `exit 1`,
		"fails after half a disk": `printf 'half a disk' > "$last"; echo 'write: no space left on device' >&2; exit 28`,
		"killed while writing":    `printf 'half a disk' > "$last"; kill -9 $$`,
		"writes another disk":     `cat > /dev/null; printf 'some other disk' > "$last"`,
		"writes an empty disk":    `cat > /dev/null; : > "$last"`,
		"writes nothing":          `cat > /dev/null`,
		"writes one byte":         `cat > /dev/null; printf 'x' > "$last"`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "disk.img")
			err := Assemble(context.Background(), f.r, f.store, scriptMkfs(t, body), out)
			require.Error(t, err)
			assert.NoFileExists(t, out)

			// A disk from an earlier assembly does not survive a failed one.
			require.NoError(t, os.WriteFile(out, []byte("the previous image's disk"), 0o644))
			require.Error(t, Assemble(context.Background(), f.r, f.store, scriptMkfs(t, body), out))
			assert.NoFileExists(t, out, "the previous disk was left as if it were this image's")
		})
	}

	t.Run("mkfs is not there", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "disk.img")
		require.Error(t, Assemble(context.Background(), f.r, f.store, filepath.Join(t.TempDir(), "absent"), out))
		assert.NoFileExists(t, out)
	})
	t.Run("canceled while mkfs runs", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "disk.img")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.Error(t, Assemble(ctx, f.r, f.store, scriptMkfs(t, `printf 'half' > "$last"; sleep 60`), out))
		assert.NoFileExists(t, out)
	})
	t.Run("the destination cannot be written", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "absent", "dir", "disk.img")
		require.Error(t, Assemble(context.Background(), f.r, f.store, f.mkfs, out))
	})

	// With the real fake mkfs the same recipe still assembles.
	out := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, Assemble(context.Background(), f.r, f.store, f.mkfs, out))
}

// Each blob of the recipe damaged on disk in turn: the assembly fails on the
// disk digest and leaves nothing, whichever blob it was and however it was
// damaged.
func TestFaults_AssembleWithADamagedBlob(t *testing.T) {
	damages := map[string]func([]byte) []byte{
		"truncated": func(b []byte) []byte { return b[:len(b)/2] },
		"bit flip":  func(b []byte) []byte { c := bytes.Clone(b); c[len(c)/2] ^= 0x04; return c },
		"zeroed":    func(b []byte) []byte { return make([]byte, len(b)) },
		"grown":     func(b []byte) []byte { return append(bytes.Clone(b), 0) },
	}
	for name, how := range damages {
		t.Run(name, func(t *testing.T) {
			f := build(t)
			for _, b := range f.r.Blobs {
				p, err := f.store.Path(b.Digest)
				require.NoError(t, err)
				whole, err := os.ReadFile(p)
				require.NoError(t, err)
				if len(whole) < 2 {
					continue
				}
				require.NoError(t, os.Chmod(p, 0o644))
				require.NoError(t, os.WriteFile(p, how(whole), 0o644))

				out := filepath.Join(t.TempDir(), "disk.img")
				err = Assemble(context.Background(), f.r, f.store, f.mkfs, out)
				require.ErrorContains(t, err, "does not match", "blob %s", b.Digest)
				assert.NoFileExists(t, out)

				require.NoError(t, os.WriteFile(p, whole, 0o644))
			}
			require.NoError(t, Assemble(context.Background(), f.r, f.store, f.mkfs, filepath.Join(t.TempDir(), "disk.img")))
		})
	}
}

// The recipe itself damaged in the store: Load refuses it, whatever the
// damage, instead of assembling from a description that is not the image's.
func TestFaults_LoadOfADamagedRecipe(t *testing.T) {
	f := build(t)
	p, err := f.store.Path(f.m.Recipe)
	require.NoError(t, err)
	whole, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(p, 0o644))
	for cut := 0; cut < len(whole); cut += max(1, len(whole)/97) {
		require.NoError(t, os.WriteFile(p, whole[:cut], 0o644))
		_, err := Load(f.store, f.m.Recipe)
		require.Error(t, err, "a recipe cut at byte %d loaded", cut)
	}
	for i := 0; i < len(whole); i += max(1, len(whole)/211) {
		damaged := bytes.Clone(whole)
		damaged[i] ^= 0x02
		require.NoError(t, os.WriteFile(p, damaged, 0o644))
		_, err := Load(f.store, f.m.Recipe)
		require.Error(t, err, "a recipe with byte %d changed loaded", i)
	}
	require.NoError(t, os.Remove(p))
	_, err = Load(f.store, f.m.Recipe)
	require.ErrorIs(t, err, cas.ErrNotFound)
	_, err = Load(f.store, "sha256:../../x")
	require.Error(t, err)
}

// A recipe that is valid JSON and well formed, and still not a recipe a
// client may assemble: each field a hostile registry could set.
func TestFaults_ParseRefusesHostileRecipes(t *testing.T) {
	f := build(t)
	encode := func(change func(r *Recipe)) []byte {
		c := *f.r
		c.Blobs = append([]Blob(nil), f.r.Blobs...)
		change(&c)
		data, err := json.Marshal(&c)
		require.NoError(t, err)
		return data
	}
	whole := encode(func(*Recipe) {})
	_, err := Parse(whole)
	require.NoError(t, err)

	cases := map[string][]byte{
		"empty":                   nil,
		"null":                    []byte("null"),
		"an array":                []byte("[]"),
		"empty object":            []byte("{}"),
		"truncated":               whole[:len(whole)/2],
		"two recipes":             append(bytes.Clone(whole), whole...),
		"trailing newline":        append(bytes.Clone(whole), '\n'),
		"unknown field":           []byte(strings.Replace(string(whole), `{"schemaVersion":1,`, `{"schemaVersion":1,"postinstall":"curl|sh",`, 1)),
		"duplicate field":         []byte(strings.Replace(string(whole), `{"schemaVersion":1,`, `{"schemaVersion":1,"schemaVersion":1,`, 1)),
		"future schema":           encode(func(r *Recipe) { r.SchemaVersion = 2 }),
		"another media type":      encode(func(r *Recipe) { r.MediaType = "application/vnd.oci.image.manifest.v1+json" }),
		"unknown platform":        encode(func(r *Recipe) { r.Platform, r.Architecture = "windows/amd64", "amd64" }),
		"platform as a path":      encode(func(r *Recipe) { r.Platform = "linux/../../etc" }),
		"unknown layout":          encode(func(r *Recipe) { r.Layout = "raw" }),
		"toolset as a path":       encode(func(r *Recipe) { r.Toolset.Version = "v0.2.3/../../../bin" }),
		"toolset without v":       encode(func(r *Recipe) { r.Toolset.Version = "0.2.3" }),
		"no disk digest":          encode(func(r *Recipe) { r.DiskDigest = "" }),
		"disk digest as a path":   encode(func(r *Recipe) { r.DiskDigest = "sha256:../../../../etc/shadow" }),
		"zero disk size":          encode(func(r *Recipe) { r.DiskSize = 0 }),
		"negative disk size":      encode(func(r *Recipe) { r.DiskSize = -1 }),
		"negative blob size":      encode(func(r *Recipe) { r.Blobs[0].Size = -1 }),
		"blob digest as a path":   encode(func(r *Recipe) { r.Blobs[0].Digest = "sha256:../../../../etc/shadow" }),
		"blob listed twice":       encode(func(r *Recipe) { r.Blobs = append(r.Blobs[:1], r.Blobs...) }),
		"no blobs":                encode(func(r *Recipe) { r.Blobs = nil }),
		"boot not a blob":         encode(func(r *Recipe) { r.Toolset.Boot = "/boot/vmlinuz" }),
		"kernel not listed":       encode(func(r *Recipe) { r.Toolset.Kernel = "sha256:" + strings.Repeat("e", 64) }),
		"manifest reads the host": encode(func(r *Recipe) { r.Manifest = `(children:(passwd:(contents:(host:"/etc/passwd"))))` }),
		"manifest unbalanced":     encode(func(r *Recipe) { r.Manifest = r.Manifest[:len(r.Manifest)-1] }),
		"manifest empty":          encode(func(r *Recipe) { r.Manifest = "" }),
		"manifest with a NUL":     encode(func(r *Recipe) { r.Manifest = strings.Replace(r.Manifest, "(", "(\x00", 1) }),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := Parse(data)
			require.Error(t, err)
			assert.Nil(t, r)
		})
	}
}

// Recording a build whose inputs are gone or unreadable stores no recipe.
func TestFaults_RecordWithMissingInputs(t *testing.T) {
	dir := t.TempDir()
	store, err := cas.New(filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	present := filepath.Join(dir, "present")
	require.NoError(t, os.WriteFile(present, []byte("x"), 0o644))
	absent := filepath.Join(dir, "absent")
	rec := Recorder{Store: store}
	ctx := context.Background()

	_, err = rec.Record(ctx, image.BuildRecord{})
	require.ErrorContains(t, err, "no toolset")
	for name, ts := range map[string]image.Toolset{
		"bad version":    {Version: "latest", Boot: present, Kernel: present},
		"boot missing":   {Version: "0.2.3", Boot: absent, Kernel: present},
		"kernel missing": {Version: "0.2.3", Boot: present, Kernel: absent},
	} {
		_, err := rec.Record(ctx, image.BuildRecord{Config: image.BuildConfig{Toolset: &ts}})
		require.Error(t, err, name)
	}
	entries, err := os.ReadDir(store.Root())
	require.NoError(t, err)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(store.Root(), e.Name()))
		require.NoError(t, err)
		assert.NotContains(t, string(data), "schemaVersion", "a recipe was stored for a build that could not be recorded")
	}
}

func FuzzParse(f *testing.F) {
	const blobA, blobB = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	valid := &Recipe{
		SchemaVersion: SchemaVersion, MediaType: MediaType, Platform: "linux/amd64", Architecture: "amd64",
		Toolset:    Toolset{Version: "v0.2.3", Boot: blobA, Kernel: blobB},
		Manifest:   `(children:(program:(contents:(host:"blob:` + blobA + `"))))`,
		Blobs:      []Blob{{Digest: blobA, Size: 4}, {Digest: blobB, Size: 6}},
		DiskDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", DiskSize: 4096,
	}
	seed, err := valid.Encode()
	require.NoError(f, err)
	_, err = Parse(seed)
	require.NoError(f, err, "the seed recipe must be valid")
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add([]byte(strings.Replace(string(seed), "blob:", "/etc/", 1)))
	f.Add([]byte(strings.Replace(string(seed), `"diskSize":4096`, `"diskSize":-1`, 1)))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := Parse(data)
		if err != nil {
			require.Nil(t, r)
			return
		}
		// What parses is canonical (its digest is the digest of the bytes
		// read), valid, and reads no file but its own blobs.
		again, err := r.Encode()
		require.NoError(t, err)
		require.Equal(t, data, again)
		require.NoError(t, r.Validate())
		hosts, err := image.ManifestHosts(r.Manifest)
		require.NoError(t, err)
		listed := map[string]bool{}
		for _, b := range r.Blobs {
			_, ok := cas.ParseDigest(b.Digest)
			require.True(t, ok)
			require.GreaterOrEqual(t, b.Size, int64(0))
			listed[b.Digest] = true
		}
		for _, h := range hosts {
			d, ok := strings.CutPrefix(h, blobScheme)
			require.True(t, ok, "the manifest names host path %q", h)
			require.True(t, listed[d], "the manifest reads a blob the recipe does not list")
		}
		require.True(t, listed[r.Toolset.Boot])
		require.True(t, listed[r.Toolset.Kernel])
	})
}

// mkfs assembles a recipe someone else published: it gets the store root it
// is confined to and none of the daemon's credentials.
func TestAssembleGivesMkfsNoCredentials(t *testing.T) {
	for _, name := range []string{"JERBOA_AUTH_TOKEN", "JERBOA_CLUSTER_TOKEN", "JERBOA_OBSERVABILITY_TOKEN", "JERBOA_REGISTRY_TOKEN"} {
		t.Setenv(name, "s3cr3t")
	}
	f := build(t)
	envFile := filepath.Join(t.TempDir(), "env")
	out := filepath.Join(t.TempDir(), "disk.img")
	require.Error(t, Assemble(context.Background(), f.r, f.store, scriptMkfs(t, "cat > /dev/null; env > '"+envFile+"'"), out))
	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "s3cr3t")
	assert.Contains(t, string(data), HostRootEnv+"="+f.store.Root())
}
