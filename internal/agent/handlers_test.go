package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

// startNullRPCStub speaks just enough of the daemon JSON-RPC protocol to make
// every client call succeed: Auth.Hello gets an ok, anything else gets a null
// result (which decodes into each method's zero value).
func startNullRPCStub(t *testing.T) string {
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
					switch req.Method {
					case "Auth.Challenge":
						resp.Result = json.RawMessage(`{"nonce":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`)
					case "Auth.Hello":
						resp.Result = helloOK("daemon-token", req.Params)
					default:
						resp.Result = json.RawMessage(`null`)
					}
					_ = enc.Encode(resp)
				}
			}()
		}
	}()
	return "tcp://" + ln.Addr().String()
}

// helloOK accepts an Auth.Hello and answers the client's nonce for token, as
// the daemon does (the stubs' challenge nonce is 32 zero bytes).
func helloOK(token string, params json.RawMessage) json.RawMessage {
	var p api.AuthParams
	_ = json.Unmarshal(params, &p)
	raw, _ := json.Marshal(api.HelloResult{Status: "ok", ServerProof: api.ServerAuthProof(token, make([]byte, 32), p.ClientNonce)})
	return raw
}

func newStubServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("JERBOA_AUTH_TOKEN", "daemon-token")
	endpoint := startNullRPCStub(t)
	return NewServer(Config{Endpoint: endpoint, Token: "agent-token", Version: "test"})
}

func doAgentRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestHandlers_SuccessAgainstStubDaemon(t *testing.T) {
	s := newStubServer(t)
	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/health", ""},
		{http.MethodGet, "/v1/preflight", ""},
		{http.MethodGet, "/v1/daemon/status", ""},
		{http.MethodGet, "/v1/vms", ""},
		{http.MethodPost, "/v1/vms", `{"image":"hello:latest","image_path":"","memory":"256M","cpus":1}`},
		{http.MethodGet, "/v1/vms/abc", ""},
		{http.MethodGet, "/v1/vms/abc/inspect", ""},
		{http.MethodGet, "/v1/vms/abc/logs", ""},
		{http.MethodGet, "/v1/vms/abc/stats", ""},
		{http.MethodPost, "/v1/vms/abc/stop", `{"force":true}`},
		{http.MethodPost, "/v1/vms/abc/kill", ""},
		{http.MethodPost, "/v1/vms/abc/signal", `{"signal":"SIGTERM"}`},
		{http.MethodDelete, "/v1/vms/abc", ""},
		{http.MethodGet, "/v1/images", ""},
		{http.MethodDelete, "/v1/images/hello:latest", ""},
		{http.MethodGet, "/v1/networks", ""},
		{http.MethodPost, "/v1/networks", `{"name":"testnet","subnet":"10.0.0.0/24"}`},
		{http.MethodGet, "/v1/networks/testnet", ""},
		{http.MethodDelete, "/v1/networks/testnet", ""},
		{http.MethodGet, "/v1/dns", ""},
		{http.MethodGet, "/v1/dns/resolve?name=web&network=app", ""},
		{http.MethodGet, "/v1/dns/resolve?name=web&all=true", ""},
		{http.MethodGet, "/v1/nodes", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rr := doAgentRequest(t, s, tc.method, tc.path, tc.body)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		})
	}
}

func TestHandlers_DaemonUnreachableIs502(t *testing.T) {
	t.Setenv("JERBOA_AUTH_TOKEN", "daemon-token")
	// Nothing listens on port 1; every dial fails fast.
	s := NewServer(Config{Endpoint: "tcp://127.0.0.1:1", Token: "agent-token", Version: "test"})

	rr := doAgentRequest(t, s, http.MethodGet, "/v1/vms", "")
	require.Equal(t, http.StatusBadGateway, rr.Code)
	var body ErrorBody
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Equal(t, KindDaemonUnreachable, body.Error.Kind)

	// Streaming attach hits the same dial error before any SSE is written.
	rr = doAgentRequest(t, s, http.MethodGet, "/v1/vms/abc/attach", "")
	require.Equal(t, http.StatusBadGateway, rr.Code)

	// Health endpoints degrade gracefully instead of erroring.
	rr = doAgentRequest(t, s, http.MethodGet, "/v1/health", "")
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"daemonReachable":false`)
	rr = doAgentRequest(t, s, http.MethodGet, "/v1/daemon/status", "")
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"reachable":false`)
}

func TestHandlers_InvalidJSONBodyIs400(t *testing.T) {
	s := newStubServer(t)
	for _, path := range []string{"/v1/vms", "/v1/vms/abc/stop", "/v1/vms/abc/signal", "/v1/networks"} {
		rr := doAgentRequest(t, s, http.MethodPost, path, "{not json")
		require.Equal(t, http.StatusBadRequest, rr.Code, "path %s", path)
	}
}

func TestHandleDaemonStop_NotSupportedOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows this would stop a real WSL daemon")
	}
	s := newStubServer(t)
	rr := doAgentRequest(t, s, http.MethodPost, "/v1/daemon/stop", "")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), string(KindNotSupported))
}

func TestHandleEvents_StreamsDaemonStatus(t *testing.T) {
	s := newStubServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer agent-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	// The poller's first poll runs on subscribe and reports the stub as
	// reachable before the request context expires.
	require.Contains(t, rr.Body.String(), "daemon-status")
	require.Contains(t, rr.Body.String(), `"reachable":true`)
}

func TestServe_ShutsDownOnContextCancel(t *testing.T) {
	s := newStubServer(t)
	ln, err := s.Listen()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()

	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/v1/preflight", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer agent-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	cancel()
	select {
	case err := <-served:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}

func TestDaemonPort(t *testing.T) {
	cases := []struct {
		endpoint, want string
	}{
		{"tcp://127.0.0.1:9999", "9999"},
		{"tcp://[::1]:8080", "8080"},
		{"tcp://hostonly", "7890"},
		{"unix:///run/jerboad.sock", "7890"},
		{"", "7890"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, daemonPort(tc.endpoint), "endpoint %q", tc.endpoint)
	}
}

func TestResolveEndpoint_OverrideWinsEverywhere(t *testing.T) {
	got, err := ResolveEndpoint("tcp://10.1.2.3:7890")
	require.NoError(t, err)
	require.Equal(t, "tcp://10.1.2.3:7890", got)
}

func TestWSLConfig_DerivesListenEndpoint(t *testing.T) {
	s := NewServer(Config{Endpoint: "tcp://172.20.0.2:7891", Distro: "jerboa", User: "root"})
	cfg := s.wslConfig("tok")
	require.Equal(t, "tcp://0.0.0.0:7891", cfg.ListenEndpoint)
	require.Equal(t, "tcp://172.20.0.2:7891", cfg.Endpoint)
	require.Equal(t, "tok", cfg.Token)

	// Unix-socket endpoints must not get a TCP listen override.
	s = NewServer(Config{Endpoint: "unix:///run/jerboad.sock"})
	require.Empty(t, s.wslConfig("tok").ListenEndpoint)
}
