//go:build !windows

package api

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// A socket something still answers on belongs to a running daemon. Taking
// it over would leave that daemon running, unreachable, with its VMs.
func TestListenRefusesALiveSocket(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "d.sock")
	first, err := Listen("unix://" + path)
	require.NoError(t, err)
	defer func() { _ = first.Close() }()

	_, err = Listen("unix://" + path)
	require.ErrorContains(t, err, "already listening")

	// The first listener is still the one behind the path.
	accepted := make(chan struct{})
	go func() {
		if conn, aerr := first.Accept(); aerr == nil {
			_ = conn.Close()
			close(accepted)
		}
	}()
	conn, err := net.Dial("unix", path)
	require.NoError(t, err)
	_ = conn.Close()
	<-accepted
}

// A socket file nobody listens on is what a killed daemon leaves behind.
func TestListenReplacesADeadSocket(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "d.sock")
	dead, err := net.Listen("unix", path)
	require.NoError(t, err)
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, dead.Close())
	require.FileExists(t, path)

	l, err := Listen("unix://" + path)
	require.NoError(t, err)
	require.NoError(t, l.Close())
}

// A mistyped --socket must not delete whatever is at the path.
func TestListenRefusesToReplaceWhatIsNotASocket(t *testing.T) {
	dir := shortSocketDir(t)
	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("keep me"), 0o600))
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(sub, 0o700))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(file, link))

	for _, path := range []string{file, sub, link} {
		_, err := Listen("unix://" + path)
		require.ErrorContains(t, err, "not a socket", path)
	}
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(data))
	require.DirExists(t, sub)
	_, err = os.Lstat(link)
	require.NoError(t, err)
}
