//go:build registry_e2e

// Package registry runs the Go registry client against the real Worker
// (registry/worker) under wrangler dev, with local R2 and D1 and a fake
// GitHub, so both implementations of docs/registry-protocol.md are checked
// against each other.
//
//	(cd registry/worker && pnpm install)
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
	"runtime"
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
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "registry", "worker")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "wrangler")); err != nil {
		t.Skip("run pnpm install in registry/worker first")
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
	gh := fakeGitHub(t)
	host := startWorker(t, gh.URL)
	cl := client.New(host)
	mkfs := filepath.Join(t.TempDir(), "mkfs")
	require.NoError(t, os.WriteFile(mkfs, []byte(fakeMkfs), 0o755))
	mkfsFor := func(context.Context, string) (string, error) { return mkfs, nil }

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

	// CI uses a personal access token.
	_, pat, err := cl.CreatePAT(ctx, refresh, "ci")
	require.NoError(t, err)

	ref := func(s string) naming.ImageReference {
		r, err := naming.ParseImageReference(host + "/" + s)
		require.NoError(t, err)
		return r
	}
	local := func() *cas.Store {
		s, err := cas.New(t.TempDir())
		require.NoError(t, err)
		return s
	}

	// Push two versions to an organization: the second sends one file.
	builder := local()
	v1 := buildApp(t, mkfs, builder, "console.log('v1')")
	v2 := buildApp(t, mkfs, builder, "console.log('v2')")
	token, err := cl.PushToken(ctx, pat, "acme", "web")
	require.NoError(t, err)
	res, err := transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("acme/web:v1"), Store: builder, Recipe: v1, Mkfs: mkfsFor})
	require.NoError(t, err)
	require.Equal(t, res.Blobs, res.BlobsUploaded)
	res, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("acme/web:v2"), Store: builder, Recipe: v2, Mkfs: mkfsFor})
	require.NoError(t, err)
	require.Equal(t, 1, res.BlobsUploaded)

	// Another machine pulls both, anonymously, through the CDN layout.
	machine := local()
	out := filepath.Join(t.TempDir(), "disk.img")
	got, err := transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("acme/web:v1"), Store: machine, Mkfs: mkfsFor, Out: out})
	require.NoError(t, err)
	require.Equal(t, v1, got.Recipe)
	require.Equal(t, got.Blobs, got.BlobsFetched)
	got, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("acme/web:v2"), Store: machine, Mkfs: mkfsFor, Out: out})
	require.NoError(t, err)
	require.Equal(t, 1, got.BlobsFetched)

	// Namespaces are enforced.
	_, err = cl.PushToken(ctx, refresh, "bob", "web")
	require.True(t, client.IsCode(err, "DENIED"), "got %v", err)
	other, err := cl.PushToken(ctx, refresh, "alice", "other")
	require.NoError(t, err)
	_, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: other, Ref: ref("acme/web:v3"), Store: builder, Recipe: v1, Mkfs: mkfsFor})
	require.True(t, client.IsCode(err, "DENIED"), "got %v", err)
}
