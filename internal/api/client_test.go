package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

func TestClientAttachTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  string
	}{
		{name: "clean"},
		{name: "guest failure", err: "HVF guest exit: Exited, code 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer ln.Close()
			go func() {
				conn, acceptErr := ln.Accept()
				if acceptErr != nil {
					return
				}
				defer conn.Close()
				var req api.Request
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				_ = json.NewEncoder(conn).Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"framed":true}`)})
				frames := api.NewFrameWriter(conn)
				_, _ = frames.Write([]byte("guest output\n"))
				_ = frames.Close()
				_ = json.NewEncoder(conn).Encode(api.AttachResult{Done: true, Error: tc.err})
			}()
			t.Setenv("JERBOA_AUTH_TOKEN", "")
			client, err := api.Dial("tcp://" + ln.Addr().String())
			require.NoError(t, err)
			defer client.Close()
			var output bytes.Buffer
			err = client.Attach(context.Background(), "vm", &output)
			require.Equal(t, "guest output\n", output.String())
			if tc.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.err)
			}
		})
	}
}

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

// startProtoServer stubs a daemon whose Auth.Hello reply advertises the given
// wire protocol version, so the client's proto negotiation can be exercised.
func startProtoServer(t *testing.T, proto int) string {
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
					if req.Method == "Auth.Hello" {
						raw, _ := json.Marshal(api.HelloResult{Status: "ok", Proto: proto})
						resp.Result = raw
					}
					_ = enc.Encode(resp)
				}
			}()
		}
	}()
	return "tcp://" + ln.Addr().String()
}

func TestClient_ProtoMatch(t *testing.T) {
	c, err := api.DialWithToken(startProtoServer(t, api.ProtoVersion), "tok")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
}

func TestClient_ProtoMismatch(t *testing.T) {
	_, err := api.DialWithToken(startProtoServer(t, api.ProtoVersion+1), "tok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "protocol version mismatch")
}
