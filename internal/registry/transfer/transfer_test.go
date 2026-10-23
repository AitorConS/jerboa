package transfer

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/naming"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/pack"
	"github.com/AitorConS/jerboa/internal/registry/recipe"
	"github.com/AitorConS/jerboa/internal/registry/registrytest"
	"github.com/stretchr/testify/require"
)

// fakeMkfs writes a "disk" that depends only on what a real mkfs consumes.
const fakeMkfs = `#!/bin/sh
for last; do :; done
manifest=$(cat)
printf '%s' "$manifest" | sed 's/host:"[^"]*"/host:X/g' > "$last"
cat "$2" "$4" >> "$last"
printf '%s' "$manifest" | grep -o 'host:"[^"]*"' | sed 's/^host:"//; s/"$//' | while IFS= read -r p; do cat "$p" >> "$last"; done
`

type env struct {
	dir  string
	mkfs string
	reg  *registrytest.Server
	cl   *client.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	mkfs := filepath.Join(dir, "mkfs")
	require.NoError(t, os.WriteFile(mkfs, []byte(fakeMkfs), 0o755))
	reg := registrytest.New()
	t.Cleanup(reg.Close)
	return &env{dir: dir, mkfs: mkfs, reg: reg, cl: client.New(reg.Host())}
}

func (e *env) mkfsFor(context.Context, string) (string, error) { return e.mkfs, nil }

func staticELF() []byte {
	b := make([]byte, 512)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(b[18:], uint16(elf.EM_X86_64))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}

// build builds an image whose lib file holds lib, recording its recipe in store.
func (e *env) build(t *testing.T, store *cas.Store, lib string) string {
	t.Helper()
	in := filepath.Join(e.dir, "in-"+strings.ReplaceAll(lib, " ", "_"))
	write := func(name string, data []byte) string {
		p := filepath.Join(in, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, data, 0o755))
		return p
	}
	big := make([]byte, 3<<20)
	r := rand.New(rand.NewPCG(7, 7))
	for i := range big {
		big[i] = byte(r.Uint32())
	}
	boot, kernel := write("boot.img", []byte("BOOT")), write("kernel.img", []byte("KERNEL"))
	img, err := image.NewStore(filepath.Join(e.dir, "images"))
	require.NoError(t, err)
	m, err := image.NewBuilder(img).WithRecorder(recipe.Recorder{Store: store}).Build(context.Background(), image.BuildConfig{
		Name: "web", Tag: "local", Platform: "linux/amd64", Layout: image.LayoutStandard,
		BinaryPath: write("program", staticELF()),
		PkgFiles: []pkg.File{
			{HostPath: write("lib.js", []byte(lib)), GuestPath: "app/lib.js"},
			{HostPath: write("runtime", big), GuestPath: "usr/bin/runtime"},
		},
		MkfsRun: func(ctx context.Context, out, _ string, manifest string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, e.mkfs, "-b", boot, "-k", kernel, out)
			cmd.Stdin = strings.NewReader(manifest)
			return cmd
		},
		Toolset: &image.Toolset{Version: "0.2.3", Boot: boot, Kernel: kernel},
	})
	require.NoError(t, err)
	require.NotEmpty(t, m.Recipe)
	return m.Recipe
}

func (e *env) ref(t *testing.T, tag string) naming.ImageReference {
	t.Helper()
	r, err := naming.ParseImageReference(e.reg.Host() + "/alice/web:" + tag)
	require.NoError(t, err)
	return r
}

func newStore(t *testing.T) *cas.Store {
	t.Helper()
	s, err := cas.New(t.TempDir())
	require.NoError(t, err)
	return s
}

func TestPushPullRoundTripAndDeduplication(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	local := newStore(t)

	v1 := e.build(t, local, "version 1")
	res, err := Push(ctx, PushOptions{Client: e.cl, Token: "t", Ref: e.ref(t, "v1"), Store: local, Recipe: v1, Mkfs: e.mkfsFor})
	require.NoError(t, err)
	require.Equal(t, res.Blobs, res.BlobsUploaded)

	// A second app version only uploads the file that changed.
	v2 := e.build(t, local, "version 2")
	res, err = Push(ctx, PushOptions{Client: e.cl, Token: "t", Ref: e.ref(t, "v2"), Store: local, Recipe: v2, Mkfs: e.mkfsFor})
	require.NoError(t, err)
	require.Equal(t, 1, res.BlobsUploaded)

	// A machine with an empty store pulls v1, then v2 fetches one blob.
	remote := newStore(t)
	out := filepath.Join(t.TempDir(), "disk.img")
	pulled, err := Pull(ctx, PullOptions{Client: e.cl, Ref: e.ref(t, "v1"), Store: remote, Mkfs: e.mkfsFor, Out: out})
	require.NoError(t, err)
	require.Equal(t, v1, pulled.Recipe)
	require.Equal(t, pulled.Blobs, pulled.BlobsFetched)
	require.LessOrEqual(t, pulled.Requests, 2)
	pulled, err = Pull(ctx, PullOptions{Client: e.cl, Ref: e.ref(t, "v2"), Store: remote, Mkfs: e.mkfsFor, Out: out})
	require.NoError(t, err)
	require.Equal(t, 1, pulled.BlobsFetched)

	// Pulling by digest skips the tag.
	byDigest := e.ref(t, "v1")
	byDigest.Digest = v1
	_, err = Pull(ctx, PullOptions{Client: e.cl, Ref: byDigest, Store: remote, Mkfs: e.mkfsFor, Out: out})
	require.NoError(t, err)

	// Every successful pull reports itself to the hub's counter.
	require.Equal(t, 3, e.reg.Pulls(byDigest.Namespace, byDigest.Repository))
}

func TestPullRejectsCorruptPack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	local := newStore(t)
	v1 := e.build(t, local, "version 1")
	_, err := Push(ctx, PushOptions{Client: e.cl, Token: "t", Ref: e.ref(t, "v1"), Store: local, Recipe: v1, Mkfs: e.mkfsFor})
	require.NoError(t, err)
	for _, key := range e.reg.Packs() {
		e.reg.Corrupt(key, 40)
	}
	remote := newStore(t)
	_, err = Pull(ctx, PullOptions{Client: e.cl, Ref: e.ref(t, "v1"), Store: remote, Mkfs: e.mkfsFor, Out: filepath.Join(t.TempDir(), "d")})
	require.Error(t, err)
	r, err := recipe.Load(remote, v1)
	require.NoError(t, err)
	stored := 0
	for _, b := range r.Blobs {
		if remote.Has(b.Digest) {
			stored++
		}
	}
	require.Less(t, stored, len(r.Blobs), "corrupt blobs must not be stored")
	require.Zero(t, e.reg.Pulls("alice", "web"), "a failed pull is not counted")
}

func TestPushRefusesRecipeThatDoesNotRebuild(t *testing.T) {
	e := newEnv(t)
	local := newStore(t)
	v1 := e.build(t, local, "version 1")
	r, err := recipe.Load(local, v1)
	require.NoError(t, err)
	p, _ := local.Path(r.Toolset.Kernel)
	require.NoError(t, os.Chmod(p, 0o644))
	require.NoError(t, os.WriteFile(p, []byte("KERNEX"), 0o644))
	_, err = Push(context.Background(), PushOptions{Client: e.cl, Token: "t", Ref: e.ref(t, "v1"), Store: local, Recipe: v1, Mkfs: e.mkfsFor})
	require.ErrorContains(t, err, "does not rebuild")
	require.Empty(t, e.reg.Packs())
}

func TestPullRejectsHostileLocations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	local := newStore(t)
	v1 := e.build(t, local, "version 1")
	_, err := Push(ctx, PushOptions{Client: e.cl, Token: "t", Ref: e.ref(t, "v1"), Store: local, Recipe: v1, Mkfs: e.mkfsFor})
	require.NoError(t, err)
	key := "locations/" + strings.TrimPrefix(v1, "sha256:") + ".json"
	data, ok := e.reg.Object(key)
	require.True(t, ok)
	var locs struct {
		Blobs map[string]pack.Location `json:"blobs"`
	}
	require.NoError(t, json.Unmarshal(data, &locs))

	for name, mutate := range map[string]func(l *pack.Location){
		"negative length": func(l *pack.Location) { l.Length = -1000 },
		"negative offset": func(l *pack.Location) { l.Offset = -1 },
		"huge length":     func(l *pack.Location) { l.Length = 1 << 40 },
		"bad pack":        func(l *pack.Location) { l.Pack = "../x" },
	} {
		t.Run(name, func(t *testing.T) {
			hostile := map[string]pack.Location{}
			for d, l := range locs.Blobs {
				mutate(&l)
				hostile[d] = l
			}
			b, err := json.Marshal(map[string]any{"blobs": hostile})
			require.NoError(t, err)
			e.reg.SetObject(key, b)
			_, err = Pull(ctx, PullOptions{Client: e.cl, Ref: e.ref(t, "v1"), Store: newStore(t), Mkfs: e.mkfsFor, Out: filepath.Join(t.TempDir(), "d")})
			require.ErrorContains(t, err, "pack: ")
		})
	}
}

func TestPushUploadsOnlyTheRecipesBlobs(t *testing.T) {
	e := newEnv(t)
	local := newStore(t)
	v1 := e.build(t, local, "version 1")
	other, err := local.PutBytes([]byte("a blob of another image"))
	require.NoError(t, err)

	uploaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/blobs/missing"):
			_ = json.NewEncoder(w).Encode(map[string][]string{"missing": {other}})
		case strings.Contains(r.URL.Path, "/packs/"):
			uploaded = true
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()
	ref, err := naming.ParseImageReference(strings.TrimPrefix(srv.URL, "http://") + "/alice/web:v1")
	require.NoError(t, err)
	_, err = Push(context.Background(), PushOptions{Client: client.New(ref.Registry), Token: "t", Ref: ref, Store: local, Recipe: v1, Mkfs: e.mkfsFor})
	require.ErrorContains(t, err, "does not use")
	require.False(t, uploaded)
}
