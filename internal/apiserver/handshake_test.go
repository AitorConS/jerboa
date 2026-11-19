//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

func startHandshakeServer(t *testing.T, token string, timeout ...time.Duration) string {
	t.Helper()
	_, addr := newHandshakeServer(t, token, timeout...)
	return addr
}

func newHandshakeServer(t *testing.T, token string, timeout ...time.Duration) (*Server, string) {
	t.Helper()
	return newHandshakeServerWith(t, token, func(srv *Server) {
		if len(timeout) > 0 {
			srv.handshakeTimeout = timeout[0]
		}
	})
}

// newHandshakeServerWith is newHandshakeServer with setup run on the server
// before it starts serving.
func newHandshakeServerWith(t *testing.T, token string, setup func(*Server)) (*Server, string) {
	t.Helper()
	srv, err := NewServer(vm.NewMockManager(), nil, "tcp://127.0.0.1:0", nil, "test", nil)
	require.NoError(t, err)
	srv.authFailureDelay = 0
	setup(srv)
	srv.SetAuthToken(token)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(cancel)
	return srv, srv.listener.Addr().String()
}

func rawCall(t *testing.T, dec *json.Decoder, enc *json.Encoder, id int64, method string, params any) api.Response {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	require.NoError(t, enc.Encode(api.Request{JSONRPC: "2.0", ID: id, Method: method, Params: raw}))
	var resp api.Response
	require.NoError(t, dec.Decode(&resp))
	return resp
}

func TestHandshake_ChallengeProof(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token")
	c, err := api.DialWithToken("tcp://"+addr, "s3cret-token")
	require.NoError(t, err)
	_, err = c.DaemonVersion(context.Background())
	require.NoError(t, err)
	_ = c.Close()

	_, err = api.DialWithToken("tcp://"+addr, "wrong")
	require.ErrorContains(t, err, "authentication")
}

func TestHandshake_LegacyTokenStillAccepted(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	resp := rawCall(t, dec, enc, 1, "Auth.Hello", api.AuthParams{Token: "s3cret-token"})
	require.Nil(t, resp.Error)
}

func TestHandshake_ProofIsBoundToTheNonce(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	require.Nil(t, rawCall(t, dec, enc, 1, "Auth.Challenge", struct{}{}).Error)
	// A proof computed for another nonce (a replayed one) is rejected.
	resp := rawCall(t, dec, enc, 2, "Auth.Hello", api.AuthParams{Proof: api.AuthProof("s3cret-token", []byte("another nonce, another session"))})
	require.NotNil(t, resp.Error)
}

func TestHandshake_UnauthenticatedReadIsBounded(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"Auth.Hello","params":{"token":"`))
	chunk := []byte(strings.Repeat("A", 32<<10))
	var werr error
	for range 1024 { // 32 MiB, far past the pre-auth budget
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, werr = conn.Write(chunk); werr != nil {
			break
		}
	}
	require.Error(t, werr, "the daemon must drop a peer that streams an oversized handshake")
}

func TestHandshake_IdlePeerTimesOut(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token", 200*time.Millisecond)
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err, "an idle unauthenticated connection must be closed")
}

func TestHandshake_DaemonProvesTheTokenAfterTheClient(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	resp := rawCall(t, dec, enc, 1, "Auth.Challenge", struct{}{})
	require.Nil(t, resp.Error)
	var challenge api.ChallengeResult
	require.NoError(t, json.Unmarshal(resp.Result, &challenge))
	clientNonce := api.NewAuthNonce()
	resp = rawCall(t, dec, enc, 2, "Auth.Hello", api.AuthParams{Proof: api.AuthProof("s3cret-token", challenge.Nonce), Proto: api.ProtoVersion, ClientNonce: clientNonce})
	require.Nil(t, resp.Error)
	var hello api.HelloResult
	require.NoError(t, json.Unmarshal(resp.Result, &hello))
	require.True(t, api.CheckServerAuthProof("s3cret-token", challenge.Nonce, clientNonce, hello.ServerProof))
	require.False(t, api.CheckServerAuthProof("other-token", challenge.Nonce, clientNonce, hello.ServerProof))
}

func TestHandshake_NoProofForAPeerWithoutTheToken(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	require.Nil(t, rawCall(t, dec, enc, 1, "Auth.Challenge", struct{}{}).Error)
	// The daemon answers a client nonce only after a valid proof, so it
	// cannot be used to compute answers for someone else's nonce.
	resp := rawCall(t, dec, enc, 2, "Auth.Hello", api.AuthParams{Proof: api.AuthProof("guess", []byte("x")), ClientNonce: api.NewAuthNonce()})
	require.NotNil(t, resp.Error)
	require.Empty(t, resp.Result)
}

func claim(t *testing.T, srv *Server, peer string) (*pendingHandshake, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	h, ok := srv.claimHandshake(peer, server)
	if !ok {
		return nil, client
	}
	return h, client
}

func requireClosed(t *testing.T, client net.Conn) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := client.Read(make([]byte, 1))
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrDeadlineExceeded, "the connection was left open")
}

func TestHandshake_SlotsAreSharedOutPerPeer(t *testing.T) {
	srv := &Server{handshakes: make(chan struct{}, maxPendingHandshake)}
	var first []*pendingHandshake
	var firstClients []net.Conn
	for range maxPendingPerPeer {
		h, c := claim(t, srv, "uid:1000")
		require.NotNil(t, h)
		first, firstClients = append(first, h), append(firstClients, c)
	}
	// One more from the same peer takes the oldest one's slot, not another.
	h, _ := claim(t, srv, "uid:1000")
	require.NotNil(t, h)
	require.Len(t, srv.pending["uid:1000"], maxPendingPerPeer)
	require.Len(t, srv.handshakes, maxPendingPerPeer)
	requireClosed(t, firstClients[0])
	require.False(t, srv.releaseHandshake(first[0]), "an evicted connection has no slot to give back")
	require.Len(t, srv.handshakes, maxPendingPerPeer)
	require.True(t, srv.releaseHandshake(first[1]))
	require.False(t, srv.releaseHandshake(first[1]), "released twice")
	require.Len(t, srv.handshakes, maxPendingPerPeer-1)

	h, _ = claim(t, srv, "uid:1001")
	require.NotNil(t, h, "another peer still gets a slot")
	// A peer that cannot be told apart from others is bound only by the
	// overall limit, and a full daemon refuses everyone.
	for range maxPendingHandshake - maxPendingPerPeer {
		h, _ := claim(t, srv, "")
		require.NotNil(t, h)
	}
	h, _ = claim(t, srv, "")
	require.Nil(t, h)
	h, _ = claim(t, srv, "uid:1002")
	require.Nil(t, h)
}

// Every Windows-side client reaches the WSL daemon as one peer, so any local
// process holding that peer's slots with idle connections used to lock the
// CLI out. A client that completes the handshake now gets in: the oldest idle
// connection gives up its slot.
func TestHandshake_IdleSquattersCannotLockAPeerOut(t *testing.T) {
	srv, addr := newHandshakeServerWith(t, "s3cret-token", func(srv *Server) {
		srv.peerOf = func(net.Conn) string { return "ip:192.0.2.1" }
	})
	var squatters []net.Conn
	for range maxPendingPerPeer {
		conn, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		squatters = append(squatters, conn)
	}
	require.Eventually(t, func() bool {
		srv.pendingMu.Lock()
		defer srv.pendingMu.Unlock()
		return len(srv.pending["ip:192.0.2.1"]) == maxPendingPerPeer
	}, 5*time.Second, 10*time.Millisecond)

	c, err := api.DialWithToken("tcp://"+addr, "s3cret-token")
	require.NoError(t, err)
	_, err = c.DaemonVersion(context.Background())
	require.NoError(t, err)
	_ = c.Close()
	requireClosed(t, squatters[0])
}

func TestHandshake_BusyDaemonSaysSo(t *testing.T) {
	// Every connection its own peer: only the overall limit applies.
	var n atomic.Int64
	srv, addr := newHandshakeServerWith(t, "s3cret-token", func(srv *Server) {
		srv.peerOf = func(net.Conn) string { return "ip:peer-" + strconv.FormatInt(n.Add(1), 10) }
	})
	for range maxPendingHandshake {
		conn, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
	}
	require.Eventually(t, func() bool {
		srv.pendingMu.Lock()
		defer srv.pendingMu.Unlock()
		return len(srv.handshakes) == maxPendingHandshake
	}, 5*time.Second, 10*time.Millisecond)
	_, err := api.DialWithToken("tcp://"+addr, "s3cret-token")
	require.ErrorContains(t, err, "daemon busy")
	require.NotContains(t, err.Error(), "update jerboad")
}
