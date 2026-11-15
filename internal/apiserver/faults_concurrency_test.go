//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Several clients ask for conflicting things at the same moment. Whatever
// order the daemon serves them in, exactly the requests that can succeed do,
// and the address leases match the registered VMs afterwards.

// together runs fn n times at once and returns each call's RPC error.
func together(n int, fn func(i int) *api.RPCError) []*api.RPCError {
	errs := make([]*api.RPCError, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func succeeded(errs []*api.RPCError) int {
	n := 0
	for _, err := range errs {
		if err == nil {
			n++
		}
	}
	return n
}

func TestConcurrentFault_RunWithTheSameName(t *testing.T) {
	e := newFaultEnv(t)
	errs := together(16, func(int) *api.RPCError {
		_, rpcErr := e.s.handleRun(context.Background(), rawParams(t, api.RunParams{Name: "web", ImagePath: "/images/app.img", NetworkName: faultNet}))
		return rpcErr
	})
	require.Equal(t, 1, succeeded(errs), "a name must go to exactly one VM")
	for _, rpcErr := range errs {
		if rpcErr != nil {
			requireRPCError(t, rpcErr, -32000, "already in use")
		}
	}
	require.Len(t, e.mgr.List(), 1)
	e.requireNoLeak()
}

func TestConcurrentFault_RemoveTheSameVM(t *testing.T) {
	e := newFaultEnv(t)
	v := e.stoppedVM("web")
	errs := together(16, func(int) *api.RPCError {
		_, rpcErr := e.s.dispatch(context.Background(), &api.Request{ID: 1, Method: "VM.Remove", Params: rawParams(t, api.IDParams{ID: v.ID})}, nil, nil)
		return rpcErr
	})
	require.Equal(t, 1, succeeded(errs), "a VM can be removed once")
	for _, rpcErr := range errs {
		if rpcErr != nil {
			requireRPCError(t, rpcErr, rpcErr.Code, "")
		}
	}
	require.Empty(t, e.mgr.List())
	require.Empty(t, e.leases(), "the removed VM's address was not released, or was released twice into a negative count")

	// The freed address can be handed out again, once.
	info, rpcErr := e.run(api.RunParams{Name: "next", NetworkName: faultNet})
	require.Nil(t, rpcErr)
	next, err := e.mgr.Get(info.ID)
	require.NoError(t, err)
	require.Equal(t, []string{next.Cfg.IPAddress}, e.leases())
}

// A stopped VM is started and removed at the same moment. Either order is a
// valid outcome; a VM that is both gone and holding an address, or two VMs
// with one name, is not.
func TestConcurrentFault_RemoveRacingStart(t *testing.T) {
	for round := range 40 {
		e := newFaultEnv(t)
		v := e.stoppedVM("web")
		errs := together(2, func(i int) *api.RPCError {
			method := "VM.Start"
			if i == 1 {
				method = "VM.Remove"
			}
			_, rpcErr := e.s.dispatch(context.Background(), &api.Request{ID: 1, Method: method, Params: rawParams(t, api.IDParams{ID: v.ID})}, nil, nil)
			return rpcErr
		})
		vms := e.mgr.List()
		require.LessOrEqual(t, len(vms), 1, "round %d: start and remove left %d VMs (errors: %v)", round, len(vms), errs)
		if errs[0] != nil && errs[1] != nil {
			t.Fatalf("round %d: both the start and the remove of a stopped VM failed: %v / %v", round, errs[0], errs[1])
		}
		if len(vms) == 1 {
			require.Equal(t, "web", vms[0].Cfg.Name)
		}
		e.requireNoLeak()
	}
}

// More VMs are requested at once than the subnet has addresses. Every VM
// that starts has an address of its own; the rest are refused; nothing leaks.
func TestConcurrentFault_RunExhaustingTheSubnet(t *testing.T) {
	e := newFaultEnv(t)
	_, err := e.nets.Create("tiny", "10.200.0.0/29", "bridge")
	require.NoError(t, err)
	errs := together(24, func(i int) *api.RPCError {
		_, rpcErr := e.s.handleRun(context.Background(), rawParams(t, api.RunParams{Name: fmt.Sprintf("vm%d", i), ImagePath: "/images/app.img", NetworkName: "tiny"}))
		return rpcErr
	})
	started := succeeded(errs)
	require.Positive(t, started)
	require.Less(t, started, 8, "a /29 cannot hold %d VMs beside its gateway", started)
	for _, rpcErr := range errs {
		if rpcErr != nil {
			requireRPCError(t, rpcErr, -32000, "")
		}
	}
	addrs := map[string]string{}
	for _, v := range e.mgr.List() {
		require.NotEmpty(t, v.Cfg.IPAddress)
		require.NotEqual(t, "10.200.0.1", v.Cfg.IPAddress, "a VM was given the gateway's address")
		if other, dup := addrs[v.Cfg.IPAddress]; dup {
			t.Fatalf("%s and %s share %s", other, v.Cfg.Name, v.Cfg.IPAddress)
		}
		addrs[v.Cfg.IPAddress] = v.Cfg.Name
	}
	require.Len(t, addrs, started)
	require.Len(t, leasesOfTiny(t, e), started, "the lease file does not match the VMs that started")
}

func leasesOfTiny(t *testing.T, e *faultEnv) []string {
	t.Helper()
	var out []string
	for _, ip := range leasesOf(t, e.netRoot, "tiny") {
		if ip != "10.200.0.1" {
			out = append(out, ip)
		}
	}
	return out
}

// Static addresses: many VMs ask for the same one at once. One gets it.
func TestConcurrentFault_RunWithTheSameStaticAddress(t *testing.T) {
	e := newFaultEnv(t)
	errs := together(12, func(i int) *api.RPCError {
		_, rpcErr := e.s.handleRun(context.Background(), rawParams(t, api.RunParams{
			Name: fmt.Sprintf("vm%d", i), ImagePath: "/images/app.img", NetworkName: faultNet, IPAddress: "10.100.0.50", StaticIP: true,
		}))
		return rpcErr
	})
	require.Equal(t, 1, succeeded(errs), "one address was given to several VMs")
	require.Len(t, e.mgr.List(), 1)
	require.Equal(t, []string{"10.100.0.50"}, e.leases())
	e.requireNoLeak()
}

// --- handshake abuse over real connections ---

// A peer that sends its handshake one byte at a time, never finishing, is
// cut off at the handshake deadline: sending bytes does not extend it.
func TestHandshakeFault_SlowClientIsDropped(t *testing.T) {
	addr := startHandshakeServer(t, "s3cret-token-0123456789abcdef", 300*time.Millisecond)
	conn, err := net.Dial("tcp", addr) //nolint:noctx // test client
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	payload := `{"jsonrpc":"2.0","id":1,"method":"Auth.Challenge","params":` + strings.Repeat(" ", 4096)
	dropped := make(chan struct{})
	go func() {
		defer close(dropped)
		buf := make([]byte, 256)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
	deadline := time.After(faultWait)
	for i := 0; ; i++ {
		select {
		case <-dropped:
			return
		case <-deadline:
			t.Fatal("a peer trickling its handshake was never dropped")
		default:
		}
		// Write errors are expected once the daemon hangs up.
		_, _ = conn.Write([]byte{payload[i%len(payload)]})
	}
}

// Peers that connect and say nothing hold handshake slots only until the
// deadline; a client with the token is then served again.
func TestHandshakeFault_IdlePeersFreeTheirSlots(t *testing.T) {
	const token = "s3cret-token-0123456789abcdef"
	addr := startHandshakeServer(t, token, 300*time.Millisecond)
	for range maxPendingHandshake + 8 {
		conn, err := net.Dial("tcp", addr) //nolint:noctx // test client
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
	}
	require.Eventually(t, func() bool {
		c, err := api.DialWithToken("tcp://"+addr, token)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_, err = c.DaemonVersion(context.Background())
		return err == nil
	}, faultWait, 20*time.Millisecond, "idle peers kept the daemon from accepting a client that holds the token")
}

// Wrong tokens, replayed proofs and requests sent in place of the handshake
// all end with a refusal and a closed connection, and none of them stops the
// next client with the right token.
func TestHandshakeFault_RefusalsCloseTheConnection(t *testing.T) {
	const token = "s3cret-token-0123456789abcdef"
	addr := startHandshakeServer(t, token)
	attempts := map[string]string{
		"wrong legacy token":   `{"jsonrpc":"2.0","id":1,"method":"Auth.Hello","params":{"token":"not-the-token"}}`,
		"empty token":          `{"jsonrpc":"2.0","id":1,"method":"Auth.Hello","params":{"token":""}}`,
		"proof without nonce":  `{"jsonrpc":"2.0","id":1,"method":"Auth.Hello","params":{"proof":"` + api.AuthProof(token, []byte("0123456789abcdef")) + `"}}`,
		"hello without params": `{"jsonrpc":"2.0","id":1,"method":"Auth.Hello"}`,
		"params of wrong type": `{"jsonrpc":"2.0","id":1,"method":"Auth.Hello","params":[1,2]}`,
		"shutdown first":       `{"jsonrpc":"2.0","id":1,"method":"Daemon.Shutdown"}`,
	}
	for name, attempt := range attempts {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr) //nolint:noctx // test client
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.SetDeadline(time.Now().Add(faultWait)))
			_, err = conn.Write([]byte(attempt + "\n"))
			require.NoError(t, err)
			dec := json.NewDecoder(conn)
			var resp api.Response
			require.NoError(t, dec.Decode(&resp))
			require.NotNil(t, resp.Error, "an unauthenticated peer was answered: %s", resp.Result)
			assert.Empty(t, resp.Result)
			assert.NotContains(t, resp.Error.Message, token)

			// The refusal is final: the connection is closed, not left open
			// for another guess.
			_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"VM.List"}` + "\n"))
			var next api.Response
			require.Error(t, dec.Decode(&next), "the daemon kept talking to a peer it refused: %+v", next)
		})
	}
	c, err := api.DialWithToken("tcp://"+addr, token)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = c.DaemonVersion(context.Background())
	require.NoError(t, err)
}
