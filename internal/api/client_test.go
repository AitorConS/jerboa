package api_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

// startStubServer accepts JSON-RPC connections and replies with canned results.
// Daemon.Version returns a real string (exercising result unmarshalling); any
// request whose params contain "FORCE_ERROR" gets an RPC error; everything else
// gets an empty (null) result so each client wrapper exercises its happy path.
func startStubServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				dec := json.NewDecoder(conn)
				enc := json.NewEncoder(conn)
				for {
					var req api.Request
					if err := dec.Decode(&req); err != nil {
						return
					}
					resp := api.Response{JSONRPC: "2.0", ID: req.ID}
					switch {
					case req.Method == "Daemon.Version":
						resp.Result = json.RawMessage(`{"version":"v1.2.3"}`)
					case strings.Contains(string(req.Params), "FORCE_ERROR"):
						resp.Error = &api.RPCError{Code: -32000, Message: "boom"}
					}
					_ = enc.Encode(resp)
				}
			}()
		}
	}()
	return "tcp://" + ln.Addr().String()
}

func TestClient_AllMethods(t *testing.T) {
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	c, err := api.Dial(startStubServer(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()

	ver, err := c.DaemonVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, "v1.2.3", ver)

	_, err = c.Run(ctx, api.RunParams{Image: "hello:latest"})
	require.NoError(t, err)
	require.NoError(t, c.Stop(ctx, "id", false))
	require.NoError(t, c.Kill(ctx, "id"))
	require.NoError(t, c.Signal(ctx, "id", "SIGTERM"))
	require.NoError(t, c.Remove(ctx, "id"))
	require.NoError(t, c.Shutdown(ctx))

	_, err = c.List(ctx)
	require.NoError(t, err)
	_, err = c.Get(ctx, "id")
	require.NoError(t, err)
	_, err = c.Logs(ctx, "id")
	require.NoError(t, err)
	_, err = c.Inspect(ctx, "id")
	require.NoError(t, err)
	_, err = c.Stats(ctx, "id")
	require.NoError(t, err)
	_, err = c.NodeList(ctx)
	require.NoError(t, err)

	_, err = c.NetworkCreate(ctx, "net", "10.0.0.0/24", "bridge")
	require.NoError(t, err)
	_, err = c.NetworkList(ctx)
	require.NoError(t, err)
	_, err = c.NetworkGet(ctx, "net")
	require.NoError(t, err)
	require.NoError(t, c.NetworkRemove(ctx, "net"))
	_, err = c.NetworkAllocateIP(ctx, "net")
	require.NoError(t, err)
	require.NoError(t, c.NetworkReleaseIP(ctx, "net", "10.0.0.2"))

	_, err = c.DNSResolve(ctx, "web", "net")
	require.NoError(t, err)
	_, err = c.DNSResolveAll(ctx, "web", "net")
	require.NoError(t, err)
	_, err = c.DNSList(ctx, "net")
	require.NoError(t, err)

	_, err = c.ImageList(ctx)
	require.NoError(t, err)
	_, err = c.ImageGet(ctx, "hello:latest")
	require.NoError(t, err)
	require.NoError(t, c.ImageRemove(ctx, "hello:latest"))
}

func TestClient_RPCError(t *testing.T) {
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	c, err := api.Dial(startStubServer(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.Get(context.Background(), "FORCE_ERROR")
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

func TestClient_WithToken_Handshake(t *testing.T) {
	c, err := api.DialWithToken(startStubServer(t), "secret-token")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ver, err := c.DaemonVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v1.2.3", ver)
}

func TestDial_BadEndpoint(t *testing.T) {
	_, err := api.Dial("tcp://127.0.0.1:1")
	require.Error(t, err)
}
