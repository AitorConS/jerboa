//go:build linux || (darwin && arm64)

package apiserver

import (
	"archive/tar"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/api"
)

// localCaller returns a context whose caller is this process over a Unix socket.
func localCaller(t *testing.T) context.Context {
	t.Helper()
	dir, err := os.MkdirTemp("", "jhp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	client, err := net.Dial("unix", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	server, err := ln.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	return withPeer(context.Background(), server)
}

func TestCheckHostImagePath(t *testing.T) {
	ctx := localCaller(t)
	disk := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(disk, []byte("x"), 0o600))
	link := filepath.Join(t.TempDir(), "link.img")
	require.NoError(t, os.Symlink(disk, link))

	got, err := checkHostImagePath(ctx, link)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(disk)
	require.NoError(t, err)
	require.Equal(t, want, got)

	_, err = checkHostImagePath(ctx, "disk.img")
	require.ErrorContains(t, err, "absolute")
	_, err = checkHostImagePath(ctx, t.TempDir())
	require.ErrorContains(t, err, "regular file")
	_, err = checkHostImagePath(context.Background(), disk)
	require.ErrorContains(t, err, "local clients")

	uid, ok := callerUID(ctx)
	require.True(t, ok)
	require.Equal(t, uint32(os.Getuid()), uid) //nolint:gosec // test uid
}

func TestResolveNamedVolumesRejectsRawPaths(t *testing.T) {
	s := &Server{}
	err := s.resolveNamedVolumes([]api.VolumeMountSpec{{DiskPath: "/etc/hosts", GuestPath: "/x"}})
	require.NotNil(t, err)
	require.Equal(t, -32602, err.Code)
}

func TestExtractBuildContextRejectsQuotedNames(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	name := `x"))y:(contents:(host:"z`
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("a"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	_, err = extractBuildContext(&buf, t.TempDir())
	require.ErrorContains(t, err, "unsupported character")
}
