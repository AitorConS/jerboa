package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The calls that stream a build context, a volume seed or a disk image to the
// daemon, and the console stream coming back. What must hold when either end
// fails mid-stream: the sender reports it, the receiver never takes a partial
// stream for a whole one, and nothing waits forever on a peer that is gone.

// upload is one of the client calls that stream content after their request.
type upload struct {
	name   string
	method string
	result string // a result the daemon may answer with
	call   func(c *api.Client, ctx context.Context, content io.Reader) error
}

var uploads = []upload{
	{
		name: "ImageBuild", method: "Image.Build", result: `{"name":"app","tag":"latest"}`,
		call: func(c *api.Client, ctx context.Context, r io.Reader) error {
			res, err := c.ImageBuild(ctx, api.BuildParams{Name: "app"}, r)
			if err == nil && res.Name != "app" {
				return errors.New("build result lost")
			}
			return err
		},
	},
	{
		name: "VolumeSeed", method: "Volume.Seed", result: `{"disk_path":"/vol/data.img","size_bytes":7}`,
		call: func(c *api.Client, ctx context.Context, r io.Reader) error {
			res, err := c.VolumeSeed(ctx, api.VolumeSeedParams{VolumeName: "data"}, r)
			if err == nil && res.SizeBytes != 7 {
				return errors.New("seed result lost")
			}
			return err
		},
	},
	{
		name: "VolumeImport", method: "Volume.Import", result: `{"id":"data","size_bytes":7}`,
		call: func(c *api.Client, ctx context.Context, r io.Reader) error {
			res, err := c.VolumeImport(ctx, api.VolumeImportParams{Name: "data"}, r)
			if err == nil && res.ID != "data" {
				return errors.New("import result lost")
			}
			return err
		},
	},
}

// received is what the daemon got from one streamed call.
type received struct {
	method  string
	content []byte
	err     error // reading the content failed
}

// uploadDaemon is a daemon that requires token (when not empty) and, for a
// streamed call, hands the connection and the framed content to onUpload.
// Other connections are answered with empty results.
func uploadDaemon(t *testing.T, token string, onUpload func(conn net.Conn, req api.Request, frames io.Reader)) (string, net.Listener) {
	t.Helper()
	return fakeDaemon(t, func(conn net.Conn) {
		dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
		nonce := api.NewAuthNonce()
		authed := token == ""
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
				if !api.CheckAuthProof(token, nonce, p.Proof) {
					resp.Error = &api.RPCError{Code: -32001, Message: "invalid token"}
					_ = enc.Encode(resp)
					return
				}
				authed = true
				resp.Result, _ = json.Marshal(api.HelloResult{Status: "ok", Proto: api.ProtoVersion, ServerProof: api.ServerAuthProof(token, nonce, p.ClientNonce)})
			case "Image.Build", "Volume.Seed", "Volume.Import":
				if !authed {
					resp.Error = &api.RPCError{Code: -32001, Message: "authentication required"}
					_ = enc.Encode(resp)
					return
				}
				// The content follows the request's newline, part of it
				// already inside the decoder's buffer.
				br := bufio.NewReader(io.MultiReader(dec.Buffered(), conn))
				if b, err := br.ReadByte(); err != nil || b != '\n' {
					return
				}
				onUpload(conn, req, api.NewFrameReader(br))
				return
			}
			if enc.Encode(resp) != nil {
				return
			}
		}
	})
}

// acceptUpload reads the whole content, reports it on got and answers result.
func acceptUpload(got chan<- received, result string) func(net.Conn, api.Request, io.Reader) {
	return func(conn net.Conn, req api.Request, frames io.Reader) {
		content, err := io.ReadAll(frames)
		got <- received{method: req.Method, content: content, err: err}
		if err != nil {
			return
		}
		_ = json.NewEncoder(conn).Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(result)})
	}
}

func dialUpload(t *testing.T, endpoint, token string) *api.Client {
	t.Helper()
	c, err := api.DialWithToken(endpoint, token)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func TestUpload_ContentArrivesWhole(t *testing.T) {
	for _, up := range uploads {
		for _, token := range []string{"", "secret-token"} {
			for _, size := range []int{0, 1, 4, 300_000} {
				name := up.name + "/token=" + token + "/" + byteCount(size)
				t.Run(name, func(t *testing.T) {
					got := make(chan received, 1)
					endpoint, _ := uploadDaemon(t, token, acceptUpload(got, up.result))
					c := dialUpload(t, endpoint, token)
					content := randomBytes(t, size)
					// Small contents go through a plain Reader, large ones
					// through one that hands everything over in a single
					// Write: the stream must not depend on which.
					var src io.Reader = bytes.NewReader(content)
					if size < 100 {
						src = struct{ io.Reader }{src}
					}
					err := returns(t, up.name, func() error { return up.call(c, context.Background(), src) })
					require.NoError(t, err)
					rec := <-got
					require.NoError(t, rec.err)
					require.Equal(t, up.method, rec.method)
					require.True(t, bytes.Equal(content, rec.content), "the daemon received %d bytes, %d were sent", len(rec.content), len(content))
				})
			}
		}
	}
}

func byteCount(n int) string {
	if n >= 1000 {
		return "large"
	}
	return string(rune('0'+n)) + "B"
}

// brokenSource serves data and then fails, as a file on a failing disk does.
type brokenSource struct {
	data []byte
	err  error
}

func (b *brokenSource) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func TestUpload_SourceFailsMidStream(t *testing.T) {
	for _, up := range uploads {
		t.Run(up.name, func(t *testing.T) {
			got := make(chan received, 1)
			endpoint, _ := uploadDaemon(t, "", acceptUpload(got, up.result))
			c := dialUpload(t, endpoint, "")

			sent := randomBytes(t, 50_000)
			err := returns(t, up.name, func() error {
				return up.call(c, context.Background(), &brokenSource{data: sent, err: syscall.EIO})
			})
			require.ErrorIs(t, err, syscall.EIO, "the read error that cut the upload must reach the caller")

			// The daemon saw the stream end without its terminator: it must
			// not build an image or seed a volume from half the content.
			select {
			case rec := <-got:
				require.Error(t, rec.err, "the daemon took a cut upload of %d bytes for a complete one", len(rec.content))
				require.True(t, bytes.HasPrefix(sent, rec.content))
			case <-time.After(patience):
				t.Fatal("the daemon is still waiting for an upload its client abandoned")
			}
		})
	}
}

// endless yields zeroes forever and counts how many bytes were taken.
type endless struct{ taken atomic.Int64 }

func (e *endless) Read(p []byte) (int, error) {
	clear(p)
	e.taken.Add(int64(len(p)))
	return len(p), nil
}

func TestUpload_DaemonDiesMidStream(t *testing.T) {
	for _, up := range uploads {
		t.Run(up.name, func(t *testing.T) {
			// The daemon reads a little of the content and is killed.
			endpoint, _ := uploadDaemon(t, "", func(_ net.Conn, _ api.Request, frames io.Reader) {
				_, _ = io.CopyN(io.Discard, frames, 100_000)
			})
			c := dialUpload(t, endpoint, "")
			src := &endless{}
			err := returns(t, up.name, func() error { return up.call(c, context.Background(), src) })
			require.Error(t, err, "an upload to a dead daemon reported success")
			var rpcErr *api.RPCError
			require.NotErrorAs(t, err, &rpcErr)
			// The sender stopped: it did not keep reading a source nobody
			// receives (a few buffers past the cut at most).
			assert.Less(t, src.taken.Load(), int64(64<<20))
		})
	}
}

func TestUpload_DaemonRefusesTheRequest(t *testing.T) {
	for _, up := range uploads {
		t.Run(up.name, func(t *testing.T) {
			// The daemon rejects the request (unknown volume) and discards
			// the content the client is already sending.
			endpoint, _ := uploadDaemon(t, "", func(conn net.Conn, req api.Request, frames io.Reader) {
				_ = json.NewEncoder(conn).Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Error: &api.RPCError{Code: api.CodeNotFound, Message: `volume "data" not found`}})
				_, _ = io.Copy(io.Discard, frames)
			})
			c := dialUpload(t, endpoint, "")
			err := returns(t, up.name, func() error {
				return up.call(c, context.Background(), bytes.NewReader(randomBytes(t, 200_000)))
			})
			require.True(t, api.IsNotFound(err), "the daemon's refusal was lost: %v", err)
			require.ErrorContains(t, err, `volume "data" not found`)
		})
	}
}

func TestUpload_DaemonAnswersBadlyAfterTheContent(t *testing.T) {
	answers := map[string]string{
		"nothing":                "",
		"half a response":        `{"jsonrpc":"2.0","id":1,"resu`,
		"garbage":                "\x00\x01\x02 not json\n",
		"a result of wrong type": `{"jsonrpc":"2.0","id":1,"result":[1,2,3]}` + "\n",
		"a result that is text":  `{"jsonrpc":"2.0","id":1,"result":"done"}` + "\n",
	}
	for _, up := range uploads {
		for name, wire := range answers {
			t.Run(up.name+"/"+name, func(t *testing.T) {
				endpoint, _ := uploadDaemon(t, "", func(conn net.Conn, _ api.Request, frames io.Reader) {
					_, _ = io.Copy(io.Discard, frames)
					_, _ = conn.Write([]byte(wire))
				})
				c := dialUpload(t, endpoint, "")
				err := returns(t, up.name, func() error {
					return up.call(c, context.Background(), bytes.NewReader([]byte("content")))
				})
				require.Error(t, err, "an upload the daemon never confirmed reported success")
				var rpcErr *api.RPCError
				require.NotErrorAs(t, err, &rpcErr)
			})
		}
	}
}

// untouched fails the test if anything reads it.
type untouched struct{ read atomic.Bool }

func (u *untouched) Read([]byte) (int, error) {
	u.read.Store(true)
	return 0, io.EOF
}

func TestUpload_DaemonGoneSinceDial(t *testing.T) {
	for _, up := range uploads {
		t.Run(up.name, func(t *testing.T) {
			endpoint, ln := uploadDaemon(t, "", acceptUpload(make(chan received, 1), up.result))
			c := dialUpload(t, endpoint, "")
			// The daemon stops between the client's dial and its upload,
			// which opens a connection of its own.
			require.NoError(t, ln.Close())
			src := &untouched{}
			err := returns(t, up.name, func() error { return up.call(c, context.Background(), src) })
			require.Error(t, err)
			var netErr *net.OpError
			require.ErrorAs(t, err, &netErr, "the caller must be able to tell the daemon is unreachable")
			require.False(t, src.read.Load(), "content was read with no daemon to send it to")
		})
	}
}

func TestUpload_AuthenticatesItsOwnConnection(t *testing.T) {
	for _, up := range uploads {
		t.Run(up.name, func(t *testing.T) {
			// The client holds a token the daemon does not accept (it was
			// restarted with a new one). The first connection is made without
			// handshake trouble by a daemon that takes the old token.
			got := make(chan received, 1)
			oldEndpoint, oldLn := uploadDaemon(t, "old-token", acceptUpload(got, up.result))
			c := dialUpload(t, oldEndpoint, "old-token")
			if runtime.GOOS == "windows" {
				t.Skip("needs a second daemon on the same Unix socket path")
			}
			path := oldEndpoint[len("unix://"):]
			require.NoError(t, oldLn.Close())
			newLn, err := net.Listen("unix", path)
			require.NoError(t, err)
			defer newLn.Close()
			refused := make(chan string, 1)
			go func() {
				conn, err := newLn.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				tapped, seen := tap(conn)
				realAuth(tapped, "new-token")
				_, _ = io.Copy(io.Discard, tapped)
				refused <- seen()
			}()

			src := &untouched{}
			err = returns(t, up.name, func() error { return up.call(c, context.Background(), src) })
			require.ErrorContains(t, err, "authentication failed")
			require.False(t, src.read.Load(), "content was read for a daemon that refused the client")
			select {
			case sent := <-refused:
				require.NotContains(t, sent, up.method, "the request was sent to a daemon that refused the client")
				require.NotContains(t, sent, "old-token")
			case <-time.After(patience):
				t.Fatal("the client kept the refused connection open")
			}
		})
	}
}

func TestVolumeImport_CancelledWhileTheDaemonStalls(t *testing.T) {
	// The daemon takes the whole disk and then never answers.
	arrived := make(chan struct{})
	endpoint, _ := uploadDaemon(t, "", func(conn net.Conn, _ api.Request, frames io.Reader) {
		_, _ = io.Copy(io.Discard, frames)
		close(arrived)
		_, _ = io.Copy(io.Discard, conn)
	})
	c := dialUpload(t, endpoint, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	content := randomBytes(t, 10_000)
	go func() {
		_, err := c.VolumeImport(ctx, api.VolumeImportParams{Name: "data"}, bytes.NewReader(content))
		done <- err
	}()
	select {
	case <-arrived:
	case <-time.After(patience):
		t.Fatal("the disk never reached the daemon")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(patience):
		t.Fatal("canceling did not release an import the daemon sits on")
	}
}

func TestVolumeImport_CancelledWhileTheDaemonStopsReading(t *testing.T) {
	// The daemon accepts the request and stops reading: the socket buffers
	// fill and the client blocks in a write. Canceling must release it.
	stalled, release := make(chan struct{}), make(chan struct{})
	endpoint, _ := uploadDaemon(t, "", func(net.Conn, api.Request, io.Reader) {
		close(stalled)
		<-release
	})
	t.Cleanup(func() { close(release) })
	c := dialUpload(t, endpoint, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &endless{}
	done := make(chan error, 1)
	go func() {
		_, err := c.VolumeImport(ctx, api.VolumeImportParams{Name: "data"}, src)
		done <- err
	}()
	select {
	case <-stalled:
	case <-time.After(patience):
		t.Fatal("the request never reached the daemon")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(patience):
		t.Fatal("canceling did not release an import blocked on a daemon that stopped reading")
	}
}

func TestVolumeImport_AlreadyCancelled(t *testing.T) {
	endpoint, _ := uploadDaemon(t, "", acceptUpload(make(chan received, 1), `{"id":"data"}`))
	c := dialUpload(t, endpoint, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &untouched{}
	_, err := c.VolumeImport(ctx, api.VolumeImportParams{Name: "data"}, src)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, src.read.Load())
}

func TestVolumeImport_AnswerWithoutAVolume(t *testing.T) {
	// A response with neither result nor error is no imported volume.
	endpoint, _ := uploadDaemon(t, "", func(conn net.Conn, _ api.Request, frames io.Reader) {
		_, _ = io.Copy(io.Discard, frames)
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1}` + "\n"))
	})
	c := dialUpload(t, endpoint, "")
	vol, err := c.VolumeImport(context.Background(), api.VolumeImportParams{Name: "data"}, bytes.NewReader([]byte("disk")))
	require.ErrorContains(t, err, "decode imported volume")
	require.Nil(t, vol)
}

func TestAttachFaults_BadResponses(t *testing.T) {
	ok := `{"jsonrpc":"2.0","id":1,"result":{"framed":true}}`
	cases := map[string]struct {
		wire string
		want string
	}{
		"garbage instead of a response":  {"\x1b[2J\x00console noise", "decode attach response"},
		"connection closed, no response": {"", "decode attach response"},
		"half a response":                {ok[:20], "decode attach response"},
		"closed right after the result":  {ok, "attach stream delimiter"},
		"no newline after the result":    {ok + "X", "attach stream delimiter"},
		"framed is not a boolean":        {`{"jsonrpc":"2.0","id":1,"result":{"framed":"yes"}}` + "\n", "decode attach protocol"},
		"result is not an object":        {`{"jsonrpc":"2.0","id":1,"result":[true]}` + "\n", "decode attach protocol"},
		"terminal status is garbage":     {ok + "\n\x00\x00\x00\x00not json\n", "decode attach terminal status"},
		"terminal status is cut":         {ok + "\n\x00\x00\x00\x00" + `{"done":tr`, "decode attach terminal status"},
		"terminal status of wrong type":  {ok + "\n\x00\x00\x00\x00" + `{"done":"yes"}` + "\n", "decode attach terminal status"},
		"terminal status is null":        {ok + "\n\x00\x00\x00\x00null\n", "without terminal status"},
		"frame header cut":               {ok + "\n\x00\x00", "read attach output"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := attachFromWire(t, []byte(tc.wire), nil, &out)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, out.String(), "nothing the VM printed, nothing to show")
		})
	}
}

func TestAttachFaults_RPCError(t *testing.T) {
	wire, err := json.Marshal(api.Response{JSONRPC: "2.0", ID: 1, Error: &api.RPCError{Code: api.CodeNotFound, Message: `vm "vm-test" not found`}})
	require.NoError(t, err)
	var out bytes.Buffer
	err = attachFromWire(t, append(wire, '\n'), nil, &out)
	require.True(t, api.IsNotFound(err), "got %v", err)
	require.Equal(t, `vm "vm-test" not found`, err.Error())
	require.Empty(t, out.String())
}

func TestAttachFaults_HostileFrameLength(t *testing.T) {
	// After some output the stream carries a length no frame may have (a
	// corrupted stream, or something that is not jerboad). The output so far
	// is kept, the length is refused, nothing is allocated for it.
	wire := framedAttachWire(t, [][]byte{[]byte("boot ok\n")}, nil, false)
	wire = append(wire, 0xff, 0xff, 0xff, 0xff)
	wire = append(wire, "tail"...)
	var out bytes.Buffer
	err := attachFromWire(t, wire, nil, &out)
	require.ErrorIs(t, err, api.ErrFrameTooLarge)
	require.Equal(t, "boot ok\n", out.String())
}

func TestAttachFaults_StreamCutAtEveryOffset(t *testing.T) {
	console := []byte("line one\nline two\n")
	wire := framedAttachWire(t, [][]byte{console[:9], console[9:]}, &api.AttachResult{Done: true}, true)
	head := len(attachResponse(t, json.RawMessage(`{"framed":true}`)))

	// The terminal status ends with a newline the client does not need; a cut
	// before that last byte is the first that may read as a complete stream.
	for cut := head; cut < len(wire)-1; cut++ {
		var out bytes.Buffer
		err := attachFromWire(t, wire[:cut], nil, &out)
		require.Error(t, err, "a console stream cut at byte %d of %d ended as if the VM had stopped cleanly", cut, len(wire))
		require.True(t, bytes.HasPrefix(console, out.Bytes()), "cut at %d printed %q", cut, out.Bytes())
	}
	var out bytes.Buffer
	require.NoError(t, attachFromWire(t, wire, nil, &out))
	require.Equal(t, string(console), out.String())
}

func TestAttachFaults_GuestFailureIsReportedAfterAllOutput(t *testing.T) {
	// The VM crashed: its last output still reaches the user, then the error.
	wire := framedAttachWire(t, [][]byte{[]byte("panic: out of memory\n")}, &api.AttachResult{Done: true, Error: "guest exited with code 137"}, true)
	var out bytes.Buffer
	err := attachFromWire(t, wire, []int{1, 2, 3, 5, 8, 13}, &out)
	require.EqualError(t, err, "guest exited with code 137")
	require.Equal(t, "panic: out of memory\n", out.String())
}

func TestListenFaults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket paths")
	}
	dir := socketDir(t)

	t.Run("stale socket of a killed daemon is replaced", func(t *testing.T) {
		path := filepath.Join(dir, "stale.sock")
		old, err := net.Listen("unix", path)
		require.NoError(t, err)
		old.(*net.UnixListener).SetUnlinkOnClose(false)
		require.NoError(t, old.Close())
		require.FileExists(t, path)

		ln, err := api.Listen("unix://" + path)
		require.NoError(t, err)
		defer ln.Close()
		info, err := os.Lstat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		conn, err := net.Dial("unix", path)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	t.Run("a directory in the way is left alone", func(t *testing.T) {
		path := filepath.Join(dir, "is-a-dir")
		require.NoError(t, os.Mkdir(path, 0o700))
		keep := filepath.Join(path, "keep")
		require.NoError(t, os.WriteFile(keep, []byte("data"), 0o600))
		_, err := api.Listen("unix://" + path)
		require.Error(t, err)
		require.FileExists(t, keep)
	})

	t.Run("missing parent directory", func(t *testing.T) {
		_, err := api.Listen(filepath.Join(dir, "no", "such", "dir", "d.sock"))
		require.ErrorContains(t, err, "api server listen")
		require.NoDirExists(t, filepath.Join(dir, "no"), "Listen must not create directories it was not given")
	})

	t.Run("path longer than a socket address", func(t *testing.T) {
		long := filepath.Join(dir, string(bytes.Repeat([]byte("x"), 200))+".sock")
		_, err := api.Listen("unix://" + long)
		require.ErrorContains(t, err, "api server listen")
		require.NoFileExists(t, long)
	})

	t.Run("tcp port already in use", func(t *testing.T) {
		held, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer held.Close()
		_, err = api.Listen("tcp://" + held.Addr().String())
		require.ErrorIs(t, err, syscall.EADDRINUSE)
		// The port's holder is not disturbed.
		conn, err := net.Dial("tcp", held.Addr().String())
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	t.Run("endpoints that name nothing", func(t *testing.T) {
		for _, endpoint := range []string{"", "unix://", "tcp://", "udp://127.0.0.1:1", "tcp://not a host:port", "tcp://127.0.0.1:99999"} {
			ln, err := api.Listen(endpoint)
			if err == nil {
				_ = ln.Close()
			}
			require.Error(t, err, "endpoint %q", endpoint)
		}
	})

	t.Run("closing the listener releases the path for the next daemon", func(t *testing.T) {
		path := filepath.Join(dir, "again.sock")
		for range 3 {
			ln, err := api.Listen(path)
			require.NoError(t, err)
			conn, err := net.Dial("unix", path)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			require.NoError(t, ln.Close())
		}
	})
}
