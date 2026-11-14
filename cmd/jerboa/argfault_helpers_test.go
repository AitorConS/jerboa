package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/require"
)

// afEnv is an isolated machine for one CLI invocation: a throwaway home
// directory, no inherited daemon or registry settings, and a daemon endpoint
// that accepts connections but never answers.
type afEnv struct {
	t    *testing.T
	home string
	// host is the --host value of a socket that accepts and counts
	// connections. A command that validates its input first never shows up
	// in dials.
	host  string
	dials *atomic.Int64
}

// afIsolate points every path and setting the CLI reads at temporary
// locations, so a test can neither read nor change the machine it runs on.
func afIsolate(t *testing.T) *afEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{
		"JERBOA_HOST", "JERBOA_AUTH_TOKEN", "JERBOA_PACKAGE_ARCH", "JERBOA_GITHUB_URL",
		credentials.EnvToken, credentials.EnvHost,
		credentials.EnvActionsTokenURL, credentials.EnvActionsToken,
		credentials.EnvAccessHost, credentials.EnvAccessClientID, credentials.EnvAccessClientSecret,
		"SOURCE_DATE_EPOCH",
	} {
		t.Setenv(name, "")
	}
	// Nothing may reach the real release origin: this port answers nothing.
	t.Setenv("JERBOA_RELEASE_BASE", "http://127.0.0.1:1")
	config.SetSessionToken("")
	client.ForgetDiscoveries()
	t.Cleanup(func() {
		config.SetSessionToken("")
		client.ForgetDiscoveries()
	})

	// The package stores are package-level overrides, not home-relative.
	origPkg, origOps := pkgStoreDir, opsPkgStoreDir
	pkgStoreDir, opsPkgStoreDir = filepath.Join(home, "packages"), filepath.Join(home, "packages-ops")
	t.Cleanup(func() { pkgStoreDir, opsPkgStoreDir = origPkg, origOps })

	e := &afEnv{t: t, home: home, dials: &atomic.Int64{}}
	e.host = "unix://" + afIdleSocket(t, e.dials)
	return e
}

// afShortDir returns a directory with a short path: unix socket paths are
// limited to about a hundred bytes, which a test's own temp dir can exceed.
func afShortDir(t *testing.T) string {
	t.Helper()
	base := ""
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "jb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// afIdleSocket listens on a unix socket standing in for a daemon that is up
// but says nothing: it counts each connection and drops it as soon as the
// client sends anything, so a command that does reach the daemon fails at
// once instead of waiting for an answer.
func afIdleSocket(t *testing.T, dials *atomic.Int64) string {
	t.Helper()
	path := filepath.Join(afShortDir(t), "d.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]byte, 1)
				_, _ = conn.Read(buf)
				_ = conn.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return path
}

// afResult is what one CLI invocation produced.
type afResult struct {
	stdout, stderr string
	err            error
}

// run executes the root command as main does, against the idle daemon, with
// a deadline so a command that would wait forever fails the test instead.
func (e *afEnv) run(args ...string) afResult {
	e.t.Helper()
	return e.runWith(nil, 30*time.Second, args...)
}

func (e *afEnv) runWith(stdin []byte, timeout time.Duration, args ...string) afResult {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(bytes.NewReader(stdin))
	root.SetArgs(append([]string{"--host", e.host}, args...))
	err := root.ExecuteContext(ctx)
	return afResult{stdout: out.String(), stderr: errOut.String(), err: err}
}

// afCleanError asserts err is what a user should see for a mistake: a short
// message naming the problem, with none of the marks of a crash.
func afCleanError(t *testing.T, err error, want ...string) {
	t.Helper()
	require.Error(t, err)
	msg := err.Error()
	for _, mark := range []string{"goroutine ", "panic:", "runtime error", "nil pointer", "index out of range", ".go:"} {
		require.NotContains(t, msg, mark, "the error reads like a crash")
	}
	require.NotEmpty(t, strings.TrimSpace(msg))
	require.LessOrEqual(t, strings.Count(strings.TrimSpace(msg), "\n"), 2, "an error is a line or two, not a dump: %q", msg)
	for _, w := range want {
		require.Contains(t, msg, w)
	}
}

// afWrite creates a file with its parent directories and returns its path.
func afWrite(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// afSkipIfPermissionsIgnored skips tests that rely on file modes denying
// access: root ignores them and Windows does not have them.
func afSkipIfPermissionsIgnored(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions do not deny access here")
	}
}

// afDirEntries lists the names in dir.
func afDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// afLegacyVolume creates a volume in a client-side store, as CLIs from before
// the daemon owned volumes did, and returns the path of its disk.
func afLegacyVolume(t *testing.T, root, name string) string {
	t.Helper()
	store, err := volume.NewStore(root)
	require.NoError(t, err)
	v, err := store.Create(name, 1<<20)
	require.NoError(t, err)
	return v.DiskPath
}
