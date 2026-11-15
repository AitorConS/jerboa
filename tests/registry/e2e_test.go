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
// Against a deployed registry (staging, and production every hour from
// .github/workflows/prod-e2e.yml), it uses a test account's personal access
// token instead of the device flow, or in GitHub Actions without one, the
// job's OIDC token, as a trusted publisher of the namespace. It pushes to a
// repository unique to the run, tags a recipe many times at once, checks the
// CDN caches packs, and deletes the repository afterwards:
//
//	JERBOA_REGISTRY_E2E_HOST=staging.registry.jerboa.dev \
//	JERBOA_REGISTRY_E2E_TOKEN=jpat_... JERBOA_REGISTRY_E2E_NAMESPACE=jerboa-e2e \
//	go test -tags registry_e2e ./tests/registry/...
//
// Either way it logs how long each push and pull took and the requests it
// made, and checks the ones a step must not make: a push of what the registry
// holds uploads no pack, a pull of a disk already held reads no pack and is
// not counted, and a pull of a resolved recipe does not read the tag.
package registry

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/base64"
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/naming"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/AitorConS/jerboa/internal/registry/recipe"
	"github.com/AitorConS/jerboa/internal/registry/transfer"
	"github.com/stretchr/testify/require"
)

// e2eClientSecret is the OAuth client secret the Worker and the fake GitHub
// share; the Worker's local client id is "local-development".
const e2eClientSecret = "e2e-client-secret"

// fakeGitHub serves the device flow, whether the client or the Worker runs it,
// and the REST calls the Worker makes. The device flow answers pending once,
// then approves alice, an admin of the acme organization; bob is a plain
// member of it. gh-other is a token another OAuth App holds.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	type membership struct {
		id    int
		login string
		role  string
	}
	users := map[string]struct {
		id    int
		login string
		orgs  []membership
	}{
		"gh-alice": {1, "alice", []membership{{100, "acme", "admin"}}},
		"gh-bob":   {2, "bob", []membership{{100, "acme", "member"}}},
	}
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code", "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil || r.Method != http.MethodPost || r.Form.Get("client_id") != "local-development" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Path == "/login/device/code" {
				_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dev", "user_code": "E2E-CODE",
					"verification_uri": "https://github.test/login/device", "expires_in": 60, "interval": 1})
				return
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "dev" {
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "incorrect_device_code"})
				return
			}
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gh-alice"})
			return
		case "/applications/local-development/token":
			// The Worker checks a token was issued to its own OAuth App.
			id, secret, ok := r.BasicAuth()
			if !ok || id != "local-development" || secret != e2eClientSecret || r.Method != http.MethodPost {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body struct {
				AccessToken string `json:"access_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			u, ok := users[body.AccessToken]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"scopes": []string{"read:org"}, "user": map[string]any{"id": u.id, "login": u.login}})
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
		case "/user/memberships/orgs":
			out := []map[string]any{}
			if r.URL.Query().Get("page") == "1" || r.URL.Query().Get("page") == "" {
				for _, o := range u.orgs {
					out = append(out, map[string]any{"state": "active", "role": o.role, "organization": map[string]any{"id": o.id, "login": o.login}})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/user/orgs":
			orgs := []map[string]any{}
			for _, o := range u.orgs {
				orgs = append(orgs, map[string]any{"id": o.id, "login": o.login})
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
		"--var", "GITHUB_WEB:"+github,
		"--var", "CDN_BASE:"+base+"/cdn",
		"--var", "JWT_SIGNING_KEY:e2e-signing-key-that-is-long-enough-0123456789",
		"--var", "GITHUB_CLIENT_SECRET:"+e2eClientSecret,
		"--var", "GITHUB_TOKEN_KEY:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
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

func staticELF(machine elf.Machine) []byte {
	b := make([]byte, 512)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(b[18:], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}

// buildApp builds a linux/amd64 image of a runtime (2 MiB, shared between
// versions) and an app file, recording its recipe in store.
func buildApp(t *testing.T, mkfs string, store *cas.Store, app string) string {
	t.Helper()
	return buildAppFor(t, mkfs, store, app, "linux/amd64")
}

// buildAppFor builds the image for a platform.
func buildAppFor(t *testing.T, mkfs string, store *cas.Store, app, platform string) string {
	t.Helper()
	machine := elf.EM_X86_64
	if platform == "linux/arm64" {
		machine = elf.EM_AARCH64
	}
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
		Name: "web", Tag: "build", Platform: platform, Layout: image.LayoutStandard,
		BinaryPath: write("program", staticELF(machine)),
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
		if token == "" && credentials.InGitHubActions() {
			token = credentials.GitHubActions
		}
		require.NotEmpty(t, token, "JERBOA_REGISTRY_E2E_TOKEN, or permissions: id-token: write in GitHub Actions")
		require.NotEmpty(t, ns, "JERBOA_REGISTRY_E2E_NAMESPACE")
		repo := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
		runFlow(t, ctx, client.New(host), token, ns, repo, true)
		return
	}
	gh := fakeGitHub(t)
	host := startWorker(t, gh.URL)
	cl := client.New(host)

	// jerboa login: the device flow, run by the Worker if it offers to, then
	// the registry's refresh token.
	d, err := cl.Discover(ctx)
	require.NoError(t, err)
	refresh, login := loginWithGitHub(t, ctx, cl, d, gh.URL)
	require.Equal(t, "alice", login)

	// CI uses a personal access token; alice may push for her organization.
	patID, pat, err := cl.CreatePAT(ctx, refresh, client.PATOptions{Name: "ci", ExpiresInDays: 30})
	require.NoError(t, err)
	pats, err := cl.ListPATs(ctx, refresh)
	require.NoError(t, err)
	require.Len(t, pats, 1)
	require.Equal(t, "ci", pats[0].Name)
	require.NotNil(t, pats[0].ExpiresAt)
	runFlow(t, ctx, cl, pat, "acme", "web", false)

	// A personal access token cannot mint another, nor delete unless created
	// to; alice, an admin of acme, can delete with her login.
	_, _, err = cl.CreatePAT(ctx, pat, client.PATOptions{Name: "more"})
	require.True(t, client.IsStatus(err, http.StatusForbidden), "got %v", err)
	_, err = cl.DeleteToken(ctx, pat, "acme", "web")
	require.True(t, client.IsStatus(err, http.StatusForbidden), "got %v", err)
	del, err := cl.DeleteToken(ctx, refresh, "acme", "web")
	require.NoError(t, err)
	require.NoError(t, cl.DeleteTag(ctx, del, "acme", "web", "v1"))

	require.NoError(t, cl.RevokePAT(ctx, refresh, patID))
	_, err = cl.PushToken(ctx, pat, "acme", "web")
	require.True(t, client.IsCode(err, "UNAUTHORIZED"), "got %v", err)

	// A GitHub token issued to another app does not sign in; a Worker that
	// runs the flow itself takes no GitHub token at all.
	_, _, err = cl.LoginWithGitHub(ctx, "gh-other")
	want := http.StatusUnauthorized
	if d.Login == client.LoginDevice {
		want = http.StatusGone
	}
	require.True(t, client.IsStatus(err, want), "got %v", err)

	other, err := cl.PushToken(ctx, refresh, "alice", "other")
	require.NoError(t, err)
	mkfs, store := fakeMkfsPath(t), newStore(t)
	v := buildApp(t, mkfs, store, "x")
	_, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: other, Ref: refOf(t, cl, "acme/web:v3"), Store: store, Recipe: v,
		Mkfs: func(context.Context, string) (string, error) { return mkfs, nil }})
	require.True(t, client.IsCode(err, "DENIED"), "got %v", err)
}

// loginWithGitHub logs in as alice through whichever flow the Worker offers,
// and returns the refresh token and login.
func loginWithGitHub(t *testing.T, ctx context.Context, cl *client.Client, d client.Discovery, github string) (string, string) {
	t.Helper()
	if d.Login == client.LoginDevice {
		l, err := cl.StartLogin(ctx)
		require.NoError(t, err)
		require.Equal(t, "E2E-CODE", l.UserCode)
		refresh, login, err := cl.WaitLogin(ctx, l)
		require.NoError(t, err)
		return refresh, login
	}
	flow := client.DeviceFlow{GitHub: github}
	code, err := flow.Start(ctx, d.GitHubClientID)
	require.NoError(t, err)
	ghToken, err := flow.Wait(ctx, d.GitHubClientID, code)
	require.NoError(t, err)
	refresh, login, err := cl.LoginWithGitHub(ctx, ghToken)
	require.NoError(t, err)
	return refresh, login
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

// requests counts the requests a client makes, by kind.
type requests struct {
	next http.RoundTripper
	mu   sync.Mutex
	n    map[string]int
}

// record makes cl count its requests from now on.
func record(cl *client.Client) *requests {
	r := &requests{next: httpclient.Streaming.Transport, n: map[string]int{}}
	if r.next == nil {
		r.next = http.DefaultTransport
	}
	cl.HTTP = &http.Client{Transport: r, CheckRedirect: httpclient.Streaming.CheckRedirect}
	return r
}

// requestKind names what a request is for, from its method and path: the
// CDN's layout and the API's share the words that tell them apart.
func requestKind(method, path string) string {
	read := method == http.MethodGet || method == http.MethodHead
	switch {
	case path == "/v2/":
		return "discover"
	case method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
		return "count"
	case strings.HasSuffix(path, "/blobs/missing"):
		return "missing"
	case strings.Contains(path, "/packs/") && read:
		return "pack-read"
	case strings.Contains(path, "/packs/"):
		return "pack-write"
	case strings.Contains(path, "/tags/") && read:
		return "tag-read"
	case strings.Contains(path, "/tags/"):
		return "tag-write"
	case strings.Contains(path, "/refs/"):
		return "ref"
	case strings.Contains(path, "/recipes/") && read:
		return "recipe-read"
	case strings.Contains(path, "/recipes/"):
		return "recipe-write"
	case strings.Contains(path, "/locations/"):
		return "locations"
	}
	return "other"
}

func (r *requests) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.n[requestKind(req.Method, req.URL.Path)]++
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

// during runs step and returns the requests it made, logging them with the
// time it took: the figures to compare between runs of a deployed registry.
func (r *requests) during(t *testing.T, name string, step func()) map[string]int {
	t.Helper()
	r.mu.Lock()
	before := make(map[string]int, len(r.n))
	for k, v := range r.n {
		before[k] = v
	}
	r.mu.Unlock()
	start := time.Now()
	step()
	took := time.Since(start)
	r.mu.Lock()
	defer r.mu.Unlock()
	made, total := map[string]int{}, 0
	var parts []string
	for k, v := range r.n {
		if d := v - before[k]; d > 0 {
			made[k] = d
			total += d
			parts = append(parts, fmt.Sprintf("%s=%d", k, d))
		}
	}
	slices.Sort(parts)
	t.Logf("%-32s %8s  %3d requests  %s", name, took.Round(time.Millisecond), total, strings.Join(parts, " "))
	return made
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
			// The repository too, not only its tags, so runs leave nothing
			// behind. A registry without the delete action takes a push token.
			del, err := cl.DeleteToken(context.Background(), credential, ns, repo)
			if err != nil {
				del = token
			}
			if err := cl.DeleteRepo(context.Background(), del, ns, repo); err != nil {
				for _, tag := range []string{"v1", "v2", "v2-arm64", "multi", "moving"} {
					_ = cl.DeleteTag(context.Background(), del, ns, repo, tag)
				}
			}
		})
	}
	ref := func(tag string) naming.ImageReference { return refOf(t, cl, ns+"/"+repo+":"+tag) }

	rec := record(cl)
	// The registry describes itself once a while, not once a command.
	var d client.Discovery
	made := rec.during(t, "discover, twice", func() {
		d, err = cl.Discover(ctx)
		require.NoError(t, err)
		_, err = cl.Discover(ctx)
	})
	require.NoError(t, err)
	require.LessOrEqual(t, made["discover"], 1)

	var res transfer.PushResult
	rec.during(t, "push v1", func() {
		res, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("v1"), Store: builder, Recipe: v1, Mkfs: mkfsFor})
	})
	require.NoError(t, err)
	if !remote {
		// A shared registry may already hold the runtime from an earlier run.
		require.Equal(t, res.Blobs, res.BlobsUploaded)
	}
	made = rec.during(t, "push v2, one new blob", func() {
		res, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("v2"), Store: builder, Recipe: v2, Mkfs: mkfsFor})
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.BlobsUploaded)
	require.NotZero(t, made["pack-write"])

	// Pushing what the registry holds uploads nothing, and still tags it.
	made = rec.during(t, "push v2 again", func() {
		res, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("v2"), Store: builder, Recipe: v2, Mkfs: mkfsFor})
	})
	require.NoError(t, err)
	require.Equal(t, v2, res.Recipe)
	require.Zero(t, res.BlobsUploaded)
	require.Zero(t, res.PacksUploaded)
	require.Zero(t, made["pack-write"])

	// Tags written at once all land: they share objects on R2, which takes
	// one write a second to each.
	rec.during(t, "tag v2 eight times at once", func() {
		burst := make(chan error, 8)
		for i := range cap(burst) {
			go func() { burst <- cl.PutTag(ctx, token, ns, repo, fmt.Sprintf("burst-%d", i), v2) }()
		}
		for range cap(burst) {
			require.NoError(t, <-burst)
		}
	})

	// Another machine pulls both, anonymously, through the CDN layout.
	machine := newStore(t)
	out := filepath.Join(t.TempDir(), "disk.img")
	var got transfer.PullResult
	made = rec.during(t, "pull v1 into an empty store", func() {
		got, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("v1"), Store: machine, Mkfs: mkfsFor, Out: out})
	})
	require.NoError(t, err)
	require.Equal(t, v1, got.Recipe)
	require.Equal(t, got.Blobs, got.BlobsFetched)
	require.False(t, got.AlreadyCached)
	require.Equal(t, 1, made["count"])
	made = rec.during(t, "pull v2, one new blob", func() {
		got, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("v2"), Store: machine, Mkfs: mkfsFor, Out: out})
	})
	require.NoError(t, err)
	require.Equal(t, 1, got.BlobsFetched)
	require.Equal(t, 1, made["count"])

	// Pulling an image whose disk the machine holds reads the tag and the
	// recipe, then stops: no pack, no disk written, and no pull counted.
	held := got.Parsed.DiskDigest
	warm := filepath.Join(t.TempDir(), "warm.img")
	made = rec.during(t, "pull v2, disk already held", func() {
		got, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("v2"), Store: machine, Mkfs: mkfsFor, Out: warm,
			Have: func(disk string) bool { return disk == held }})
	})
	require.NoError(t, err)
	require.True(t, got.AlreadyCached)
	require.Equal(t, v2, got.Recipe)
	require.Zero(t, got.BlobsFetched)
	require.NoFileExists(t, warm)
	require.Zero(t, made["pack-read"])
	require.Zero(t, made["locations"])
	require.Zero(t, made["count"])
	require.Zero(t, made["discover"])
	// A machine that has the blobs but lost the disk builds it again.
	made = rec.during(t, "pull v2, disk lost", func() {
		got, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("v2"), Store: machine, Mkfs: mkfsFor, Out: warm,
			Have: func(string) bool { return false }})
	})
	require.NoError(t, err)
	require.False(t, got.AlreadyCached)
	require.Zero(t, got.BlobsFetched)
	require.FileExists(t, warm)
	require.Zero(t, made["pack-read"])

	// jerboa pull resolves the tag, checks that recipe's signatures, and
	// pulls the recipe it checked: a tag moved in between changes nothing,
	// and a recipe the repository never tagged is refused.
	require.NoError(t, cl.PutTag(ctx, token, ns, repo, "moving", v1))
	resolved, err := cl.Tag(ctx, d.CDN, ns, repo, "moving")
	require.NoError(t, err)
	require.Equal(t, v1, resolved.Recipe)
	require.NoError(t, cl.PutTag(ctx, token, ns, repo, "moving", v2))
	made = rec.during(t, "pull a resolved recipe", func() {
		got, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("moving"), Store: newStore(t), Mkfs: mkfsFor, Out: out, Recipe: resolved.Recipe})
	})
	require.NoError(t, err)
	require.Equal(t, v1, got.Recipe)
	require.Zero(t, made["tag-read"])
	require.NotZero(t, made["ref"])
	stranger := buildApp(t, mkfs, builder, fmt.Sprintf("console.log('never pushed %s')", repo))
	_, err = transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("moving"), Store: newStore(t), Mkfs: mkfsFor, Out: out, Recipe: stranger})
	require.ErrorContains(t, err, "is not in "+ns+"/"+repo)

	// One tag for both platforms, on registries that store image indexes.
	if d.Supports(client.FeatureIndex) {
		arm := buildAppFor(t, mkfs, builder, fmt.Sprintf("console.log('v2 %s')", repo), "linux/arm64")
		_, err = transfer.Push(ctx, transfer.PushOptions{Client: cl, Token: token, Ref: ref("v2-arm64"), Store: builder, Recipe: arm, Mkfs: mkfsFor})
		require.NoError(t, err)
		ix, err := transfer.PushIndex(ctx, transfer.IndexOptions{Client: cl, Token: token, Ref: ref("multi"), Sources: []naming.ImageReference{ref("v2-arm64"), ref("v2")}})
		require.NoError(t, err)
		for platform, want := range map[string]string{"linux/amd64": v2, "linux/arm64": arm} {
			got, err := transfer.Pull(ctx, transfer.PullOptions{Client: cl, Ref: ref("multi"), Store: machine, Mkfs: mkfsFor, Out: out, Platform: platform})
			require.NoError(t, err)
			require.Equal(t, want, got.Recipe)
			require.Equal(t, ix.Index, got.Index)
		}
	} else {
		t.Logf("%s does not store image indexes: skipping them", cl.Base)
	}

	if d.Supports(client.FeatureAttestations) {
		checkAttestations(t, ctx, cl, d, credential, token, ref("v2"), v2)
	} else {
		t.Logf("%s does not store attestations: skipping them", cl.Base)
	}

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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		// A private registry keeps its CDN behind Cloudflare Access too.
		if id, secret, ok := credentials.AccessToken(req.URL.Hostname()); ok {
			req.Header.Set("CF-Access-Client-Id", id)
			req.Header.Set("CF-Access-Client-Secret", secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		status, cacheControl = resp.Header.Get("cf-cache-status"), resp.Header.Get("Cache-Control")
		return status == "HIT"
	}, 30*time.Second, 2*time.Second, "CDN does not cache %s (cf-cache-status %q, Cache-Control %q)", url, status, cacheControl)
	require.Contains(t, cacheControl, "immutable")
}
