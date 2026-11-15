package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fdDaemon stands in for jerboad on a Unix socket. It speaks the daemon's
// JSON-RPC wire format and nothing else, so a test decides, per method and
// per call, what the daemon answers and how the connection dies. It lets the
// commands be exercised against a daemon that misbehaves on every platform,
// without a VM manager behind it.
type fdDaemon struct {
	t        *testing.T
	ln       net.Listener
	endpoint string
	sockPath string

	mu       sync.Mutex
	handlers map[string]fdHandler
	fallback fdHandler
	calls    []fdCall
	seen     map[string]int
	conns    map[net.Conn]struct{}
	received bytes.Buffer

	// token, when set, makes the daemon require the challenge handshake.
	token string
	// auth selects how the handshake goes wrong; empty is a correct daemon.
	auth string

	wg sync.WaitGroup
}

// fdCall is one request the daemon received.
type fdCall struct {
	Method string
	Params json.RawMessage
}

// fdReply is what the daemon does with one request.
type fdReply struct {
	// result is sent as the response's result.
	result any
	// rpcErr is sent as the response's error.
	rpcErr *api.RPCError
	// raw replaces the response envelope with these bytes, verbatim.
	raw []byte
	// silent sends nothing at all.
	silent bool
	// drain reads the framed stream that follows the request before replying,
	// as the daemon does for a build context or a volume import.
	drain bool
	// after is written verbatim once the response has been sent: the console
	// stream that follows an attach response.
	after []byte
	// hangup closes the connection once everything above was written.
	hangup bool
}

// fdHandler answers the nth call (0 is the first) of a method.
type fdHandler func(n int, req api.Request) fdReply

func fdOK(result any) fdHandler {
	return func(int, api.Request) fdReply { return fdReply{result: result} }
}

func fdFail(code int, msg string) fdHandler {
	return func(int, api.Request) fdReply {
		return fdReply{rpcErr: &api.RPCError{Code: code, Message: msg}}
	}
}

// fdHangup closes the connection without answering: the daemon died, or was
// killed, after it read the request.
func fdHangup() fdHandler {
	return func(int, api.Request) fdReply { return fdReply{silent: true, hangup: true} }
}

// fdRaw answers with bytes that are not a response envelope and hangs up.
func fdRaw(raw string) fdHandler {
	return func(int, api.Request) fdReply { return fdReply{raw: []byte(raw), hangup: true} }
}

// fdTruncated answers with the first half of a response: the daemon died
// while it was writing.
func fdTruncated() fdHandler {
	return fdRaw(`{"jsonrpc":"2.0","id":1,"result":{"id":"vm-1","sta`)
}

// fdSeq answers each call with the next handler, and keeps using the last.
func fdSeq(handlers ...fdHandler) fdHandler {
	return func(n int, req api.Request) fdReply {
		if n >= len(handlers) {
			n = len(handlers) - 1
		}
		return handlers[n](n, req)
	}
}

// fdShortDir returns a directory whose path is short enough to hold a Unix
// socket: macOS limits socket paths to about 104 bytes, which the default
// per-test temporary directory already exceeds.
func fdShortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "jb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fdNew starts a fake daemon whose every method is unknown until a handler is
// installed, which is how a daemon older than the CLI answers.
func fdNew(t *testing.T) *fdDaemon {
	t.Helper()
	return fdNewAt(t, filepath.Join(fdShortDir(t), "d.sock"))
}

// fdNewAt starts a fake daemon listening on the given socket path.
func fdNewAt(t *testing.T, sock string) *fdDaemon {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake daemon listens on a Unix socket under /tmp")
	}
	ln, err := net.Listen("unix", sock) //nolint:noctx // test listener
	require.NoError(t, err)
	d := &fdDaemon{
		t:        t,
		ln:       ln,
		endpoint: "unix://" + sock,
		sockPath: sock,
		handlers: map[string]fdHandler{},
		seen:     map[string]int{},
		conns:    map[net.Conn]struct{}{},
	}
	d.fallback = func(_ int, req api.Request) fdReply {
		return fdReply{rpcErr: &api.RPCError{Code: -32601, Message: "method not found: " + req.Method}}
	}
	d.wg.Add(1)
	go d.accept()
	t.Cleanup(d.close)
	return d
}

func (d *fdDaemon) close() {
	_ = d.ln.Close()
	d.mu.Lock()
	for c := range d.conns {
		_ = c.Close()
	}
	d.mu.Unlock()
	d.wg.Wait()
}

// secure makes the daemon require the challenge handshake for token; mode
// selects how the handshake goes wrong, and empty is a correct daemon.
func (d *fdDaemon) secure(token, mode string) *fdDaemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.token, d.auth = token, mode
	return d
}

// on installs the handler for a method.
func (d *fdDaemon) on(method string, h fdHandler) *fdDaemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[method] = h
	return d
}

// all makes every method without a handler of its own answer with h.
func (d *fdDaemon) all(h fdHandler) *fdDaemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fallback = h
	return d
}

// methods lists the methods called so far, in order, the handshake excluded.
func (d *fdDaemon) methods() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.calls))
	for _, c := range d.calls {
		out = append(out, c.Method)
	}
	return out
}

// count returns how many times a method was called.
func (d *fdDaemon) count(method string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen[method]
}

// params decodes the parameters of the nth call of a method into out.
func (d *fdDaemon) params(method string, nth int, out any) {
	d.t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	i := 0
	for _, c := range d.calls {
		if c.Method != method {
			continue
		}
		if i == nth {
			require.NoError(d.t, json.Unmarshal(c.Params, out))
			return
		}
		i++
	}
	require.Failf(d.t, "call not received", "%s call %d; got %d", method, nth, i)
}

// wire returns every byte the daemon read from its clients.
func (d *fdDaemon) wire() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.received.String()
}

func (d *fdDaemon) accept() {
	defer d.wg.Done()
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns[conn] = struct{}{}
		d.mu.Unlock()
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.serve(conn)
			_ = conn.Close()
			d.mu.Lock()
			delete(d.conns, conn)
			d.mu.Unlock()
		}()
	}
}

// fdTap records what is read from a connection.
type fdTap struct {
	d *fdDaemon
	r io.Reader
}

func (t fdTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.d.mu.Lock()
		t.d.received.Write(p[:n])
		t.d.mu.Unlock()
	}
	return n, err //nolint:wrapcheck // transparent reader
}

func (d *fdDaemon) serve(conn net.Conn) {
	dec := json.NewDecoder(fdTap{d: d, r: conn})
	enc := json.NewEncoder(conn)
	d.mu.Lock()
	token, mode := d.token, d.auth
	d.mu.Unlock()
	authed := token == ""
	var nonce []byte
	for {
		var req api.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		if !authed {
			done, ok := fdHandshake(enc, req, token, mode, &nonce)
			if !ok {
				return
			}
			authed = done
			continue
		}
		d.mu.Lock()
		n := d.seen[req.Method]
		d.seen[req.Method] = n + 1
		d.calls = append(d.calls, fdCall{Method: req.Method, Params: req.Params})
		h, found := d.handlers[req.Method]
		if !found {
			h = d.fallback
		}
		d.mu.Unlock()

		rep := h(n, req)
		if rep.drain {
			// The request is newline-terminated; the frames follow it.
			stream := bufio.NewReader(io.MultiReader(dec.Buffered(), fdTap{d: d, r: conn}))
			if _, err := stream.ReadByte(); err != nil {
				return
			}
			if _, err := io.Copy(io.Discard, api.NewFrameReader(stream)); err != nil {
				return
			}
			rep.hangup = true
		}
		switch {
		case rep.silent:
		case rep.raw != nil:
			if _, err := conn.Write(rep.raw); err != nil {
				return
			}
		default:
			resp := api.Response{JSONRPC: "2.0", ID: req.ID, Error: rep.rpcErr}
			if rep.rpcErr == nil {
				raw, err := json.Marshal(rep.result)
				if err != nil {
					return
				}
				resp.Result = raw
			}
			if err := enc.Encode(resp); err != nil {
				return
			}
		}
		if len(rep.after) > 0 {
			if _, err := conn.Write(rep.after); err != nil {
				return
			}
		}
		if rep.hangup {
			return
		}
	}
}

// handshake answers one pre-authentication request. It reports whether the
// connection is now authenticated and whether it should stay open.
func fdHandshake(enc *json.Encoder, req api.Request, token, mode string, nonce *[]byte) (authed, keep bool) {
	refuse := func(code int, msg string) (bool, bool) {
		_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Error: &api.RPCError{Code: code, Message: msg}})
		return false, false
	}
	reply := func(v any) bool {
		raw, err := json.Marshal(v)
		if err != nil {
			return false
		}
		return enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Result: raw}) == nil
	}
	switch mode {
	case "busy":
		return refuse(api.ErrCodeBusy, "daemon busy: too many connections waiting to authenticate; retry shortly")
	case "old":
		// A daemon from before challenge authentication treats the challenge
		// as any other call made without a token.
		return refuse(-32001, "authentication required")
	}
	switch req.Method {
	case "Auth.Challenge":
		*nonce = api.NewAuthNonce()
		if mode == "short-nonce" {
			*nonce = []byte("short")
		}
		return false, reply(api.ChallengeResult{Nonce: *nonce})
	case "Auth.Hello":
		var p api.AuthParams
		if err := json.Unmarshal(req.Params, &p); err != nil || !api.CheckAuthProof(token, *nonce, p.Proof) {
			return refuse(-32001, "invalid token")
		}
		hello := api.HelloResult{
			Status:      "ok",
			Proto:       api.ProtoVersion,
			ServerProof: api.ServerAuthProof(token, *nonce, p.ClientNonce),
		}
		switch mode {
		case "no-proof":
			hello.ServerProof = ""
		case "bad-proof":
			hello.ServerProof = api.ServerAuthProof("some other token", *nonce, p.ClientNonce)
		case "proto":
			hello.Proto = api.ProtoVersion + 41
		}
		ok := reply(hello)
		return ok, ok
	default:
		return refuse(-32001, "authentication required")
	}
}

// fdFrames encodes data as the framed stream the daemon sends after an attach
// response: length-prefixed chunks and a zero-length terminator.
func fdFrames(chunks ...string) []byte {
	var out bytes.Buffer
	for _, c := range chunks {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(c))) //nolint:gosec // test data is small
		out.Write(hdr[:])
		out.WriteString(c)
	}
	out.Write([]byte{0, 0, 0, 0})
	return out.Bytes()
}

// fdResult is the outcome of one CLI invocation.
type fdResult struct {
	// stdout and stderr hold everything the command printed, whether through
	// cobra's writers or straight to the process's streams.
	stdout string
	stderr string
	err    error
	// panicked holds the value of a panic the command raised.
	panicked any
}

// fdIsolate points every per-user location at an empty temporary home, so a
// command under test can neither read nor change the files of whoever runs
// the tests, and clears everything that would choose a daemon for it.
func fdIsolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("JERBOA_HOST", "")
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	config.SetSessionToken("")
	t.Cleanup(func() { config.SetSessionToken("") })
	return home
}

// fdRun runs the CLI against endpoint in an isolated home and captures its
// output and outcome.
func fdRun(t *testing.T, endpoint string, args ...string) fdResult {
	t.Helper()
	fdIsolate(t)
	return fdExec(t, "", append([]string{"--host", endpoint}, args...)...)
}

// fdRunHere is fdRun in the home the test has already isolated, for
// commands that must see what an earlier one left there.
func fdRunHere(t *testing.T, endpoint string, args ...string) fdResult {
	t.Helper()
	return fdExec(t, "", append([]string{"--host", endpoint}, args...)...)
}

// fdExec runs the CLI with exactly args, feeding it stdin, in whatever
// environment the test has prepared.
func fdExec(t *testing.T, stdin string, args ...string) (res fdResult) {
	t.Helper()
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	_, err = inW.WriteString(stdin)
	require.NoError(t, err)
	require.NoError(t, inW.Close())

	oldOut, oldErr, oldIn := os.Stdout, os.Stderr, os.Stdin
	os.Stdout, os.Stderr, os.Stdin = outW, errW, inR
	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&stdout, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&stderr, errR) }()

	func() {
		defer func() {
			if r := recover(); r != nil {
				res.panicked = r
			}
		}()
		root := newRootCmd()
		root.SetOut(outW)
		root.SetErr(errW)
		root.SetIn(inR)
		root.SetArgs(args)
		res.err = root.Execute()
	}()

	os.Stdout, os.Stderr, os.Stdin = oldOut, oldErr, oldIn
	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	_ = outR.Close()
	_ = errR.Close()
	_ = inR.Close()
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

// fdRequireCleanFailure holds a failed command to what a user must see: an
// error, on one line, that is a sentence about the failure and not a crash.
func fdRequireCleanFailure(t *testing.T, res fdResult, want ...string) {
	t.Helper()
	require.Nil(t, res.panicked, "the command panicked: %v", res.panicked)
	require.Error(t, res.err, "stdout: %s", res.stdout)
	fdRequireNoCrashText(t, res)
	msg := res.err.Error()
	assert.NotEmpty(t, strings.TrimSpace(msg))
	assert.NotContains(t, msg, "\n", "the error must fit one line")
	for _, w := range want {
		assert.Contains(t, msg, w)
	}
}

// fdRequireNoCrashText checks that nothing the command printed or returned
// looks like a Go crash.
func fdRequireNoCrashText(t *testing.T, res fdResult) {
	t.Helper()
	require.Nil(t, res.panicked, "the command panicked: %v", res.panicked)
	all := res.stdout + res.stderr
	if res.err != nil {
		all += res.err.Error()
	}
	for _, crash := range []string{"goroutine ", "panic:", "runtime error", "nil pointer", "SIGSEGV", "%!"} {
		assert.NotContains(t, all, crash)
	}
}

// fdRequireJSON checks that the command's standard output is one JSON
// document and returns it decoded.
func fdRequireJSON(t *testing.T, res fdResult) any {
	t.Helper()
	require.Nil(t, res.panicked)
	var v any
	dec := json.NewDecoder(strings.NewReader(res.stdout))
	require.NoError(t, dec.Decode(&v), "stdout is not JSON: %q", res.stdout)
	_, err := dec.Token()
	require.ErrorIs(t, err, io.EOF, "stdout holds more than one JSON document: %q", res.stdout)
	return v
}

// fdString renders a call list for failure messages.
func fdString(calls []string) string { return fmt.Sprintf("%v", calls) }
