package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/api"
)

// quickWatchdog makes the watchdog act within a test's patience.
func quickWatchdog(t *testing.T) {
	t.Helper()
	t.Cleanup(api.SetWatchdog(50*time.Millisecond, 50*time.Millisecond))
	t.Cleanup(api.SetHandshakeTimeout(200 * time.Millisecond))
}

// A daemon that accepts connections and answers nothing, on any of them, is
// stuck: the call ends, says why, and the connection is retired.
func TestWatchdog_ASilentDaemonEndsTheCall(t *testing.T) {
	quickWatchdog(t)
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	c := dialFake(t, endpoint)

	began := time.Now()
	err := returns(t, "List", func() error { _, err := c.List(context.Background()); return err })
	require.ErrorIs(t, err, api.ErrDaemonUnresponsive)
	require.ErrorContains(t, err, "VM.List")
	assert.Less(t, time.Since(began), 5*time.Second)

	_, err = c.Get(context.Background(), "vm-1")
	require.ErrorIs(t, err, api.ErrDaemonUnresponsive, "a later call on the retired connection")
}

// busyDaemon holds the first request it is sent until release closes, and
// answers every other connection at once, as a daemon in the middle of a
// long build does. probes counts those other requests.
func busyDaemon(t *testing.T, release <-chan struct{}, probes *atomic.Int32) string {
	t.Helper()
	var first atomic.Bool
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
		dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
		var req api.Request
		if dec.Decode(&req) != nil {
			return
		}
		if first.CompareAndSwap(false, true) {
			<-release
			_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`[]`)})
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		probes.Add(1)
		_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"version":"test"}`)})
	})
	return endpoint
}

// A daemon that is busy still answers the watchdog, so a long call is left
// alone however long it takes, and is asked about more than once.
func TestWatchdog_ABusyDaemonKeepsTheCall(t *testing.T) {
	quickWatchdog(t)
	release := make(chan struct{})
	var probes atomic.Int32
	c := dialFake(t, busyDaemon(t, release, &probes))

	done := make(chan error, 1)
	go func() { _, err := c.List(context.Background()); done <- err }()
	require.Eventually(t, func() bool { return probes.Load() >= 3 }, patience, 10*time.Millisecond, "the watchdog stopped asking")
	select {
	case err := <-done:
		t.Fatalf("a call to a busy daemon was ended: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(patience):
		t.Fatal("the call did not return once the daemon answered")
	}
	// The connection is intact.
}

// A call answered before the watchdog's delay costs nothing: no second
// connection is opened.
func TestWatchdog_AFastCallOpensNoSecondConnection(t *testing.T) {
	t.Cleanup(api.SetWatchdog(time.Hour, time.Hour))
	var conns atomic.Int32
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
		conns.Add(1)
		answer(conn, func(api.Request) api.Response { return api.Response{Result: json.RawMessage(`[]`)} })
	})
	c := dialFake(t, endpoint)
	for range 20 {
		_, err := c.List(context.Background())
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), conns.Load())
}

// The daemon dies while the watchdog is asking: the call reports the lost
// connection, not a stuck daemon.
func TestWatchdog_ADaemonThatGoesAwayIsNotCalledStuck(t *testing.T) {
	quickWatchdog(t)
	// Each connection is read once and closed shortly after, the call's
	// and the watchdog's alike.
	endpoint, _ := fakeDaemon(t, afterRequest(func(net.Conn) { time.Sleep(120 * time.Millisecond) }))
	c := dialFake(t, endpoint)
	err := returns(t, "List", func() error { _, err := c.List(context.Background()); return err })
	require.Error(t, err)
	assert.NotErrorIs(t, err, api.ErrDaemonUnresponsive)
}

// With a token the watchdog authenticates like any connection: a daemon
// that answers the handshake is alive.
func TestWatchdog_WithATokenASilentDaemonEndsTheCall(t *testing.T) {
	quickWatchdog(t)
	var served atomic.Int32
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
		if served.Add(1) > 1 {
			// Every connection after the first: accept and stay silent.
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		authThenSilent(conn, "secret-token")
	})
	c, err := api.DialWithToken(endpoint, "secret-token")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	err = returns(t, "List", func() error { _, err := c.List(context.Background()); return err })
	require.ErrorIs(t, err, api.ErrDaemonUnresponsive)
}

// authThenSilent serves the handshake as jerboad does and then answers
// nothing more.
func authThenSilent(conn net.Conn, token string) {
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	nonce := api.NewAuthNonce()
	for {
		var req api.Request
		if dec.Decode(&req) != nil {
			return
		}
		resp := api.Response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "Auth.Challenge":
			resp.Result, _ = json.Marshal(api.ChallengeResult{Nonce: nonce})
		case "Auth.Hello":
			var p api.AuthParams
			_ = json.Unmarshal(req.Params, &p)
			resp.Result, _ = json.Marshal(api.HelloResult{Status: "ok", Proto: api.ProtoVersion, ServerProof: api.ServerAuthProof(token, nonce, p.ClientNonce)})
		default:
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		if enc.Encode(resp) != nil {
			return
		}
	}
}
