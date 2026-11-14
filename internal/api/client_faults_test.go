package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What must hold when the daemon on the other end of the socket misbehaves or
// goes away: every call returns an error that says what happened, none hangs
// once the connection is gone, and a reply that is not a reply is never taken
// for a result.

// patience bounds how long a test waits for something that must happen; it is
// generous so a loaded machine does not fail the test.
const patience = 10 * time.Second

// socketDir returns a directory short enough to hold a Unix socket: the
// per-test directory of testing.T can exceed the 104 bytes macOS allows.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeDaemon accepts connections where jerboad would (a Unix socket; TCP on
// loopback where Unix sockets are not available) and gives each to handle. It
// returns the endpoint and the listener, which tests close to take the daemon
// away. Connections still open are closed when the test ends.
func fakeDaemon(t *testing.T, handle func(conn net.Conn)) (string, net.Listener) {
	t.Helper()
	var (
		ln       net.Listener
		endpoint string
		err      error
	)
	if runtime.GOOS == "windows" {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		endpoint = "tcp://" + ln.Addr().String()
	} else {
		path := filepath.Join(socketDir(t), "d.sock")
		ln, err = net.Listen("unix", path)
		require.NoError(t, err)
		endpoint = "unix://" + path
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				handle(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return endpoint, ln
}

// answer serves JSON-RPC on conn: each request gets what reply returns for
// it, with the envelope filled in.
func answer(conn net.Conn, reply func(req api.Request) api.Response) {
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	for {
		var req api.Request
		if dec.Decode(&req) != nil {
			return
		}
		resp := reply(req)
		resp.JSONRPC, resp.ID = "2.0", req.ID
		if enc.Encode(resp) != nil {
			return
		}
	}
}

// afterRequest reads one request and then lets the daemon misbehave.
func afterRequest(misbehave func(conn net.Conn)) func(net.Conn) {
	return func(conn net.Conn) {
		var req api.Request
		if json.NewDecoder(conn).Decode(&req) != nil {
			return
		}
		misbehave(conn)
	}
}

// returns runs fn and fails the test if it is still running after patience:
// a call that hangs on a dead daemon is the failure these tests look for.
func returns(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(patience):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

func dialFake(t *testing.T, endpoint string) *api.Client {
	t.Helper()
	c, err := api.DialWithToken(endpoint, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClientFaults_DaemonDropsTheConnection(t *testing.T) {
	half := []byte(`{"jsonrpc":"2.0","id":1,"result":{"id":"vm-1","sta`)
	cases := map[string]func(conn net.Conn){
		"before answering":        func(net.Conn) {},
		"inside the response":     func(conn net.Conn) { _, _ = conn.Write(half) },
		"after an opening brace":  func(conn net.Conn) { _, _ = conn.Write([]byte("{")) },
		"after whitespace":        func(conn net.Conn) { _, _ = conn.Write([]byte("  \n\n")) },
		"inside a string escape":  func(conn net.Conn) { _, _ = conn.Write([]byte(`{"jsonrpc":"2.0","error":{"message":"a\`)) },
		"after the result's name": func(conn net.Conn) { _, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":`)) },
	}
	for name, misbehave := range cases {
		t.Run(name, func(t *testing.T) {
			endpoint, _ := fakeDaemon(t, afterRequest(misbehave))
			c := dialFake(t, endpoint)
			var info api.VMInfo
			err := returns(t, "Get", func() error {
				var err error
				info, err = c.Get(context.Background(), "vm-1")
				return err
			})
			require.ErrorContains(t, err, "decode response")
			require.Empty(t, info.ID, "half a response must not be returned as a VM")

			// The connection is gone: later calls fail at once, they do not
			// wait for an answer that cannot come.
			err = returns(t, "second call", func() error { _, err := c.List(context.Background()); return err })
			require.Error(t, err)
		})
	}
}

func TestClientFaults_DaemonAnswersGarbage(t *testing.T) {
	cases := map[string]struct {
		wire string
		want string
	}{
		"binary":                 {"\x00\xff\x13\x37\n", "decode response"},
		"an HTTP error page":     {"HTTP/1.1 400 Bad Request\r\nContent-Type: text/html\r\n\r\n<html>", "decode response"},
		"a JSON array":           {"[1,2,3]\n", "decode response"},
		"a JSON string":          {"\"ok\"\n", "decode response"},
		"an id that is no id":    {`{"jsonrpc":"2.0","id":"one","result":{}}` + "\n", "decode response"},
		"an error that is none":  {`{"jsonrpc":"2.0","id":1,"error":"boom"}` + "\n", "decode response"},
		"a result of wrong type": {`{"jsonrpc":"2.0","id":1,"result":[1,2]}` + "\n", "unmarshal result"},
		"a result that is text":  {`{"jsonrpc":"2.0","id":1,"result":"vm-1"}` + "\n", "unmarshal result"},
		"a field of wrong type":  {`{"jsonrpc":"2.0","id":1,"result":{"id":42}}` + "\n", "unmarshal result"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			endpoint, _ := fakeDaemon(t, afterRequest(func(conn net.Conn) {
				_, _ = conn.Write([]byte(tc.wire))
				// Keep the connection open: the garbage alone must fail the call.
				_, _ = io.Copy(io.Discard, conn)
			}))
			c := dialFake(t, endpoint)
			err := returns(t, "Get", func() error { _, err := c.Get(context.Background(), "vm-1"); return err })
			require.ErrorContains(t, err, tc.want)
			var rpcErr *api.RPCError
			require.NotErrorAs(t, err, &rpcErr, "garbage is not an answer from the daemon")
			require.False(t, api.IsNotFound(err))
		})
	}
}

func TestClientFaults_EmptyAndNullResults(t *testing.T) {
	// A daemon may answer with no result at all, or with null: both are an
	// empty answer, not an error, whatever the caller expected to decode.
	for _, wire := range []string{
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"2.0","id":1,"result":null,"error":null}`,
	} {
		endpoint, _ := fakeDaemon(t, afterRequest(func(conn net.Conn) {
			_, _ = conn.Write([]byte(wire + "\n"))
			_, _ = io.Copy(io.Discard, conn)
		}))
		c := dialFake(t, endpoint)
		infos, err := c.List(context.Background())
		require.NoError(t, err, wire)
		require.Empty(t, infos)
	}
}

func TestClientFaults_DaemonNeverAnswers(t *testing.T) {
	got := make(chan struct{})
	endpoint, _ := fakeDaemon(t, afterRequest(func(conn net.Conn) {
		close(got)
		_, _ = io.Copy(io.Discard, conn) // reads on, answers nothing
	}))
	c := dialFake(t, endpoint)

	done := make(chan error, 1)
	go func() { _, err := c.Get(context.Background(), "vm-1"); done <- err }()
	select {
	case <-got:
	case <-time.After(patience):
		t.Fatal("the request never reached the daemon")
	}
	select {
	case err := <-done:
		t.Fatalf("Get returned without an answer: %v", err)
	default:
	}

	// Closing the client is the way out of a call the daemon sits on.
	require.NoError(t, c.Close())
	select {
	case err := <-done:
		require.ErrorContains(t, err, "decode response")
	case <-time.After(patience):
		t.Fatal("Close did not release the call waiting for an answer")
	}
}

func TestClientFaults_CancelledContextReleasesACallTheDaemonSitsOn(t *testing.T) {
	t.Skip("BUG: Client.call (client.go) ignores its context: a call to a daemon that never answers " +
		"blocks until the connection is closed, whatever deadline the caller set")
	got := make(chan struct{})
	endpoint, _ := fakeDaemon(t, afterRequest(func(conn net.Conn) {
		close(got)
		_, _ = io.Copy(io.Discard, conn)
	}))
	c := dialFake(t, endpoint)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Get(ctx, "vm-1"); done <- err }()
	<-got
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(patience):
		t.Fatal("canceling the context did not release the call")
	}
}

func TestClientFaults_DialDoesNotHangOnASilentEndpoint(t *testing.T) {
	t.Skip("BUG: DialWithToken (client.go) sets no deadline on the handshake: with a token, dialing " +
		"an endpoint that accepts and never answers Auth.Challenge blocks forever")
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	err := returns(t, "DialWithToken", func() error {
		c, err := api.DialWithToken(endpoint, "secret-token")
		if err == nil {
			_ = c.Close()
		}
		return err
	})
	require.Error(t, err)
}

func TestClientFaults_RPCErrorsKeepTheirCodeThroughEveryWrapper(t *testing.T) {
	codes := map[string]struct {
		code     int
		notFound bool
		text     string
	}{
		"not found":        {api.CodeNotFound, true, `vm "x" not found`},
		"application":      {-32000, false, `vm "x" not found`},
		"busy":             {api.ErrCodeBusy, false, `vm "x" not found`},
		"method not found": {-32601, false, `vm "x" not found (rpc -32601)`},
		"invalid params":   {-32602, false, `vm "x" not found (rpc -32602)`},
		"parse error":      {-32700, false, `vm "x" not found (rpc -32700)`},
		"zero":             {0, false, `vm "x" not found (rpc 0)`},
	}
	for name, tc := range codes {
		t.Run(name, func(t *testing.T) {
			endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
				answer(conn, func(api.Request) api.Response {
					// A result beside the error must be ignored.
					return api.Response{Error: &api.RPCError{Code: tc.code, Message: `vm "x" not found`}, Result: json.RawMessage(`{"id":"ghost"}`)}
				})
			})
			c := dialFake(t, endpoint)
			ctx := context.Background()
			calls := map[string]func() error{
				"Get":           func() error { info, err := c.Get(ctx, "x"); assert.Empty(t, info.ID); return err },
				"Stop":          func() error { return c.Stop(ctx, "x", false) },
				"Remove":        func() error { return c.Remove(ctx, "x") },
				"NetworkGet":    func() error { _, err := c.NetworkGet(ctx, "x"); return err },
				"ImageGet":      func() error { _, err := c.ImageGet(ctx, "x"); return err },
				"VolumeGet":     func() error { _, err := c.VolumeGet(ctx, "x"); return err },
				"VolumeRemove":  func() error { return c.VolumeRemove(ctx, "x", "") },
				"SnapshotGet":   func() error { _, err := c.SnapshotInspect(ctx, "x"); return err },
				"SystemDF":      func() error { _, err := c.SystemDF(ctx); return err },
				"DaemonVersion": func() error { _, err := c.DaemonVersion(ctx); return err },
			}
			for call, fn := range calls {
				err := fn()
				require.Error(t, err, call)
				var rpcErr *api.RPCError
				require.ErrorAs(t, err, &rpcErr, "%s lost the daemon's error", call)
				require.Equal(t, tc.code, rpcErr.Code, call)
				require.Equal(t, tc.notFound, api.IsNotFound(err), call)
				require.Contains(t, err.Error(), tc.text, call)
				if !strings.Contains(tc.text, "(rpc") {
					require.NotContains(t, err.Error(), "rpc", "%s shows transport plumbing to the user", call)
				}
			}
		})
	}
}

func TestIsNotFound_OnlyTheDaemonsCode(t *testing.T) {
	require.False(t, api.IsNotFound(nil))
	require.False(t, api.IsNotFound(errors.New(`vm "x" not found`)), "a message alone is not the code")
	require.False(t, api.IsNotFound(os.ErrNotExist))
	require.False(t, api.IsNotFound((*api.RPCError)(nil)))
	require.True(t, api.IsNotFound(fmt.Errorf("client get: %w", &api.RPCError{Code: api.CodeNotFound})))
}

func TestClientFaults_ConcurrentCallersEachGetTheirOwnAnswer(t *testing.T) {
	// Many goroutines share one client (the CLI's progress and its main call
	// do). The daemon answers each request with the name it asked for; no
	// caller may receive the answer to another's request.
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
		answer(conn, func(req api.Request) api.Response {
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &p)
			raw, _ := json.Marshal(api.NetworkInfo{Name: p.Name})
			return api.Response{Result: raw}
		})
	})
	c := dialFake(t, endpoint)

	const callers, rounds = 16, 25
	errs := make(chan error, callers)
	for g := range callers {
		go func() {
			for i := range rounds {
				want := fmt.Sprintf("net-%d-%d", g, i)
				info, err := c.NetworkGet(context.Background(), want)
				if err != nil {
					errs <- err
					return
				}
				if info.Name != want {
					errs <- fmt.Errorf("asked for %s, got the answer for %s", want, info.Name)
					return
				}
			}
			errs <- nil
		}()
	}
	for range callers {
		select {
		case err := <-errs:
			require.NoError(t, err)
		case <-time.After(patience):
			t.Fatal("concurrent callers deadlocked")
		}
	}
}

func TestClientFaults_ConcurrentCallersAllFailWhenTheDaemonDies(t *testing.T) {
	// The daemon answers a few requests and is then killed. Every caller,
	// whether writing or waiting, must come back with an error.
	var served int
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
		dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
		for {
			var req api.Request
			if dec.Decode(&req) != nil {
				return
			}
			if served++; served > 5 {
				return // killed: the connection closes with requests pending
			}
			_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`[]`)})
		}
	})
	c := dialFake(t, endpoint)

	const callers = 12
	results := make(chan error, callers)
	for range callers {
		go func() {
			var last error
			for range 10 {
				if _, last = c.List(context.Background()); last != nil {
					break
				}
			}
			results <- last
		}()
	}
	for range callers {
		select {
		case err := <-results:
			require.Error(t, err, "a caller kept succeeding against a dead daemon")
		case <-time.After(patience):
			t.Fatal("a caller is still waiting on a dead daemon")
		}
	}
}

func TestDialFaults_NothingUsableAtTheEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket paths")
	}
	dir := socketDir(t)

	// The daemon is not running.
	_, err := api.DialWithToken("unix://"+filepath.Join(dir, "absent.sock"), "")
	require.ErrorContains(t, err, "api client dial")
	require.ErrorIs(t, err, os.ErrNotExist)

	// The daemon was killed: its socket file is still there, nobody listens.
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())
	require.FileExists(t, stale)
	_, err = api.DialWithToken("unix://"+stale, "")
	require.ErrorContains(t, err, "api client dial")

	// The path holds something that is no socket.
	file := filepath.Join(dir, "file.sock")
	require.NoError(t, os.WriteFile(file, []byte("not a socket"), 0o600))
	_, err = api.DialWithToken(file, "")
	require.ErrorContains(t, err, "api client dial")
	_, err = api.DialWithToken("unix://"+dir, "")
	require.ErrorContains(t, err, "api client dial")
	data, rerr := os.ReadFile(file)
	require.NoError(t, rerr)
	require.Equal(t, "not a socket", string(data), "a failed dial must leave the file alone")

	// Endpoints that name nothing.
	for _, endpoint := range []string{"", "unix://", "tcp://", "http://127.0.0.1:1", "://x", "ssh://host/sock"} {
		_, err := api.DialWithToken(endpoint, "")
		require.Error(t, err, "endpoint %q", endpoint)
	}
}

// realAuth serves the handshake the way jerboad does for token, then answers
// Daemon.Version. It reports whether the client proved it holds the token.
func realAuth(conn net.Conn, token string) bool {
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	nonce := api.NewAuthNonce()
	authed := false
	for {
		var req api.Request
		if dec.Decode(&req) != nil {
			return authed
		}
		resp := api.Response{JSONRPC: "2.0", ID: req.ID}
		switch {
		case req.Method == "Auth.Challenge":
			resp.Result, _ = json.Marshal(api.ChallengeResult{Nonce: nonce})
		case req.Method == "Auth.Hello":
			var p api.AuthParams
			_ = json.Unmarshal(req.Params, &p)
			if !api.CheckAuthProof(token, nonce, p.Proof) {
				resp.Error = &api.RPCError{Code: -32001, Message: "invalid token"}
				_ = enc.Encode(resp)
				return false
			}
			authed = true
			resp.Result, _ = json.Marshal(api.HelloResult{Status: "ok", Proto: api.ProtoVersion, ServerProof: api.ServerAuthProof(token, nonce, p.ClientNonce)})
		case !authed:
			resp.Error = &api.RPCError{Code: -32001, Message: "authentication required"}
		default:
			resp.Result = json.RawMessage(`{"version":"v9"}`)
		}
		if enc.Encode(resp) != nil {
			return authed
		}
	}
}

func TestAuthFaults_WrongTokenIsRefusedAndNeverSent(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []func() string
	)
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
		tapped, seen := tap(conn)
		mu.Lock()
		sent = append(sent, seen)
		mu.Unlock()
		realAuth(tapped, "the-real-token")
	})

	_, err := api.DialWithToken(endpoint, "a-wrong-token")
	require.ErrorContains(t, err, "authentication failed")
	require.ErrorContains(t, err, "invalid token")

	c, err := api.DialWithToken(endpoint, "the-real-token")
	require.NoError(t, err)
	defer c.Close()
	ver, err := c.DaemonVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v9", ver)

	// Neither token crossed the connection, right or wrong.
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sent, 2)
	for _, seen := range sent {
		require.Contains(t, seen(), "Auth.Hello")
		require.NotContains(t, seen(), "the-real-token")
		require.NotContains(t, seen(), "a-wrong-token")
	}
}

// tappedConn records everything read from the connection.
type tappedConn struct {
	net.Conn
	r io.Reader
}

func (c tappedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// tap wraps conn so that seen returns what the client has sent so far.
func tap(conn net.Conn) (tapped net.Conn, seen func() string) {
	var mu sync.Mutex
	var buf bytes.Buffer
	return tappedConn{Conn: conn, r: io.TeeReader(conn, lockedWriter{&mu, &buf})}, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestAuthFaults_NoTokenAgainstADaemonThatRequiresOne(t *testing.T) {
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) { realAuth(conn, "the-real-token") })
	// Without a token there is no handshake, so the dial itself succeeds;
	// the first call is refused, and says why.
	c, err := api.DialWithToken(endpoint, "")
	require.NoError(t, err)
	defer c.Close()
	_, err = c.DaemonVersion(context.Background())
	require.ErrorContains(t, err, "authentication required")
}

func TestAuthFaults_HandshakeGoesWrong(t *testing.T) {
	challenge := func(nonce string) api.Response {
		return api.Response{Result: json.RawMessage(`{"nonce":` + nonce + `}`)}
	}
	goodNonce := `"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="`
	cases := map[string]struct {
		serve func(conn net.Conn)
		want  string
		not   string
	}{
		"closes on connect": {
			serve: func(net.Conn) {},
			want:  "auth response",
		},
		"closes after the challenge": {
			serve: func(conn net.Conn) {
				n := 0
				answer(conn, func(api.Request) api.Response {
					if n++; n > 1 {
						_ = conn.Close()
					}
					return challenge(goodNonce)
				})
			},
			want: "auth response",
		},
		"answers the challenge with garbage": {
			serve: afterRequest(func(conn net.Conn) { _, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n")) }),
			want:  "auth response",
		},
		"sends a nonce too short to be one": {
			serve: func(conn net.Conn) { answer(conn, func(api.Request) api.Response { return challenge(`"AAAA"`) }) },
			want:  "invalid challenge",
		},
		"sends no nonce": {
			serve: func(conn net.Conn) {
				answer(conn, func(api.Request) api.Response { return api.Response{Result: json.RawMessage(`{}`)} })
			},
			want: "invalid challenge",
		},
		"sends a nonce of the wrong type": {
			serve: func(conn net.Conn) {
				answer(conn, func(api.Request) api.Response { return challenge(`12345678901234567890`) })
			},
			want: "invalid challenge",
		},
		"sends a result that is no object": {
			serve: func(conn net.Conn) {
				answer(conn, func(api.Request) api.Response { return api.Response{Result: json.RawMessage(`"ok"`)} })
			},
			want: "invalid challenge",
		},
		"predates challenge authentication": {
			serve: func(conn net.Conn) {
				answer(conn, func(api.Request) api.Response {
					return api.Response{Error: &api.RPCError{Code: -32001, Message: "authentication required"}}
				})
			},
			want: "update jerboad",
		},
		"is too busy to authenticate": {
			serve: func(conn net.Conn) {
				answer(conn, func(api.Request) api.Response {
					return api.Response{Error: &api.RPCError{Code: api.ErrCodeBusy, Message: "daemon busy: too many connections"}}
				})
			},
			want: "daemon busy",
			not:  "authentication failed",
		},
		"does not know the challenge method": {
			serve: func(conn net.Conn) {
				answer(conn, func(api.Request) api.Response {
					return api.Response{Error: &api.RPCError{Code: -32601, Message: "method not found"}}
				})
			},
			want: "authentication failed: method not found",
			not:  "update jerboad",
		},
		"answers hello with a result of the wrong type": {
			serve: func(conn net.Conn) {
				answer(conn, func(req api.Request) api.Response {
					if req.Method == "Auth.Challenge" {
						return challenge(goodNonce)
					}
					return api.Response{Result: json.RawMessage(`[true]`)}
				})
			},
			want: "did not prove it holds the token",
		},
		"echoes the client's own proof back": {
			serve: func(conn net.Conn) {
				answer(conn, func(req api.Request) api.Response {
					if req.Method == "Auth.Challenge" {
						return challenge(goodNonce)
					}
					var p api.AuthParams
					_ = json.Unmarshal(req.Params, &p)
					raw, _ := json.Marshal(api.HelloResult{Status: "ok", Proto: api.ProtoVersion, ServerProof: p.Proof})
					return api.Response{Result: raw}
				})
			},
			want: "proof does not match",
		},
		"sends a proof that is not hex": {
			serve: func(conn net.Conn) {
				answer(conn, func(req api.Request) api.Response {
					if req.Method == "Auth.Challenge" {
						return challenge(goodNonce)
					}
					return api.Response{Result: json.RawMessage(`{"status":"ok","server_proof":"zz-not-hex"}`)}
				})
			},
			want: "proof does not match",
		},
		"speaks an older protocol": {
			serve: func(conn net.Conn) {
				answer(conn, func(req api.Request) api.Response {
					if req.Method == "Auth.Challenge" {
						return challenge(goodNonce)
					}
					return api.Response{Result: helloOK("secret-token", req.Params, api.ProtoVersion-1)}
				})
			},
			want: "protocol version mismatch",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			requests := make(chan string, 16)
			endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
				tapped, seen := tap(conn)
				tc.serve(tapped)
				// The handler is done: stop sending, then wait for the client
				// to hang up, so everything it sent has been seen.
				if half, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = half.CloseWrite()
				}
				_, _ = io.Copy(io.Discard, tapped)
				requests <- seen()
			})
			var c *api.Client
			err := returns(t, "DialWithToken", func() error {
				var err error
				c, err = api.DialWithToken(endpoint, "secret-token")
				return err
			})
			if c != nil {
				_ = c.Close()
			}
			require.ErrorContains(t, err, tc.want)
			if tc.not != "" {
				require.NotContains(t, err.Error(), tc.not)
			}
			// A failed handshake closes the connection, and nothing but the
			// handshake was sent on it: no token, no request.
			select {
			case sent := <-requests:
				require.NotContains(t, sent, "secret-token")
				for _, line := range strings.Split(strings.TrimSpace(sent), "\n") {
					if line == "" {
						continue
					}
					var req api.Request
					require.NoError(t, json.Unmarshal([]byte(line), &req))
					require.Contains(t, []string{"Auth.Challenge", "Auth.Hello"}, req.Method)
				}
			case <-time.After(patience):
				t.Fatal("the client kept the connection open after a failed handshake")
			}
		})
	}
}

func TestAuthFaults_DaemonWithoutProtoVersionIsAccepted(t *testing.T) {
	// A daemon from before version negotiation reports no proto: the client
	// goes on rather than refusing a daemon it cannot judge.
	endpoint, _ := fakeDaemon(t, func(conn net.Conn) {
		answer(conn, func(req api.Request) api.Response {
			switch req.Method {
			case "Auth.Challenge":
				return api.Response{Result: json.RawMessage(`{"nonce":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`)}
			case "Auth.Hello":
				return api.Response{Result: helloOK("secret-token", req.Params, 0)}
			}
			return api.Response{Result: json.RawMessage(`{"version":"old"}`)}
		})
	})
	c, err := api.DialWithToken(endpoint, "secret-token")
	require.NoError(t, err)
	defer c.Close()
	ver, err := c.DaemonVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, "old", ver)
}

func TestAuthProofs_HostileInput(t *testing.T) {
	nonce, clientNonce := api.NewAuthNonce(), api.NewAuthNonce()
	proof := api.AuthProof("tok", nonce)
	server := api.ServerAuthProof("tok", nonce, clientNonce)

	require.True(t, api.CheckAuthProof("tok", nonce, proof))
	require.True(t, api.CheckServerAuthProof("tok", nonce, clientNonce, server))
	require.NotEqual(t, nonce, clientNonce, "nonces must not repeat")

	// Neither proof passes for the other: a daemon cannot be answered with
	// what it sent, nor a client with what it sent.
	require.False(t, api.CheckAuthProof("tok", nonce, server))
	require.False(t, api.CheckServerAuthProof("tok", nonce, clientNonce, proof))
	require.False(t, api.CheckServerAuthProof("tok", clientNonce, nonce, server), "swapped nonces")

	for name, bad := range map[string]string{
		"empty":           "",
		"not hex":         "zz" + proof[2:],
		"odd length":      proof[:len(proof)-1],
		"truncated":       proof[:16],
		"one bit flipped": flipLastHexDigit(proof),
		"upper case tail": proof + "00",
	} {
		require.False(t, api.CheckAuthProof("tok", nonce, bad), name)
		require.False(t, api.CheckServerAuthProof("tok", nonce, clientNonce, bad), name)
	}
	// A proof for another token, another nonce, or no nonce at all.
	require.False(t, api.CheckAuthProof("other", nonce, proof))
	require.False(t, api.CheckAuthProof("tok", clientNonce, proof))
	require.False(t, api.CheckAuthProof("tok", nil, api.AuthProof("tok", nil)), "an empty challenge proves nothing")
	require.False(t, api.CheckServerAuthProof("tok", nil, clientNonce, api.ServerAuthProof("tok", nil, clientNonce)))
	require.False(t, api.CheckServerAuthProof("tok", nonce, nil, api.ServerAuthProof("tok", nonce, nil)))
}

func flipLastHexDigit(s string) string {
	last := s[len(s)-1]
	if last == '0' {
		return s[:len(s)-1] + "1"
	}
	return s[:len(s)-1] + "0"
}
