package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The parsers and decoders of this package take what a user types on the
// command line and what arrives on the daemon's socket. Whatever the input,
// they must return, never panic, and never accept a value outside what the
// rest of the program relies on.

func TestParseHealthCheck_HostileInput(t *testing.T) {
	good := map[string]HealthCheckSpec{
		"tcp:80":               {Type: "tcp", Port: 80},
		"TCP:65535":            {Type: "tcp", Port: 65535},
		"http:8080":            {Type: "http", Port: 8080},
		"http:8080:/healthz":   {Type: "http", Port: 8080, Path: "/healthz"},
		"http:8080:healthz":    {Type: "http", Port: 8080, Path: "/healthz"},
		"HTTP:1:/a:b?c=d:e":    {Type: "http", Port: 1, Path: "/a:b?c=d:e"},
		"http:8080:":           {Type: "http", Port: 8080, Path: "/"},
		"tcp:80:/ignored-path": {Type: "tcp", Port: 80},
	}
	for spec, want := range good {
		got, err := ParseHealthCheck(spec)
		require.NoError(t, err, spec)
		require.Equal(t, want, got, spec)
	}
	bad := []string{
		"", "tcp", "http", ":", "::", "tcp:", ":80", "tcp:0", "tcp:-1", "tcp:65536", "tcp:99999999999999999999",
		"tcp:eighty", "tcp:80.0", "tcp: 80", "tcp:0x50", "udp:53", "https:443", " tcp:80", "tcp\x00:80",
		"http:/healthz", "tcp:８０",
	}
	for _, spec := range bad {
		got, err := ParseHealthCheck(spec)
		require.Error(t, err, "%q was accepted as %+v", spec, got)
		require.Equal(t, HealthCheckSpec{}, got, spec)
	}
}

func TestParseRestartPolicy_HostileInput(t *testing.T) {
	good := map[string]RestartSpec{
		"never":         {Policy: "never"},
		"always":        {Policy: "always"},
		"ALWAYS":        {Policy: "always"},
		"on-failure":    {Policy: "on-failure"},
		"on-failure:0":  {Policy: "on-failure"},
		"on-failure:5":  {Policy: "on-failure", MaxRetries: 5},
		"On-Failure:12": {Policy: "on-failure", MaxRetries: 12},
	}
	for spec, want := range good {
		got, err := ParseRestartPolicy(spec)
		require.NoError(t, err, spec)
		require.Equal(t, want, got, spec)
	}
	bad := []string{
		"", ":", "always:", "on-failure:-1", "on-failure:many", "on-failure:1:2", "on-failure:1.5",
		"on-failure:99999999999999999999", "unless-stopped", "no", " always", "always ", "on_failure", "never\n",
	}
	for _, spec := range bad {
		got, err := ParseRestartPolicy(spec)
		require.Error(t, err, "%q was accepted as %+v", spec, got)
		require.Equal(t, RestartSpec{}, got, spec)
	}
}

func TestParseVolumeMount_HostileInput(t *testing.T) {
	type mount struct {
		name, guest string
		ro          bool
	}
	good := map[string]mount{
		"data:/var/lib/data":    {"data", "/var/lib/data", false},
		"data:/var/lib/data:ro": {"data", "/var/lib/data", true},
		"data:/var/lib/data:RO": {"data", "/var/lib/data", true},
		"d:/":                   {"d", "/", false},
	}
	for spec, want := range good {
		name, guest, ro, err := ParseVolumeMount(spec)
		require.NoError(t, err, spec)
		require.Equal(t, want, mount{name, guest, ro}, spec)
	}
	bad := []string{
		"", ":", "data", "data:", ":/data", "data:relative", "data:./x", "data:/x:rw", "data:/x:", "data:/x:ro:extra",
		"data:/x:read-only", `C:\vol:/data`, "data::/x", "::", ":::", "data: /x",
	}
	for _, spec := range bad {
		name, guest, ro, err := ParseVolumeMount(spec)
		require.Error(t, err, "%q was accepted as %q at %q", spec, name, guest)
		require.Empty(t, name, spec)
		require.Empty(t, guest, spec)
		require.False(t, ro, spec)
	}
}

func TestParsePortMap_HostileInput(t *testing.T) {
	bad := []string{
		":", "::", ":::", "/", "//", "/tcp", "80/tcp", "80:80/", "80:80/tcp/udp", "80:80/tcp ", "-1:80", "80:-1",
		"+80:80", "80:65536", "65536:80", "0x50:80", "80:80:80", "::1:80:80", "[::1]:80:80", "256.0.0.1:80:80",
		"127.0.0.1::80", "127.0.0.1:80:", ":80:80", "localhost:80:80", "80:8０", "80\x00:80", "1e2:80",
		"99999999999999999999:80", "127.0.0.1:80", "80 80",
	}
	for _, spec := range bad {
		got, err := ParsePortMap(spec)
		require.Error(t, err, "%q was accepted as %+v", spec, got)
		require.Equal(t, PortMapSpec{}, got, spec)
	}

	// One bad spec fails the whole list: no port is published by half.
	got, err := ParsePortMaps([]string{"80:80", "443:443/udp", "0:1", "8080:80"})
	require.Error(t, err)
	require.Nil(t, got)
	require.ErrorContains(t, err, `"0:1"`, "the error must name the spec that is wrong")
}

func TestParseEndpoint_HostileInput(t *testing.T) {
	bad := []string{"", "://", "://x", "unix://", "tcp://", "UNIX:///x", "TCP://h:1", "http://h", "tcp+tls://h:1", " unix:///x", "unix ://x"}
	for _, endpoint := range bad {
		network, address, err := parseEndpoint(endpoint)
		require.Error(t, err, "%q was accepted as %s %q", endpoint, network, address)
		require.Empty(t, network)
		require.Empty(t, address)
	}
	// A path may itself contain "://" after the scheme, and a bare path may
	// be relative; neither is rewritten.
	network, address, err := parseEndpoint("unix:///run/a://b.sock")
	require.NoError(t, err)
	require.Equal(t, "unix", network)
	require.Equal(t, "/run/a://b.sock", address)
	network, address, err = parseEndpoint("./jerboad.sock")
	require.NoError(t, err)
	require.Equal(t, "unix", network)
	require.Equal(t, "./jerboad.sock", address)

	// dialArgs never fails: what does not parse is tried as a socket path,
	// so the dial error names what the user gave.
	network, address = dialArgs("http://h")
	require.Equal(t, "unix", network)
	require.Equal(t, "http://h", address)
	network, address = dialArgs("tcp://127.0.0.1:1")
	require.Equal(t, "tcp", network)
	require.Equal(t, "127.0.0.1:1", address)
}

func FuzzParseEndpoint(f *testing.F) {
	for _, s := range []string{
		"", "unix:///var/run/jerboad.sock", "tcp://127.0.0.1:7890", "/tmp/jerboad.sock", "unix://", "tcp://",
		"http://localhost", "://", "a://b://c", "tcp://[::1]:1", "unix://\x00", "\xff\xfe://x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, endpoint string) {
		network, address, err := parseEndpoint(endpoint)
		if err != nil {
			if network != "" || address != "" {
				t.Fatalf("parseEndpoint(%q) failed and still returned %q %q", endpoint, network, address)
			}
		} else {
			if network != "unix" && network != "tcp" {
				t.Fatalf("parseEndpoint(%q) accepted network %q", endpoint, network)
			}
			if address == "" {
				t.Fatalf("parseEndpoint(%q) accepted an empty address", endpoint)
			}
			if !strings.HasSuffix(endpoint, address) {
				t.Fatalf("parseEndpoint(%q) invented address %q", endpoint, address)
			}
		}
		dn, da := dialArgs(endpoint)
		if dn != "unix" && dn != "tcp" {
			t.Fatalf("dialArgs(%q) = network %q", endpoint, dn)
		}
		if err == nil && (dn != network || da != address) {
			t.Fatalf("dialArgs(%q) = %q %q, parseEndpoint says %q %q", endpoint, dn, da, network, address)
		}
	})
}

func FuzzParseHealthCheck(f *testing.F) {
	for _, s := range []string{
		"tcp:80", "http:8080:/healthz", "http:8080:healthz", "http:80:", "", ":", "tcp:0", "tcp:65536", "udp:53",
		"tcp:-1", "http::/", "tcp:+80", "HTTP:1:\x00", "tcp:99999999999999999999",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		hc, err := ParseHealthCheck(spec)
		if err != nil {
			if hc != (HealthCheckSpec{}) {
				t.Fatalf("ParseHealthCheck(%q) failed and still returned %+v", spec, hc)
			}
			return
		}
		if hc.Type != "tcp" && hc.Type != "http" {
			t.Fatalf("ParseHealthCheck(%q) accepted type %q", spec, hc.Type)
		}
		if hc.Port < 1 || hc.Port > 65535 {
			t.Fatalf("ParseHealthCheck(%q) accepted port %d", spec, hc.Port)
		}
		if hc.Path != "" && (hc.Type != "http" || !strings.HasPrefix(hc.Path, "/")) {
			t.Fatalf("ParseHealthCheck(%q) accepted path %q for %s", spec, hc.Path, hc.Type)
		}
	})
}

func FuzzParseRestartPolicy(f *testing.F) {
	for _, s := range []string{
		"never", "always", "on-failure", "on-failure:3", "on-failure:-1", "always:", "", ":", "ALWAYS:0",
		"on-failure:99999999999999999999", "on-failure:1:2", "always\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		rs, err := ParseRestartPolicy(spec)
		if err != nil {
			if rs != (RestartSpec{}) {
				t.Fatalf("ParseRestartPolicy(%q) failed and still returned %+v", spec, rs)
			}
			return
		}
		if rs.Policy != "never" && rs.Policy != "on-failure" && rs.Policy != "always" {
			t.Fatalf("ParseRestartPolicy(%q) accepted policy %q", spec, rs.Policy)
		}
		if rs.MaxRetries < 0 {
			t.Fatalf("ParseRestartPolicy(%q) accepted %d retries", spec, rs.MaxRetries)
		}
	})
}

func FuzzParseVolumeMount(f *testing.F) {
	for _, s := range []string{
		"data:/var/lib/data", "data:/x:ro", "data:/x:RO", "data:/x:rw", "", ":", "::", ":/x", "data:x", "a:/:ro:ro",
		`C:\vol:/data`, "data:/\x00", "../../etc:/x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		name, guest, ro, err := ParseVolumeMount(spec)
		if err != nil {
			if name != "" || guest != "" || ro {
				t.Fatalf("ParseVolumeMount(%q) failed and still returned %q %q %v", spec, name, guest, ro)
			}
			return
		}
		if name == "" || strings.Contains(name, ":") {
			t.Fatalf("ParseVolumeMount(%q) accepted name %q", spec, name)
		}
		if !strings.HasPrefix(guest, "/") || strings.Contains(guest, ":") {
			t.Fatalf("ParseVolumeMount(%q) accepted guest path %q", spec, guest)
		}
		// The parts account for the whole spec: nothing is dropped silently.
		rebuilt := name + ":" + guest
		if ro {
			if !strings.EqualFold(spec[len(rebuilt):], ":ro") {
				t.Fatalf("ParseVolumeMount(%q) reported read-only without a :ro suffix", spec)
			}
		} else if rebuilt != spec {
			t.Fatalf("ParseVolumeMount(%q) = %q %q, which does not add up to the spec", spec, name, guest)
		}
	})
}

func TestRPCError_HostileResponses(t *testing.T) {
	// Decoding what the daemon sent and rendering its error must not panic
	// whatever the fields hold.
	for _, wire := range []string{
		`{"error":{}}`,
		`{"error":{"code":0,"message":""}}`,
		`{"error":{"code":-9223372036854775808,"message":"min"}}`,
		`{"error":{"code":9223372036854775807,"message":"max"}}`,
		`{"error":{"code":-32050,"message":"\u0000\ufffd\n\r\t"}}`,
		`{"error":null,"result":null}`,
		`{"id":-1,"error":{"message":"` + strings.Repeat("A", 1<<16) + `"}}`,
	} {
		var resp Response
		require.NoError(t, json.Unmarshal([]byte(wire), &resp), wire)
		msg := resp.Error.Error()
		if resp.Error == nil {
			require.Empty(t, msg)
			continue
		}
		require.Contains(t, msg, resp.Error.Message)
		require.False(t, IsNotFound(resp.Error) && resp.Error.Code != CodeNotFound)
	}
	// A code that is no integer is a malformed response, not error code 0.
	for _, wire := range []string{`{"error":{"code":"-32004"}}`, `{"error":{"code":1.5}}`, `{"error":{"code":1e400}}`, `{"error":[]}`} {
		var resp Response
		require.Error(t, json.Unmarshal([]byte(wire), &resp), wire)
	}
}

func FuzzResponseDecode(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"id":"vm"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"not found"}}`,
		`{"error":{"code":-32601,"message":"method not found"}}`,
		`{"error":{}}`, `{"error":null}`, `{"result":null}`, `[]`, `null`, `{`, ``, `{"id":"x"}`,
		`{"error":{"code":1e99}}`, "\x00", `{"result":{"framed":true}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var resp Response
		if json.Unmarshal(data, &resp) != nil {
			return
		}
		msg := resp.Error.Error()
		if resp.Error == nil {
			if msg != "" {
				t.Fatalf("no error rendered as %q", msg)
			}
			return
		}
		if !strings.HasPrefix(msg, resp.Error.Message) {
			t.Fatalf("error %+v rendered as %q, which hides its message", resp.Error, msg)
		}
		app := resp.Error.Code <= -32000 && resp.Error.Code >= -32099
		if app != (msg == resp.Error.Message) {
			t.Fatalf("code %d rendered as %q", resp.Error.Code, msg)
		}
		if IsNotFound(resp.Error) != (resp.Error.Code == CodeNotFound) {
			t.Fatalf("IsNotFound disagrees with code %d", resp.Error.Code)
		}
		// The envelope survives a round trip: a proxy or a log replay does
		// not change which error the client sees.
		again, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		var back Response
		if err := json.Unmarshal(again, &back); err != nil {
			t.Fatalf("decode of re-encoded response: %v", err)
		}
		if back.Error == nil || *back.Error != *resp.Error || back.ID != resp.ID {
			t.Fatalf("round trip changed the response: %+v != %+v", back, resp)
		}
	})
}

func FuzzCheckAuthProof(f *testing.F) {
	nonce := bytes.Repeat([]byte{7}, 32)
	f.Add("tok", nonce, AuthProof("tok", nonce))
	f.Add("tok", nonce, ServerAuthProof("tok", nonce, nonce))
	f.Add("", []byte{}, "")
	f.Add("tok", nonce, "zz")
	f.Add("tok", nonce, strings.ToUpper(AuthProof("tok", nonce)))
	f.Fuzz(func(t *testing.T, token string, nonce []byte, proof string) {
		ok := CheckAuthProof(token, nonce, proof)
		want := len(nonce) > 0 && strings.EqualFold(proof, AuthProof(token, nonce))
		if ok != want {
			t.Fatalf("CheckAuthProof(%q, %x, %q) = %v", token, nonce, proof, ok)
		}
		// A client's proof never passes as the daemon's, whatever nonce the
		// other side picked.
		if CheckServerAuthProof(token, nonce, nonce, AuthProof(token, nonce)) {
			t.Fatal("a client proof was accepted as the daemon's")
		}
		if CheckServerAuthProof(token, nonce, nonce, proof) != (len(nonce) > 0 && strings.EqualFold(proof, ServerAuthProof(token, nonce, nonce))) {
			t.Fatalf("CheckServerAuthProof(%q, %x, %q) is wrong", token, nonce, proof)
		}
	})
}

// scriptedConn is a connection whose peer already said everything it will
// say: reads serve those bytes and then report the peer closed, writes are
// discarded.
type scriptedConn struct {
	r io.Reader
}

func (c *scriptedConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *scriptedConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *scriptedConn) Close() error                     { return nil }
func (c *scriptedConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "local", Net: "unix"} }
func (c *scriptedConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "remote", Net: "unix"} }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

// scriptedClient is a client connected to a daemon that answers with wire.
func scriptedClient(wire []byte) *Client {
	conn := &scriptedConn{r: bytes.NewReader(wire)}
	return &Client{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}
}

func attachSeed(t testing.TB, result string, payloads []string, terminal string) []byte {
	t.Helper()
	var wire bytes.Buffer
	wire.WriteString(`{"jsonrpc":"2.0","id":1,"result":` + result + "}\n")
	fw := NewFrameWriter(&wire)
	for _, p := range payloads {
		if _, err := fw.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	wire.WriteString(terminal)
	return wire.Bytes()
}

// FuzzClientAttach plays arbitrary bytes as the daemon's side of an attach.
// The client must return, and must only report a clean end when the stream
// really carried a terminal status (framed) or was a legacy raw stream.
func FuzzClientAttach(f *testing.F) {
	f.Add(attachSeed(f, `{"framed":true}`, []string{"boot\n", "ok\n"}, `{"done":true}`+"\n"))
	f.Add(attachSeed(f, `{"framed":true}`, []string{"boot\n"}, `{"done":true,"error":"exit 1"}`+"\n"))
	f.Add(attachSeed(f, `{"framed":true}`, nil, `{"done":false}`+"\n"))
	f.Add(attachSeed(f, `{"framed":true}`, []string{"x"}, ""))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\nlegacy console\x00\xff"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"vm not found"}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":{"framed":true}}` + "\n\xff\xff\xff\xff"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":{"framed":true}}`))
	f.Add([]byte{})
	f.Add([]byte("\n\n\n"))

	f.Fuzz(func(t *testing.T, wire []byte) {
		var out bytes.Buffer
		err := scriptedClient(wire).Attach(context.Background(), "vm", &out)
		if out.Len() > len(wire) {
			t.Fatalf("attach printed %d bytes from a %d byte stream", out.Len(), len(wire))
		}
		if err != nil {
			return
		}
		// Success: the response must have been a well-formed non-error one.
		var resp Response
		dec := json.NewDecoder(bytes.NewReader(wire))
		if derr := dec.Decode(&resp); derr != nil || resp.Error != nil {
			t.Fatalf("attach succeeded on a response that is none: %q", wire)
		}
		var protocol struct {
			Framed bool `json:"framed"`
		}
		if len(resp.Result) > 0 {
			_ = json.Unmarshal(resp.Result, &protocol)
		}
		if protocol.Framed && !bytes.Contains(wire, []byte("true")) {
			t.Fatalf("framed attach succeeded without a terminal status: %q", wire)
		}
		// A legacy stream is printed as it came: everything after the response.
		if !protocol.Framed && !bytes.HasSuffix(wire, out.Bytes()) {
			t.Fatalf("attach printed bytes the daemon never sent: %q", out.Bytes())
		}
	})
}

// FuzzClientCall plays arbitrary bytes as the daemon's answers to a series of
// calls on one connection. No call may panic or succeed on a malformed
// answer, and once the stream is broken every later call fails too.
func FuzzClientCall(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":{"id":"vm-1","state":"running"}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"id":"a"},{"id":"b"}]}` + "\n" + `{"id":2,"result":{"version":"v1"}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}` + "\n"))
	f.Add([]byte(`{"result":{"id":42}}`))
	f.Add([]byte(`{"result":null}{"result":null}{"result":null}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"resu`))
	f.Add([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xfe, 0x00})

	f.Fuzz(func(t *testing.T, wire []byte) {
		c := scriptedClient(wire)
		ctx := context.Background()
		calls := []func() error{
			func() error { _, err := c.Get(ctx, "vm-1"); return err },
			func() error { _, err := c.List(ctx); return err },
			func() error { _, err := c.DaemonInfo(ctx); return err },
			func() error { _, err := c.VolumeList(ctx); return err },
			func() error { _, err := c.ImageRecipe(ctx, "app"); return err },
			func() error { return c.Stop(ctx, "vm-1", true) },
		}
		broken := false
		for i, call := range calls {
			err := call()
			var rpcErr *RPCError
			switch {
			case err == nil:
				if broken {
					t.Fatalf("call %d succeeded after the connection broke: %q", i, wire)
				}
			case errors.As(err, &rpcErr):
				if rpcErr == nil {
					t.Fatalf("call %d returned a nil RPC error", i)
				}
			case strings.Contains(err.Error(), "decode response"):
				broken = true
			case !strings.Contains(err.Error(), "unmarshal result"):
				t.Fatalf("call %d failed with an error that explains nothing: %v", i, err)
			}
		}
		// The daemon said only len(wire) bytes: six calls cannot all have
		// been answered by fewer than six values.
		if !broken && len(bytes.TrimSpace(wire)) < len(calls) {
			t.Fatalf("six calls were answered by %d bytes: %q", len(wire), wire)
		}
	})
}

func TestScriptedClient_ReportsTheFirstBrokenAnswerOnly(t *testing.T) {
	// Two good answers, then the daemon is cut inside the third.
	wire := `{"jsonrpc":"2.0","id":1,"result":{"id":"vm-1"}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"result":[{"id":"a"}]}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"result":{"vers`
	c := scriptedClient([]byte(wire))
	ctx := context.Background()

	info, err := c.Get(ctx, "vm-1")
	require.NoError(t, err)
	assert.Equal(t, "vm-1", info.ID)
	list, err := c.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	ver, err := c.DaemonVersion(ctx)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Empty(t, ver)
	_, err = c.Get(ctx, "vm-1")
	require.Error(t, err, "the connection is broken for good")
}
