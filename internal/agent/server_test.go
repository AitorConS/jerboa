package agent

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

func TestImageRefWildcardRouting(t *testing.T) {
	t.Setenv("JERBOA_AUTH_TOKEN", "daemon-token")
	refCh := make(chan string, 1)
	endpoint := startAgentRPCStub(t, refCh)
	srv := NewServer(Config{Endpoint: endpoint, Token: "agent-token", Version: "test"})

	req := httptest.NewRequest(http.MethodGet, "/v1/images/library/alpine:3.19", nil)
	req.Header.Set("Authorization", "Bearer agent-token")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "library/alpine:3.19", <-refCh)
}

func startAgentRPCStub(t *testing.T, refCh chan<- string) string {
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
						resp.Result = json.RawMessage(`{"status":"ok"}`)
					case "Image.Get":
						var p struct {
							Ref string `json:"ref"`
						}
						_ = json.Unmarshal(req.Params, &p)
						refCh <- p.Ref
						resp.Result = json.RawMessage(`{"name":"library/alpine","tag":"3.19"}`)
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

func httptestResponse() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}
