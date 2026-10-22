//go:build registry_e2e

// Package registry runs the Go registry client against the real Worker, so
// both implementations of docs/registry-protocol.md are checked against each
// other. The Worker lives in the private AitorConS/jerboa-registry repository,
// whose CI runs these tests.
//
// Locally it starts the Worker under wrangler dev, with local R2 and D1 and a
// fake GitHub, from a checkout of that repository:
//
//	(cd <worker> && pnpm install)
//	JERBOA_REGISTRY_WORKER=<worker> go test -tags registry_e2e ./tests/registry/...
//
// Against a deployed registry (staging), it uses a test account's personal
// access token instead of the device flow, pushes to a repository unique to
// the run, checks the CDN caches packs, and deletes its tags afterwards:
//
//	JERBOA_REGISTRY_E2E_HOST=staging.registry.jerboa.dev \
//	JERBOA_REGISTRY_E2E_TOKEN=jpat_... JERBOA_REGISTRY_E2E_NAMESPACE=jerboa-e2e \
//	go test -tags registry_e2e ./tests/registry/...
package registry

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/naming"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/recipe"
	"github.com/AitorConS/jerboa/internal/registry/transfer"
	"github.com/stretchr/testify/require"
)

// fakeGitHub serves the device flow and the two REST calls the Worker makes.
// The device flow always approves alice, who belongs to the acme organization.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	users := map[string]struct {
		id    int
		login string
		orgs  []string
	}{
		"gh-alice": {1, "alice", []string{"acme"}},
		"gh-bob":   {2, "bob", nil},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dev", "user_code": "E2E-CODE",
				"verification_uri": "https://github.test/login/device", "expires_in": 60, "interval": 1})
			return
		case "/login/oauth/access_token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gh-alice"})
			return
		}
		u, ok := users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": u.id, "login": u.login})
		case "/user/orgs":
			orgs := []map[string]string{}
			for _, o := range u.orgs {
				orgs = append(orgs, map[string]string{"login": o})
			}
			_ = json.NewEncoder(w).Encode(orgs)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startWorker runs the Worker with fresh local state and returns its host.
func startWorker(t *testing.T, github string) string {
	t.Helper()
	dir := os.Getenv("JERBOA_REGISTRY_WORKER")
	if dir == "" {
		t.Skip("set JERBOA_REGISTRY_WORKER to a checkout of the registry Worker")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "wrangler")); err != nil {
		t.Skipf("run pnpm install in %s first", dir)
	}
	state := t.TempDir()
	migrate := exec.Command("pnpm", "exec", "wrangler", "d1", "migrations", "apply", "jerboa-registry-local", "--local", "--persist-to", state)
	migrate.Dir = dir
	out, err := migrate.CombinedOutput()
	require.NoError(t, err, string(out))

	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx, cancel := context.WithCancel(context.Background())
	dev := exec.CommandContext(ctx, "pnpm", "exec", "wrangler", "dev", "--local", "--ip", "127.0.0.1",
		"--port", fmt.Sprint(port), "--persist-to", state, "--show-interactive-dev-session=false",
		"--var", "GITHUB_API:"+github,
		"--var", "CDN_BASE:"+base+"/cdn",
		"--var", "JWT_SIGNING_KEY:e2e-signing-key-that-is-long-enough-0123456789")
	dev.Dir = dir
	// pnpm starts wrangler, which starts workerd: stop the whole process
	// group, or the grandchildren keep the output pipe open for ever.
	dev.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dev.Cancel = func() error { return syscall.Kill(-dev.Process.Pid, syscall.SIGTERM) }
	dev.WaitDelay = 10 * time.Second
	var logs bytes.Buffer
	dev.Stdout, dev.Stderr = &logs, &logs
	require.NoError(t, dev.Start())
	t.Cleanup(func() {
		cancel()
		_ = dev.Wait()
		if t.Failed() {
			t.Logf("wrangler dev output:\n%s", logs.String())
		}
	})
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/v2/")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 90*time.Second, 250*time.Millisecond, "wrangler dev did not start")
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// fakeMkfs writes a "disk" that depends only on what a real mkfs consumes, so
// the test needs no kernel toolchain.
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

// buildApp builds an image of a runtime (2 MiB, shared between versions) and
// an app file, recording its recipe in store.
func buildApp(t *testing.T, mkfs string, store *cas.Store, app string) string {
	t.Helper()
	in := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(in, name)
		require.NoError(t, os.WriteFile(p, data, 0o755))
		return p
	}
	runtimeBytes := make([]byte, 2<<20)
	r := rand.New(rand.NewPCG(1, 1))
	for i := range runtimeBytes {
		runtimeBytes[i] = byte(r.Uint32())
	}
	boot, kernel := write("boot.img", []byte("BOOT")), write("kernel.img", []byte("KERNEL"))
	img, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	m, err := image.NewBuilder(img).WithRecorder(recipe.Recorder{Store: store}).Build(context.Background(), image.BuildConfig{
		Name: "web", Tag: "build", Platform: "linux/amd64", Layout: image.LayoutStandard,
		BinaryPath: write("program", staticELF()),
		PkgFiles: []pkg.File{
			{HostPath: write("runtime", runtimeBytes), GuestPath: "usr/bin/runtime"},
			{HostPath: write("server.js", []byte(app)), GuestPath: "app/server.js"},
		},
		MkfsRun: func(ctx context.Context, out, _ string, manifest string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, mkfs, "-b", boot, "-k", kernel, out)
			cmd.Stdin = strings.NewReader(manifest)
			return cmd
		},
		Toolset: &image.Toolset{Version: "0.2.3", Boot: boot, Kernel: kernel},
	})
	require.NoError(t, err)
	return m.Recipe
}

func TestWorkerEndToEnd(t *testing.T) {
	ctx := context.Background()
	if host := os.Getenv("JERBOA_REGISTRY_E2E_HOST"); host != "" {
		token, ns := os.Getenv("JERBOA_REGISTRY_E2E_TOKEN"), os.Getenv("JERBOA_REGISTRY_E2E_NAMESPACE")
		require.NotEmpty(t, token, "JERBOA_REGISTRY_E2E_TOKEN")
		require.NotEmpty(t, ns, "JERBOA_REGISTRY_E2E_NAMESPACE")
		repo := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
		runFlow(t, ctx, client.New(host), token, ns, repo, true)
		return
	}
	gh := fakeGitHub(t)
	host := startWorker(t, gh.URL)
	cl := client.New(host)

	// jerboa login: device flow, then the registry's refresh token.
	d, err := cl.Discover(ctx)
	require.NoError(t, err)
	flow := client.DeviceFlow{GitHub: gh.URL}
	code, err := flow.Start(ctx, d.GitHubClientID)
	require.NoError(t, err)
	ghToken, err := flow.Wait(ctx, d.GitHubClientID, code)
	require.NoError(t, err)
	refresh, login, err := cl.LoginWithGitHub(ctx, ghToken)
	require.NoError(t, err)
	require.Equal(t, "alice", login)

	// CI uses a personal access token; alice may push for her organization.
	patID, pat, err := cl.CreatePAT(ctx, refresh, "ci")
	require.NoError(t, err)
	pats, err := cl.ListPATs(ctx, refresh)
	require.NoError(t, err)
	require.Len(t, pats, 1)
	require.Equal(t, "ci", pats[0].Name)
	runFlow(t, ctx, cl, pat, "acme", "web", false)
	require.NoError(t, cl.RevokePAT(ctx, refresh, patID))
	_, err = cl.PushToken(ctx, pat, "acme", "web")
	require.True(t, client.IsCode(err, "UNAUTHORIZED"), "got %v", err)

	other, err := cl.PushToken(ctx, refresh, "alice", "other")
	require.NoError(t, err)
	mkfs, store := fakeMkfsPath(t), newStore(t)
	v := buildApp(t, mkfs, store, "x")
	_, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: other, Ref: refOf(t, cl, "acme/web:v3"), Store: store, Recipe: v,
		Mkfs: func(context.Context, string) (string, error) { return mkfs, nil }})
	require.True(t, client.IsCode(err, "DENIED"), "got %v", err)
}

func fakeMkfsPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mkfs")
	require.NoError(t, os.WriteFile(p, []byte(fakeMkfs), 0o755))
	return p
}

func newStore(t *testing.T) *cas.Store {
	t.Helper()
	s, err := cas.New(t.TempDir())
	require.NoError(t, err)
	return s
}

func refOf(t *testing.T, cl *client.Client, s string) naming.ImageReference {
	t.Helper()
	r, err := naming.ParseImageReference(strings.TrimPrefix(strings.TrimPrefix(cl.Base, "https://"), "http://") + "/" + s)
	require.NoError(t, err)
	return r
}

// runFlow pushes two versions of an app to ns/repo with credential, pulls
// both into an empty store, and checks another user's namespace is refused.
func runFlow(t *testing.T, ctx context.Context, cl *client.Client, credential, ns, repo string, remote bool) {
	t.Helper()
	mkfs := fakeMkfsPath(t)
	mkfsFor := func(context.Context, string) (string, error) { return mkfs, nil }
	builder := newStore(t)
	// The repository name makes the app unique to the run, so v2 has one new
	// blob even on a shared registry that holds earlier runs' content.
	v1 := buildApp(t, mkfs, builder, fmt.Sprintf("console.log('v1 %s')", repo))
	v2 := buildApp(t, mkfs, builder, fmt.Sprintf("console.log('v2 %s')", repo))
	token, err := cl.PushToken(ctx, credential, ns, repo)
	require.NoError(t, err)
	if remote {
		t.Cleanup(func() {
			// The repository too, not only its tags, so runs leave nothing behind.
			if err := cl.DeleteRepo(context.Background(), token, ns, repo); err != nil {
				for _, tag := range []string{"v1", "v2"} {
					_ = cl.DeleteTag(context.Background(), token, ns, repo, tag)
				}
			}
		})
	}
	ref := func(tag string) naming.ImageReference { return refOf(t, cl, ns+"/"+repo+":"+tag) }

	res, err := transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("v1"), Store: builder, Recipe: v1, Mkfs: mkfsFor})
	require.NoError(t, err)
	if !remote {
		// A shared registry may already hold the runtime from an earlier run.
		require.Equal(t, res.Blobs, res.BlobsUploaded)
	}
	res, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("v2"), Store: builder, Recipe: v2, Mkfs: mkfsFor})
	require.NoError(t, err)
	require.Equal(t, 1, res.BlobsUploaded)

	// Another machine pulls both, anonymously, through the CDN layout.
	machine := newStore(t)
	out := filepath.Join(t.TempDir(), "disk.img")
	got, err := transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("v1"), Store: machine, Mkfs: mkfsFor, Out: out})
	require.NoError(t, err)
	require.Equal(t, v1, got.Recipe)
	require.Equal(t, got.Blobs, got.BlobsFetched)
	got, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("v2"), Store: machine, Mkfs: mkfsFor, Out: out})
	require.NoError(t, err)
	require.Equal(t, 1, got.BlobsFetched)

	_, err = cl.PushToken(ctx, credential, "jerboa-e2e-forbidden", repo)
	require.True(t, client.IsCode(err, "DENIED"), "got %v", err)

	if remote {
		requireCDNCaches(t, ctx, cl, v1)
	}
}

// requireCDNCaches checks that the CDN in front of R2 serves an immutable
// recipe from its cache, which is what keeps pulls off R2 operations.
func requireCDNCaches(t *testing.T, ctx context.Context, cl *client.Client, recipeDigest string) {
	t.Helper()
	d, err := cl.Discover(ctx)
	require.NoError(t, err)
	url := d.CDN + "/recipes/" + strings.TrimPrefix(recipeDigest, "sha256:") + ".json"
	var status, cacheControl string
	require.Eventually(t, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		status, cacheControl = resp.Header.Get("cf-cache-status"), resp.Header.Get("Cache-Control")
		return status == "HIT"
	}, 30*time.Second, 2*time.Second, "CDN does not cache %s (cf-cache-status %q, Cache-Control %q)", url, status, cacheControl)
	require.Contains(t, cacheControl, "immutable")
}
