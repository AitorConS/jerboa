//go:build linux

package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/vm"
)

// Avoid speculative keep-alive connections competing with graceful shutdown.
// Tests own no global default-client connection pool.
var testHTTPClient = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	Timeout:   3 * time.Second,
}

// awaitServerReady polls the always-open /health endpoint until the server is
// accepting connections, or fails the test. It replaces fixed time.Sleep calls,
// which raced the server's startup: on a slow/loaded machine the old tests
// either flaked or (worse) silently skipped their assertions behind `if err == nil`.
func awaitServerReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	// Bind every request to the readiness deadline. A bare http.Get has no
	// request context or client timeout, so a listener that accepts the
	// connection but never sends response headers would block the loop past its
	// deadline instead of failing fast.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	url := "http://" + addr + "/health"
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		require.NoError(t, err)
		resp, err := testHTTPClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become ready within 3s", addr)
}

type mockManager struct {
	vms []*vm.VM
}

func (m *mockManager) Create(_ context.Context, _ vm.Config) (*vm.VM, error) {
	return nil, nil
}

func (m *mockManager) Start(_ context.Context, _ string) error { return nil }

func (m *mockManager) Stop(_ context.Context, _ string) error { return nil }

func (m *mockManager) Kill(_ context.Context, _ string) error { return nil }

func (m *mockManager) Signal(_ context.Context, _ string, _ os.Signal) error {
	return nil
}

func (m *mockManager) Remove(_ context.Context, _ string) error { return nil }

func (m *mockManager) Get(_ string) (*vm.VM, error) { return nil, nil }

func (m *mockManager) List() []*vm.VM { return m.vms }

func makeVM(id string, state vm.State) *vm.VM {
	v := &vm.VM{ID: id, State: state}
	return v
}

func listenAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestServe_StartAndShutdown(t *testing.T) {
	c := NewCollectors("coverage-version")
	addr := listenAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- Serve(ctx, addr, "", c)
	}()

	awaitServerReady(t, addr)

	// /metrics is served. The assertion is unconditional now: previously it was
	// wrapped in `if err == nil`, so a startup-race failure passed silently.
	resp, err := testHTTPClient.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	resp, err = testHTTPClient.Get("http://" + addr + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.Equal(t, "ok", string(body))

	cancel()

	select {
	case err := <-doneCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not shut down in time")
	}
}

func TestServe_TokenAuth(t *testing.T) {
	c := NewCollectors("coverage-version")
	addr := listenAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- Serve(ctx, addr, "s3cret", c)
	}()
	awaitServerReady(t, addr)

	// /metrics without a token is rejected.
	resp, err := testHTTPClient.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// /metrics with the token is served.
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = testHTTPClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// /health stays open for liveness probes.
	resp, err = testHTTPClient.Get("http://" + addr + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	cancel()
	<-doneCh
}

func TestServe_ContextCancellation(t *testing.T) {
	c := NewCollectors("coverage-version")
	addr := listenAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- Serve(ctx, addr, "", c)
	}()

	cancel()

	select {
	case err := <-doneCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not shut down after context cancellation")
	}
}

func TestVMStateUpdater_UpdateCounts(t *testing.T) {
	c := NewCollectors("coverage-version")
	mgr := &mockManager{
		vms: []*vm.VM{
			makeVM("a", vm.StateCreated),
			makeVM("b", vm.StateCreated),
			makeVM("c", vm.StateStarting),
			makeVM("d", vm.StateRunning),
			makeVM("e", vm.StateRunning),
			makeVM("e2", vm.StateRunning),
			makeVM("f", vm.StateStopping),
			makeVM("g", vm.StateStopped),
		},
	}

	u := NewVMStateUpdater(c, mgr, nil, 10*time.Second)
	u.update()

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Contains(t, body, "jerboa_vms_created_total 2")
	require.Contains(t, body, "jerboa_vms_starting_total 1")
	require.Contains(t, body, "jerboa_vms_running_total 3")
	require.Contains(t, body, "jerboa_vms_stopping_total 1")
	require.Contains(t, body, "jerboa_vms_stopped_total 1")
	// Images are polled from the image store, not derived from VM count (8
	// VMs above, 0 images, verifying the two are no longer conflated).
	require.Contains(t, body, "jerboa_images_total 0")
}

func TestVMStateUpdater_ImagesTotal_FromImageStore(t *testing.T) {
	c := NewCollectors("coverage-version")
	mgr := &mockManager{vms: []*vm.VM{makeVM("a", vm.StateRunning)}}

	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)

	makeDisk := func(content string) string {
		f, ferr := os.CreateTemp(t.TempDir(), "disk-*.img")
		require.NoError(t, ferr)
		_, ferr = f.WriteString(content)
		require.NoError(t, ferr)
		require.NoError(t, f.Close())
		return f.Name()
	}

	testManifest := func(name string) image.Manifest {
		return image.Manifest{
			SchemaVersion: image.SchemaVersion,
			Name:          name,
			Tag:           "latest",
			Created:       time.Now().UTC(),
			Config:        image.Config{Memory: "256M", CPUs: 1},
			DiskSize:      1 << 20,
		}
	}
	// Distinct disk content: the store is content-addressable, so two Put
	// calls sharing a digest would dedup to a single image (as intended) and
	// defeat this test's "2 distinct images" assertion.
	require.NoError(t, store.Put("hello", "latest", testManifest("hello"), makeDisk("fake disk content: hello")))
	require.NoError(t, store.Put("world", "latest", testManifest("world"), makeDisk("fake disk content: world")))

	u := NewVMStateUpdater(c, mgr, store, 10*time.Second)
	u.update()

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Contains(t, body, "jerboa_images_total 2")
	// A single VM must not leak into the image count (the bug this guards against).
	require.Contains(t, body, "jerboa_vms_running_total 1")
}

func TestVMStateUpdater_RunStopsOnContextCancel(t *testing.T) {
	c := NewCollectors("coverage-version")
	mgr := &mockManager{vms: []*vm.VM{makeVM("x", vm.StateRunning)}}

	ctx, cancel := context.WithCancel(context.Background())
	u := NewVMStateUpdater(c, mgr, nil, 50*time.Millisecond)

	doneCh := make(chan struct{})
	go func() {
		u.Run(ctx)
		close(doneCh)
	}()

	// Run must stop promptly on cancellation regardless of where it is in its
	// tick loop; no fixed sleep is needed to "let it start" first.
	cancel()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

func TestVMStateUpdater_UnknownState(t *testing.T) {
	c := NewCollectors("coverage-version")
	mgr := &mockManager{
		vms: []*vm.VM{
			makeVM("good", vm.StateRunning),
			{ID: "weird", State: vm.State("zombie")},
		},
	}

	u := NewVMStateUpdater(c, mgr, nil, 10*time.Second)
	u.update()

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()

	require.Contains(t, body, "jerboa_vms_running_total 1")
	require.Contains(t, body, "jerboa_vms_stopped_total 0")
}

func TestVMStateCounts_AtomicOps(t *testing.T) {
	var counts VMStateCounts

	counts.Created.Add(3)
	counts.Starting.Add(1)
	counts.Running.Add(5)
	counts.Stopping.Add(2)
	counts.Stopped.Add(4)

	require.Equal(t, int64(3), counts.Created.Load())
	require.Equal(t, int64(1), counts.Starting.Load())
	require.Equal(t, int64(5), counts.Running.Load())
	require.Equal(t, int64(2), counts.Stopping.Load())
	require.Equal(t, int64(4), counts.Stopped.Load())

	counts.Running.Add(10)
	require.Equal(t, int64(15), counts.Running.Load())

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counts.Created.Add(1)
		}()
	}
	wg.Wait()
	require.Equal(t, int64(103), counts.Created.Load())
}
