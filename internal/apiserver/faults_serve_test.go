//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests go through Serve, over a real connection: what a client that
// dies, lies or floods can leave behind in a running daemon, and what a
// daemon finds and repairs when it starts over damaged state.

// requireServing checks that the daemon still answers a new client.
func requireServing(t *testing.T, addr string) {
	t.Helper()
	_, err := dialFault(t, addr).DaemonVersion(context.Background())
	require.NoError(t, err, "the daemon stopped serving")
}

// request encodes one JSON-RPC request line.
func request(t *testing.T, method string, params any) []byte {
	t.Helper()
	raw, err := json.Marshal(api.Request{JSONRPC: "2.0", ID: 1, Method: method, Params: rawParams(t, params)})
	require.NoError(t, err)
	return append(raw, '\n')
}

// hangUpAndWait ends the client's side of conn, which the daemon sees exactly
// as it sees a client that died, and waits for the daemon to finish with the
// connection, so that what the handler left behind can be examined.
func hangUpAndWait(t *testing.T, conn net.Conn) {
	t.Helper()
	tcp, ok := conn.(*net.TCPConn)
	require.True(t, ok)
	require.NoError(t, tcp.CloseWrite())
	_, err := io.Copy(io.Discard, conn)
	require.NoError(t, err, "the daemon kept a connection whose client is gone")
}

// requireEmptyEventually waits for dir to hold nothing: a handler cleans up
// after it notices its client is gone, which is shortly after the client
// hangs up, not before.
func requireEmptyEventually(t *testing.T, dir, what string) {
	t.Helper()
	left := func() []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return []string{err.Error()}
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	require.Eventually(t, func() bool { return len(left()) == 0 }, faultWait, faultTick, "%s left behind", what)
	require.Empty(t, left(), "%s left behind", what)
}

// A client uploading a build context dies at every stage of the upload. No
// image may appear, no extracted context may stay in the temporary directory,
// and the daemon keeps serving.
func TestServeFault_BuildStreamCut(t *testing.T) {
	context := tarOf(t, tarFile{name: "app", data: arm64ELF()}, tarFile{name: "lib/extra.so", data: []byte(strings.Repeat("x", 4096))})
	stream := framed(context, 512)
	// Every cut falls before the end of the archive's last file. A client
	// that dies after the whole archive arrived, with only the end-of-stream
	// marker missing, has delivered a complete context.
	cuts := map[string]int{
		"right after the request": 0,
		"inside a frame header":   2,
		"inside the first frame":  4 + 100,
		"between frames":          4 + 512,
		"half way":                len(stream) / 2,
		"inside the last file":    5040,
	}
	for name, cut := range cuts {
		t.Run(name, func(t *testing.T) {
			tmp := privateTemp(t)
			e := newFaultEnv(t)
			addr := e.served(func(s *Server) { s.EnableImageBuild(okMkfs) })
			conn := rawConn(t, addr)
			_, err := conn.Write(request(t, "Image.Build", api.BuildParams{Name: "demo", Tag: "v1", Program: "app"}))
			require.NoError(t, err)
			_, err = conn.Write(stream[:cut])
			require.NoError(t, err)
			hangUpAndWait(t, conn)

			requireEmptyEventually(t, tmp, "build context")
			list, err := e.imgs.List()
			require.NoError(t, err)
			require.Empty(t, list, "an image was stored from a build whose context never arrived")
			requireServing(t, addr)
		})
	}
}

// A build context that arrives whole but is not usable is refused with one
// error, and leaves nothing behind either.
func TestServeFault_BuildContextUnusable(t *testing.T) {
	contexts := map[string][]byte{
		"not a tar":            []byte(strings.Repeat("this is not a tar archive ", 64)),
		"empty":                {},
		"program missing":      tarOf(t, tarFile{name: "other", data: []byte("x")}),
		"program is not ELF":   tarOf(t, tarFile{name: "app", data: []byte("#!/bin/sh\necho hi\n")}),
		"escapes with dot-dot": tarOf(t, tarFile{name: "../../escape", data: []byte("x")}, tarFile{name: "app", data: arm64ELF()}),
		"truncated tar":        tarOf(t, tarFile{name: "app", data: arm64ELF()})[:300],
	}
	for name, payload := range contexts {
		t.Run(name, func(t *testing.T) {
			tmp := privateTemp(t)
			e := newFaultEnv(t)
			addr := e.served(func(s *Server) { s.EnableImageBuild(okMkfs) })
			conn := rawConn(t, addr)
			_, err := conn.Write(append(request(t, "Image.Build", api.BuildParams{Name: "demo", Tag: "v1", Program: "app"}), framed(payload, 256)...))
			require.NoError(t, err)

			var resp api.Response
			require.NoError(t, json.NewDecoder(conn).Decode(&resp), "the daemon must answer a complete upload")
			require.NotNil(t, resp.Error, "an unusable build context was accepted: %s", resp.Result)
			require.NotContains(t, resp.Error.Message, "runtime error")

			requireEmptyEventually(t, tmp, "build context")
			list, err := e.imgs.List()
			require.NoError(t, err)
			require.Empty(t, list)
			requireServing(t, addr)
		})
	}
}

// A frame header announcing more than a frame may hold is a hostile length:
// the daemon must not allocate or wait for it.
func TestServeFault_OversizedFrame(t *testing.T) {
	for _, method := range []string{"Image.Build", "Volume.Import", "Volume.Seed"} {
		t.Run(method, func(t *testing.T) {
			e := newFaultEnv(t)
			addr := e.served(func(s *Server) { s.EnableImageBuild(okMkfs) })
			conn := rawConn(t, addr)
			params := map[string]any{"name": "demo", "tag": "v1", "program": "app", "label": "demo", "size_bytes": 1 << 20, "volume_name": "demo"}
			_, err := conn.Write(append(request(t, method, params), 0xff, 0xff, 0xff, 0xff, 'x'))
			require.NoError(t, err)
			// Either an error response or a hangup; never a wait for 4 GiB.
			var resp api.Response
			if err := json.NewDecoder(conn).Decode(&resp); err == nil {
				require.NotNil(t, resp.Error)
			}
			list, err := e.vols.List()
			require.NoError(t, err)
			require.Empty(t, list)
			requireServing(t, addr)
		})
	}
}

// A client importing a volume dies during the upload, or sends fewer bytes
// than it announced. No volume may be registered and no partial disk kept.
func TestServeFault_VolumeImportStreamCut(t *testing.T) {
	disk := []byte(strings.Repeat("d", 8192))
	stream := framed(disk, 1024)
	cuts := map[string]int{
		"right after the request": 0,
		"inside a frame":          4 + 300,
		"half way":                len(stream) / 2,
		"before the terminator":   len(stream) - 4,
	}
	for name, cut := range cuts {
		t.Run(name, func(t *testing.T) {
			e := newFaultEnv(t)
			addr := e.served(nil)
			before := listTree(t, e.volRoot)
			conn := rawConn(t, addr)
			_, err := conn.Write(request(t, "Volume.Import", api.VolumeImportParams{Name: "data", Label: "data", SizeBytes: int64(len(disk))}))
			require.NoError(t, err)
			_, err = conn.Write(stream[:cut])
			require.NoError(t, err)
			hangUpAndWait(t, conn)

			require.Eventually(t, func() bool { return assert.ObjectsAreEqual(before, listTree(t, e.volRoot)) }, faultWait, faultTick,
				"a cut import left files in the volume store: %v", listTree(t, e.volRoot))
			list, err := e.vols.List()
			require.NoError(t, err)
			require.Empty(t, list, "a volume was registered from an upload that never finished")
			requireServing(t, addr)
		})
	}

	t.Run("shorter than announced", func(t *testing.T) {
		e := newFaultEnv(t)
		addr := e.served(nil)
		conn := rawConn(t, addr)
		_, err := conn.Write(append(request(t, "Volume.Import", api.VolumeImportParams{Name: "data", Label: "data", SizeBytes: 1 << 20}), framed(disk, 1024)...))
		require.NoError(t, err)
		var resp api.Response
		require.NoError(t, json.NewDecoder(conn).Decode(&resp))
		require.NotNil(t, resp.Error, "a disk shorter than announced was imported")
		list, err := e.vols.List()
		require.NoError(t, err)
		require.Empty(t, list)
	})
}

// Bytes that are not a request, sent by a client that needs no token, must
// cost the daemon nothing but that connection.
func TestServeFault_GarbageOnTheWire(t *testing.T) {
	payloads := map[string]string{
		"not json":          "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n",
		"binary":            "\x00\x01\x02\xff\xfe\xfd",
		"truncated request": `{"jsonrpc":"2.0","id":1,"method":"VM.L`,
		"wrong types":       `{"jsonrpc":2,"id":"one","method":["VM.List"],"params":7}` + "\n",
		"array":             `[{"jsonrpc":"2.0","id":1,"method":"VM.List"}]` + "\n",
		"null":              "null\n",
		"deeply nested":     strings.Repeat("[", 100000) + "\n",
		"no method":         `{"jsonrpc":"2.0","id":1}` + "\n",
		"huge id":           `{"jsonrpc":"2.0","id":99999999999999999999999999,"method":"VM.List"}` + "\n",
	}
	e := newFaultEnv(t)
	addr := e.served(nil)
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			conn := rawConn(t, addr)
			_, err := conn.Write([]byte(payload))
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			requireServing(t, addr)
			require.Empty(t, e.mgr.List())
		})
	}

	// A well-formed request for a method that does not exist is answered,
	// and the connection stays usable for the next request.
	conn := rawConn(t, addr)
	dec := json.NewDecoder(conn)
	_, err := conn.Write(request(t, "VM.Explode", struct{}{}))
	require.NoError(t, err)
	var resp api.Response
	require.NoError(t, dec.Decode(&resp))
	requireRPCError(t, resp.Error, -32601, "VM.Explode")
	_, err = conn.Write(request(t, "VM.List", struct{}{}))
	require.NoError(t, err)
	var next api.Response
	require.NoError(t, dec.Decode(&next))
	require.Nil(t, next.Error)
}

// A daemon that requires a token answers nothing but the handshake to a peer
// that has not authenticated, whatever the peer tries first.
func TestServeFault_NothingBeforeAuthentication(t *testing.T) {
	e := newFaultEnv(t)
	addr := e.served(func(s *Server) {
		s.SetAuthToken("a-long-enough-token-0123456789abcdef")
		s.EnableImageBuild(okMkfs)
	})
	for _, method := range append([]string{"Image.Build", "Volume.Import", "Volume.Seed", "VM.List", "Daemon.Shutdown", "VM.Attach"}, paramMethods...) {
		t.Run(method, func(t *testing.T) {
			conn := rawConn(t, addr)
			_, err := conn.Write(append(request(t, method, map[string]string{"name": "x", "id": "x"}), framed([]byte("payload"), 4)...))
			require.NoError(t, err)
			var resp api.Response
			require.NoError(t, json.NewDecoder(conn).Decode(&resp))
			requireRPCError(t, resp.Error, -32001, "authentication required")
			require.Empty(t, resp.Result)
		})
	}
	require.Empty(t, e.mgr.List())
	vols, err := e.vols.List()
	require.NoError(t, err)
	require.Empty(t, vols)
	require.Empty(t, e.leases())
}

// The lease file of a network was lost (rebuilt empty after a power cut)
// while VMs held addresses on it. At start the daemon gives every registered
// VM its address back, so the next VM cannot be handed one that is in use.
func TestServeFault_LostLeasesAreReclaimedAtStart(t *testing.T) {
	e := newFaultEnv(t)
	held := map[string]bool{}
	for _, name := range []string{"a", "b", "c"} {
		info, rpcErr := e.run(api.RunParams{Name: name, NetworkName: faultNet})
		require.Nil(t, rpcErr)
		v, err := e.mgr.Get(info.ID)
		require.NoError(t, err)
		held[v.Cfg.IPAddress] = true
	}
	require.Len(t, held, 3)

	// Forget every lease but the gateway's, as a rebuilt file would.
	path := filepath.Join(e.netRoot, faultNet, "state.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state map[string]any
	require.NoError(t, json.Unmarshal(data, &state))
	state["allocated_ips"] = []string{faultGateway}
	data, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	require.Empty(t, e.leases())

	e.nets, err = network.NewStore(e.netRoot)
	require.NoError(t, err)
	addr := e.restart()
	require.Equal(t, e.vmAddrs(), e.leases(), "registered VMs did not get their addresses back")

	// New VMs get addresses nobody holds, also when requested concurrently.
	var wg sync.WaitGroup
	fresh := make(chan string, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := api.DialWithToken("tcp://"+addr, "")
			if !assert.NoError(t, err) {
				return
			}
			defer func() { _ = c.Close() }()
			info, err := c.Run(context.Background(), api.RunParams{Name: "new" + string(rune('a'+i)), ImagePath: "/nonexistent.img", NetworkName: faultNet})
			if err != nil {
				return // refused for its image path; what matters is who got an address
			}
			if v, err := e.mgr.Get(info.ID); err == nil {
				fresh <- v.Cfg.IPAddress
			}
		}()
	}
	wg.Wait()
	close(fresh)
	seen := map[string]bool{}
	for ip := range fresh {
		require.False(t, held[ip], "address %s of a running VM was handed out again", ip)
		require.False(t, seen[ip], "address %s was handed out twice", ip)
		seen[ip] = true
	}
	e.requireNoLeak()
}

// A daemon starting over stores damaged in every file it reads at start
// still serves, still lists, and reports what it cannot read as an error of
// that one request.
func TestServeFault_DamagedStoresStillServe(t *testing.T) {
	damage := map[string]func(t *testing.T, e *faultEnv){
		"network lease file truncated": func(t *testing.T, e *faultEnv) {
			require.NoError(t, os.WriteFile(filepath.Join(e.netRoot, faultNet, "state.json"), []byte(`{"allocated_ips":["10.1`), 0o600))
		},
		"network lease file is garbage": func(t *testing.T, e *faultEnv) {
			require.NoError(t, os.WriteFile(filepath.Join(e.netRoot, faultNet, "state.json"), []byte("\x00\x00\x00\x00"), 0o600))
		},
		"network directory emptied": func(t *testing.T, e *faultEnv) {
			entries, err := os.ReadDir(filepath.Join(e.netRoot, faultNet))
			require.NoError(t, err)
			for _, entry := range entries {
				require.NoError(t, os.RemoveAll(filepath.Join(e.netRoot, faultNet, entry.Name())))
			}
		},
		"image refs are garbage": func(t *testing.T, e *faultEnv) {
			require.NoError(t, os.WriteFile(filepath.Join(e.imgRoot, "refs.json"), []byte("{not json"), 0o600))
		},
		"volume store holds a stray file": func(t *testing.T, e *faultEnv) {
			require.NoError(t, os.WriteFile(filepath.Join(e.volRoot, "stray"), []byte("x"), 0o600))
			require.NoError(t, os.MkdirAll(filepath.Join(e.volRoot, "empty-dir"), 0o700))
		},
	}
	for name, breakIt := range damage {
		t.Run(name, func(t *testing.T) {
			e := newFaultEnv(t)
			v := e.stoppedVM("web")
			breakIt(t, e)

			addr := e.served(nil)
			c := dialFault(t, addr)
			_, err := c.DaemonVersion(context.Background())
			require.NoError(t, err, "the daemon did not start over a damaged store")
			vms, err := c.List(context.Background())
			require.NoError(t, err)
			require.Len(t, vms, 1)

			// Every listing answers or fails cleanly; none hangs the daemon.
			for _, call := range []func() error{
				func() error { _, err := c.NetworkList(context.Background()); return err },
				func() error { _, err := c.ImageList(context.Background()); return err },
				func() error { _, err := c.VolumeList(context.Background()); return err },
				func() error { _, err := c.Inspect(context.Background(), v.ID); return err },
			} {
				if err := call(); err != nil {
					require.NotContains(t, err.Error(), "runtime error")
					require.NotContains(t, err.Error(), "decode response", "the daemon dropped the connection: %v", err)
				}
			}
			requireServing(t, addr)
		})
	}
}

// Many clients hang up in the middle of a request at once while others work.
// The daemon must serve the well-behaved ones throughout.
func TestServeFault_ClientsVanishingUnderLoad(t *testing.T) {
	e := newFaultEnv(t)
	addr := e.served(nil)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				conn, err := net.Dial("tcp", addr) //nolint:noctx // test client
				if !assert.NoError(t, err) {
					return
				}
				_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"VM.Run","params":{"name":"half`))
				_ = conn.Close()
				return
			}
			c, err := api.DialWithToken("tcp://"+addr, "")
			if !assert.NoError(t, err) {
				return
			}
			defer func() { _ = c.Close() }()
			_, err = c.List(context.Background())
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	require.Empty(t, e.mgr.List(), "half a request created a VM")
	e.requireNoLeak()
	requireServing(t, addr)
}
