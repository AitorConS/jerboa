//go:build linux || darwin

package builder

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSandboxHelper is the confined child of TestSandboxConfinesSteps: it
// does what the step asks and fails when it cannot.
func TestSandboxHelper(t *testing.T) {
	switch os.Getenv("JERBOA_SANDBOX_HELPER") {
	case "write":
		if err := os.WriteFile(os.Getenv("TARGET"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "read":
		if _, err := os.ReadFile(os.Getenv("TARGET")); err != nil {
			t.Fatal(err)
		}
	case "dial":
		c, err := net.DialTimeout("tcp", os.Getenv("TARGET"), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	default:
		t.Skip("run by TestSandboxConfinesSteps")
	}
}

func TestSandboxConfinesSteps(t *testing.T) {
	if sandboxName() == "" {
		t.Skip("no build sandbox can confine processes here")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	outside := filepath.Join(root, "outside")
	home := filepath.Join(root, "home")
	for _, d := range []string{work, outside, filepath.Join(home, ".aws")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	secret := filepath.Join(home, ".aws", "credentials")
	require.NoError(t, os.WriteFile(secret, []byte("x"), 0o600))
	t.Setenv("HOME", home)

	local, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = local.Close() }()

	// run reports whether the confined helper succeeded, and how long it took.
	run := func(action, target string) (bool, time.Duration) {
		st := Step{Dir: work, Env: append(os.Environ(), "JERBOA_SANDBOX_HELPER="+action, "TARGET="+target)}
		cmd, done, err := Sandbox{}.Command(context.Background(), st, os.Args[0], "-test.run=^TestSandboxHelper$", "-test.count=1")
		require.NoError(t, err)
		defer done()
		start := time.Now()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("%s %s: %s", action, target, out)
		}
		return err == nil, time.Since(start)
	}

	ok, _ := run("write", filepath.Join(work, "ok"))
	require.True(t, ok, "writing to the project")
	ok, _ = run("write", filepath.Join(outside, "no"))
	require.False(t, ok, "writing outside the project")
	require.NoFileExists(t, filepath.Join(outside, "no"))
	ok, _ = run("read", secret)
	require.False(t, ok, "reading a credential store")

	// A documentation address: without the sandbox the dial would wait for
	// its timeout; confined it is refused at once.
	ok, took := run("dial", "192.0.2.1:9")
	require.False(t, ok)
	require.Less(t, took, 2*time.Second)
	if sandboxName() == "sandbox-exec" {
		// bubblewrap gives the step a network namespace of its own, where the
		// host's loopback listener does not exist.
		ok, _ = run("dial", local.Addr().String())
		require.True(t, ok, "dialing the host's loopback")
	}
}
