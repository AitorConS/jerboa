package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests put the registry commands in front of a registry that is down,
// broken or not a registry, and of a credentials file that is damaged or
// cannot be written. A command must fail with one line that names the
// problem, must never print or lose a stored token, and must not ask the
// daemon to do anything once the registry has failed.

const lfPAT = "jpat_secret_token_0123456789"

// lfRegistry is a loopback HTTP server standing in for a registry.
type lfRegistry struct {
	host string
	hits atomic.Int64
	mu   sync.Mutex
	seen []string
}

func lfNewRegistry(t *testing.T, handler func(hit int64, w http.ResponseWriter, r *http.Request)) *lfRegistry {
	t.Helper()
	reg := &lfRegistry{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg.mu.Lock()
		reg.seen = append(reg.seen, r.Method+" "+r.URL.Path)
		reg.mu.Unlock()
		handler(reg.hits.Add(1), w, r)
	}))
	t.Cleanup(srv.Close)
	reg.host = strings.TrimPrefix(srv.URL, "http://")
	return reg
}

func (r *lfRegistry) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// lfDiscovery is a valid answer to GET /v2/.
func lfDiscovery(w http.ResponseWriter, host string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"api": 1, "cdn": "http://" + host})
}

// lfRegistryEnv isolates the home directory and makes the CI token apply to
// host, the way a CI job is configured.
func lfRegistryEnv(t *testing.T, host string) string {
	t.Helper()
	home := fdIsolate(t)
	t.Setenv(credentials.EnvToken, lfPAT)
	t.Setenv(credentials.EnvHost, host)
	t.Setenv(credentials.EnvActionsTokenURL, "")
	t.Setenv(credentials.EnvActionsToken, "")
	t.Setenv(credentials.EnvAccessHost, "")
	return home
}

func lfCredentialsPath(home string) string {
	return filepath.Join(home, ".jerboa", "credentials.json")
}

// lfBrokenRegistries are ways a registry answers that no client can use and
// that retrying will not fix.
func lfBrokenRegistries() map[string]func(hit int64, w http.ResponseWriter, r *http.Request) {
	return map[string]func(int64, http.ResponseWriter, *http.Request){
		"not found": func(_ int64, w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"unauthorized": func(_ int64, w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"bad token"}`, http.StatusUnauthorized)
		},
		"html error page": func(_ int64, w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "<html><body>Forbidden by proxy</body></html>", http.StatusForbidden)
		},
		"garbage": func(_ int64, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("\x00\x01 not json")) },
		"truncated json": func(_ int64, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"api":1,"cdn":"http://`))
		},
		"empty body":      func(_ int64, _ http.ResponseWriter, _ *http.Request) {},
		"wrong json type": func(_ int64, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`["api",1]`)) },
		"another protocol": func(_ int64, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"api":99,"cdn":"http://x"}`))
		},
		"no cdn": func(_ int64, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"api":1}`)) },
	}
}

// --- login and logout ---

// A login against a registry that does not answer as one stores nothing: a
// token saved for a host that is not a registry would be sent to it later.
func TestLocalFault_LoginAgainstABrokenRegistry(t *testing.T) {
	for name, handler := range lfBrokenRegistries() {
		t.Run(name, func(t *testing.T) {
			reg := lfNewRegistry(t, handler)
			home := fdIsolate(t)
			res := fdExec(t, "", "login", "--token", lfPAT, reg.host)
			fdRequireCleanFailure(t, res, "login")
			assert.NotContains(t, res.err.Error(), lfPAT, "the token was printed in the error")
			assert.NotContains(t, res.stdout+res.stderr, lfPAT)
			_, err := os.Stat(lfCredentialsPath(home))
			require.ErrorIs(t, err, os.ErrNotExist, "a token was stored for a registry that did not answer")
			assert.Equal(t, []string{"GET /v2/"}, reg.requests(), "an answer that will not change was asked for again")
		})
	}
}

// A registry that is down is tried a few times and then given up on; one
// that recovers on a later try is logged in to.
func TestLocalFault_LoginRetriesATransientFailure(t *testing.T) {
	t.Run("stays down", func(t *testing.T) {
		reg := lfNewRegistry(t, func(_ int64, w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream connect error", http.StatusBadGateway)
		})
		home := fdIsolate(t)
		res := fdExec(t, "", "login", "--token", lfPAT, reg.host)
		fdRequireCleanFailure(t, res, "login")
		assert.Greater(t, reg.hits.Load(), int64(1), "a 502 was not retried")
		assert.LessOrEqual(t, reg.hits.Load(), int64(5), "the retries must be bounded")
		_, err := os.Stat(lfCredentialsPath(home))
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("recovers", func(t *testing.T) {
		var reg *lfRegistry
		reg = lfNewRegistry(t, func(hit int64, w http.ResponseWriter, _ *http.Request) {
			if hit == 1 {
				// The connection dies before any answer.
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			lfDiscovery(w, reg.host)
		})
		home := fdIsolate(t)
		res := fdExec(t, "", "login", "--token", lfPAT, reg.host)
		require.NoError(t, res.err)
		assert.Contains(t, res.stdout, "Stored a token for "+reg.host)
		assert.NotContains(t, res.stdout+res.stderr, lfPAT)

		info, err := os.Stat(lfCredentialsPath(home))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the credentials file must be readable by its owner only")
		f, err := credentials.Load()
		require.NoError(t, err)
		assert.Equal(t, lfPAT, f.Registries[reg.host].Token)
	})
}

func TestLocalFault_LoginToAnUnreachableRegistry(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // reserving a free port
	require.NoError(t, err)
	host := ln.Addr().String()
	require.NoError(t, ln.Close())
	home := fdIsolate(t)
	res := fdExec(t, "", "login", "--token", lfPAT, host)
	fdRequireCleanFailure(t, res, "login", host)
	assert.NotContains(t, res.err.Error(), lfPAT)
	_, err = os.Stat(lfCredentialsPath(home))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// lfHealthyRegistry answers discovery and nothing else.
func lfHealthyRegistry(t *testing.T) *lfRegistry {
	t.Helper()
	var reg *lfRegistry
	reg = lfNewRegistry(t, func(_ int64, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			lfDiscovery(w, reg.host)
			return
		}
		http.Error(w, `{"error":"unexpected request"}`, http.StatusTeapot)
	})
	return reg
}

// A damaged credentials file is reported with its path and left as it is:
// logging in or out must not replace it and silently drop every other login.
func TestLocalFault_DamagedCredentialsFile(t *testing.T) {
	contents := map[string]string{
		"truncated":  `{"registries":{"registry.example":{"token":"jpat_ab`,
		"garbage":    "\x00\x01\x02",
		"wrong type": `{"registries":["a"]}`,
		"not object": `"token"`,
	}
	for name, content := range contents {
		t.Run(name, func(t *testing.T) {
			reg := lfHealthyRegistry(t)
			home := fdIsolate(t)
			path := lfWrite(t, lfCredentialsPath(home), content)

			for _, args := range [][]string{
				{"login", "--token", lfPAT, reg.host},
				{"logout", reg.host},
				{"push", reg.host + "/acme/web:v1"},
				{"registry", "--registry", reg.host, "pat", "ls"},
			} {
				res := fdExec(t, "", args...)
				fdRequireCleanFailure(t, res, "credentials", path)
				assert.NotContains(t, res.err.Error(), "jpat_ab", "part of a stored token was printed")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, content, string(data), "%v replaced a damaged credentials file", args)
			}
		})
	}

	// An empty file is what an interrupted first login leaves: no logins.
	t.Run("empty", func(t *testing.T) {
		reg := lfHealthyRegistry(t)
		home := fdIsolate(t)
		lfWrite(t, lfCredentialsPath(home), "")
		res := fdExec(t, "", "login", "--token", lfPAT, reg.host)
		require.NoError(t, res.err)
		f, err := credentials.Load()
		require.NoError(t, err)
		assert.Equal(t, lfPAT, f.Registries[reg.host].Token)
	})
}

// Logging in to one registry and out of another never touches the tokens of
// the rest.
func TestLocalFault_LoginAndLogoutKeepOtherRegistries(t *testing.T) {
	reg := lfHealthyRegistry(t)
	home := fdIsolate(t)
	lfWrite(t, lfCredentialsPath(home), `{"registries":{"other.example":{"token":"jpat_other","login":"octocat"}}}`)

	require.NoError(t, fdExec(t, "", "login", "--token", lfPAT, reg.host).err)
	f, err := credentials.Load()
	require.NoError(t, err)
	assert.Equal(t, credentials.Entry{Token: "jpat_other", Login: "octocat"}, f.Registries["other.example"])
	assert.Equal(t, lfPAT, f.Registries[reg.host].Token)

	res := fdExec(t, "", "logout", reg.host)
	require.NoError(t, res.err)
	f, err = credentials.Load()
	require.NoError(t, err)
	assert.NotContains(t, f.Registries, reg.host)
	assert.Equal(t, "jpat_other", f.Registries["other.example"].Token)

	// Logging out twice, or of a registry never logged in to, is not an error.
	require.NoError(t, fdExec(t, "", "logout", reg.host).err)
	require.NoError(t, fdExec(t, "", "logout", "never.example").err)
}

// The disk fills up, errors or the process dies while the new token is
// saved. The file must then hold the logins from before or the new set,
// whole: a failed login never costs the user the logins they had.
func TestLocalFault_LoginSaveFailures(t *testing.T) {
	isCredentials := func(path string) bool { return filepath.Base(path) == "credentials.json" }
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			reg := lfHealthyRegistry(t)
			home := fdIsolate(t)
			lfWrite(t, lfCredentialsPath(home), `{"registries":{"other.example":{"token":"jpat_other"}}}`)

			inj := durabletest.Inject(t, f, 0, isCredentials)
			res := fdExec(t, "", "login", "--token", lfPAT, reg.host)
			require.True(t, inj.Fired())
			durable.SetFault(nil)
			require.Nil(t, res.panicked)

			got, err := credentials.Load()
			require.NoError(t, err, "a failed login left a credentials file that does not parse")
			assert.Equal(t, "jpat_other", got.Registries["other.example"].Token, "a failed login lost another registry's token")
			if res.err != nil {
				assert.NotContains(t, res.err.Error(), lfPAT)
				assert.NotContains(t, res.stdout, "Stored a token", "a login that failed to save reported success")
				if f.Step != durable.StepSyncDir {
					assert.NotContains(t, got.Registries, reg.host)
				}
			} else {
				assert.Equal(t, lfPAT, got.Registries[reg.host].Token)
			}
		})
	}
}

func TestLocalFault_LoginCannotWriteCredentials(t *testing.T) {
	lfSkipWithoutPermissions(t)
	reg := lfHealthyRegistry(t)
	home := fdIsolate(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, ".jerboa"), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, ".jerboa"), 0o700) })
	res := fdExec(t, "", "login", "--token", lfPAT, reg.host)
	fdRequireCleanFailure(t, res, "credentials")
	assert.NotContains(t, res.err.Error(), lfPAT)
	assert.NotContains(t, res.stdout, "Stored a token")
}

// --- push, pull and account commands ---

// Without a credential for the registry nothing is sent anywhere, and the
// error says how to log in.
func TestLocalFault_PushWithoutCredentials(t *testing.T) {
	reg := lfHealthyRegistry(t)
	d := fdNew(t).all(fdOK(nil))
	fdIsolate(t)
	t.Setenv(credentials.EnvToken, "")
	t.Setenv(credentials.EnvActionsTokenURL, "")
	t.Setenv(credentials.EnvActionsToken, "")
	res := fdExec(t, "", "--host", d.endpoint, "push", reg.host+"/acme/web:v1")
	fdRequireCleanFailure(t, res, "not logged in to "+reg.host, "jerboa login "+reg.host)
	assert.Empty(t, reg.requests())
	assert.Empty(t, d.methods())

	// A CI token meant for another registry is not sent to this one.
	t.Setenv(credentials.EnvToken, lfPAT)
	t.Setenv(credentials.EnvHost, "other.example")
	res = fdExec(t, "", "--host", d.endpoint, "push", reg.host+"/acme/web:v1")
	fdRequireCleanFailure(t, res, "not logged in to "+reg.host)
	assert.NotContains(t, res.err.Error(), lfPAT)
	assert.Empty(t, reg.requests(), "a token for another registry was sent")
}

// The registry fails while a push is being authorized: the daemon is never
// asked to upload, and the token is not in the error.
func TestLocalFault_PushAgainstABrokenRegistry(t *testing.T) {
	for name, handler := range lfBrokenRegistries() {
		t.Run(name, func(t *testing.T) {
			reg := lfNewRegistry(t, handler)
			d := fdNew(t).all(fdOK(nil))
			lfRegistryEnv(t, reg.host)
			res := fdExec(t, "", "--host", d.endpoint, "push", reg.host+"/acme/web:v1")
			fdRequireCleanFailure(t, res, "push")
			assert.NotContains(t, res.err.Error(), lfPAT)
			assert.Empty(t, d.methods(), "the daemon was asked to push to a registry that refused the client")
			assert.Empty(t, res.stdout)
		})
	}
}

// References that cannot name an image in a registry are refused before the
// registry, the credentials or the daemon are consulted.
func TestLocalFault_PushAndPullRejectBadReferences(t *testing.T) {
	reg := lfHealthyRegistry(t)
	d := fdNew(t).all(fdOK(nil))
	lfRegistryEnv(t, reg.host)
	digest := "@sha256:" + strings.Repeat("0", 64)
	for _, ref := range []string{
		"", "UPPER/web:v1", "acme//web:v1", "acme/web:-bad", "acme/web:" + strings.Repeat("t", 200), "../etc/passwd",
		"acme/web:v1:v2", "acme/we b:v1", "http://acme/web", reg.host + "/acme/web:v1\nInjected: header",
	} {
		t.Run("push/"+ref, func(t *testing.T) {
			res := fdExec(t, "", "--host", d.endpoint, "push", ref)
			require.Nil(t, res.panicked)
			require.Error(t, res.err)
			assert.Empty(t, reg.requests())
			assert.Empty(t, d.methods())
		})
		t.Run("pull/"+ref, func(t *testing.T) {
			res := fdExec(t, "", "--host", d.endpoint, "pull", ref)
			require.Nil(t, res.panicked)
			require.Error(t, res.err)
			assert.Empty(t, d.methods(), "a bad reference reached the daemon")
		})
	}
	res := fdExec(t, "", "--host", d.endpoint, "push", reg.host+"/acme/web"+digest)
	fdRequireCleanFailure(t, res, "push to a tag, not a digest")
	res = fdExec(t, "", "--host", d.endpoint, "pull", "--platform", "plan9/mips", reg.host+"/acme/web:v1")
	fdRequireCleanFailure(t, res, "pull")
	assert.Empty(t, d.methods())
}

// A pull is verified against the registry before the daemon downloads
// anything: when the registry fails, the image store is not touched.
func TestLocalFault_PullAgainstABrokenRegistry(t *testing.T) {
	for name, handler := range lfBrokenRegistries() {
		t.Run(name, func(t *testing.T) {
			reg := lfNewRegistry(t, handler)
			d := fdNew(t).all(fdOK(nil))
			lfRegistryEnv(t, reg.host)
			res := fdExec(t, "", "--host", d.endpoint, "pull", reg.host+"/acme/web:v1")
			fdRequireCleanFailure(t, res, "pull")
			assert.Zero(t, d.count("Image.Pull"), "the daemon was asked to pull an image that could not be checked")
			assert.NotContains(t, res.stdout, "Pulled")
		})
	}
}

// The account commands print what the registry said only when it said it in
// full; tokens are printed once, on creation, and never in an error.
func TestLocalFault_AccountCommandsAgainstABrokenRegistry(t *testing.T) {
	commands := [][]string{
		{"pat", "ls"},
		{"pat", "create", "ci"},
		{"pat", "rm", "pat_123"},
	}
	refused := map[string]bool{"not found": true, "unauthorized": true, "html error page": true}
	for name, handler := range lfBrokenRegistries() {
		for _, sub := range commands {
			t.Run(name+"/"+strings.Join(sub, " "), func(t *testing.T) {
				reg := lfNewRegistry(t, handler)
				lfRegistryEnv(t, reg.host)
				res := fdExec(t, "", append([]string{"registry", "--registry", reg.host}, sub...)...)
				fdRequireNoCrashText(t, res)
				if res.err != nil {
					assert.NotContains(t, res.err.Error(), lfPAT)
					assert.NotContains(t, res.err.Error(), "\n")
				}
				switch {
				case refused[name]:
					// The registry said no: every command fails.
					fdRequireCleanFailure(t, res, "pat "+sub[1])
					assert.Empty(t, res.stdout, "something was printed as the result of a refused request")
				case sub[1] == "create":
					// None of these answers holds a token, so none is a
					// token created.
					fdRequireCleanFailure(t, res, "pat create")
					assert.Empty(t, res.stdout, "a line was printed where the new token goes")
				case sub[1] == "ls":
					// An answer without tokens lists none.
					assert.Empty(t, res.stdout)
				}
			})
		}
	}

	t.Run("expiry out of range is refused locally", func(t *testing.T) {
		reg := lfHealthyRegistry(t)
		lfRegistryEnv(t, reg.host)
		for _, days := range []string{"0", "-1", "366", "99999999999"} {
			res := fdExec(t, "", "registry", "--registry", reg.host, "pat", "create", "ci", "--expires-in", days)
			require.Nil(t, res.panicked)
			require.Error(t, res.err)
			assert.Empty(t, reg.requests())
		}
	})
}
