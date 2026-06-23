package apiserver_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/AitorConS/unikernel-engine/internal/apiserver"
	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/network"
	"github.com/AitorConS/unikernel-engine/internal/vm"
	"github.com/stretchr/testify/require"
)

// startAuthServer starts an in-process daemon requiring token (empty disables
// auth) and returns the socket path.
func startAuthServer(t *testing.T, token string) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "unid.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd(false)))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "", nil)
	require.NoError(t, err)
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	srv.SetImageStore(store)
	srv.EnableImageBuild(fakeMkfs(t))
	if token != "" {
		srv.SetAuthToken(token)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(cancel)

	require.Eventually(t, func() bool {
		c, derr := api.DialWithToken(socketPath, token)
		if derr != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond, "server did not start")
	return socketPath
}

func TestAuth_CorrectToken(t *testing.T) {
	socketPath := startAuthServer(t, "s3cret")
	client, err := api.DialWithToken(socketPath, "s3cret")
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	_, err = client.List(context.Background())
	require.NoError(t, err)
}

func TestAuth_WrongToken(t *testing.T) {
	socketPath := startAuthServer(t, "s3cret")
	_, err := api.DialWithToken(socketPath, "wrong")
	require.Error(t, err)
	require.Contains(t, err.Error(), "authentication")
}

func TestAuth_MissingToken(t *testing.T) {
	socketPath := startAuthServer(t, "s3cret")
	// Connect without a token: the handshake is skipped, so the first call is
	// rejected by the daemon.
	client, err := api.DialWithToken(socketPath, "")
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	_, err = client.List(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "authentication required")
}

func TestAuth_Disabled(t *testing.T) {
	socketPath := startAuthServer(t, "")
	client, err := api.DialWithToken(socketPath, "")
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	_, err = client.List(context.Background())
	require.NoError(t, err)
}

func TestAuth_BuildAuthenticatesDedicatedConn(t *testing.T) {
	socketPath := startAuthServer(t, "s3cret")
	client, err := api.DialWithToken(socketPath, "s3cret")
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	ctxTar := buildContextTar(t, map[string][]byte{"app": {0x7f, 'E', 'L', 'F', 0, 1}})
	res, err := client.ImageBuild(context.Background(), api.BuildParams{
		Name: "demo", Tag: "v1", Program: "app",
	}, ctxTar)
	require.NoError(t, err)
	require.Equal(t, "demo", res.Name)
}
