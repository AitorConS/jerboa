package recipe

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"testing"

	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/registry/pack"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// TestWriteWorkerFixture writes the pack upload, recipe and hostile frame the
// registry Worker's tests decode, so both sides of the protocol are checked
// against the same bytes:
//
//	JERBOA_WORKER_FIXTURE=<worker>/test/fixtures/go.json go test ./internal/registry/recipe -run WorkerFixture
//
// where <worker> is a checkout of the registry Worker (AitorConS/jerboa-registry).
func TestWriteWorkerFixture(t *testing.T) {
	out := os.Getenv("JERBOA_WORKER_FIXTURE")
	if out == "" {
		t.Skip("set JERBOA_WORKER_FIXTURE to regenerate the Worker test fixture")
	}
	contents := map[string][]byte{
		"program": []byte("\x7fELF program bytes"),
		"lib":     bytes.Repeat([]byte("library text "), 8000),
		"empty":   {},
		"boot":    []byte("boot image"),
		"kernel":  []byte("kernel image"),
	}
	w, err := pack.NewWriter(zstd.SpeedDefault)
	require.NoError(t, err)
	digests := map[string]string{}
	for _, name := range []string{"program", "lib", "empty", "boot", "kernel"} {
		data := contents[name]
		digests[name] = digestOf(data)
		require.NoError(t, w.Add(pack.Blob{Digest: digests[name], Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}))
	}
	packs := w.Close()
	require.Len(t, packs, 1)
	upload, err := pack.EncodeUpload(packs[0])
	require.NoError(t, err)

	r := &Recipe{
		SchemaVersion: SchemaVersion, MediaType: MediaType,
		Platform: "linux/amd64", Architecture: "amd64", Layout: image.LayoutCompact,
		Config:  image.Config{Memory: "256M", CPUs: 1},
		Toolset: Toolset{Version: "v0.2.3", Boot: digests["boot"], Kernel: digests["kernel"]},
		Manifest: fmt.Sprintf("(\n    children:(\n        lib:(contents:(host:\"blob:%s\"))\n        empty:(contents:(host:\"blob:%s\"))\n        program:(contents:(host:\"blob:%s\"))\n    )\n    program:/program\n)",
			digests["lib"], digests["empty"], digests["program"]),
		DiskDigest: digestOf([]byte("disk")), DiskSize: 4,
	}
	for _, name := range []string{"program", "lib", "empty", "boot", "kernel"} {
		r.Blobs = append(r.Blobs, Blob{Digest: digests[name], Size: int64(len(contents[name]))})
	}
	sort.Slice(r.Blobs, func(i, j int) bool { return r.Blobs[i].Digest < r.Blobs[j].Digest })
	require.NoError(t, r.Validate())
	recipeBytes, err := r.Encode()
	require.NoError(t, err)
	recipeDigest, err := r.Digest()
	require.NoError(t, err)

	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	bomb := enc.EncodeAll(make([]byte, 8<<20), nil)

	doc := map[string]any{
		"upload":       base64.StdEncoding.EncodeToString(upload),
		"pack":         packs[0].Digest,
		"entries":      packs[0].Entries,
		"recipe":       base64.StdEncoding.EncodeToString(recipeBytes),
		"recipeDigest": recipeDigest,
		"bombFrame":    base64.StdEncoding.EncodeToString(bomb),
		"bombDigest":   digestOf([]byte("small")),
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(out, append(data, '\n'), 0o644))
}
