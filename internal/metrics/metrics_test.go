package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewCollectorsRegistersMetrics(t *testing.T) {
	c := NewCollectors("test-version")
	require.NotNil(t, c)

	require.NotNil(t, c.VMCreated)
	require.NotNil(t, c.VMRunning)
	require.NotNil(t, c.VMStartsTotal)
	require.NotNil(t, c.BuildInfo)
	require.NotNil(t, c.StartTime)
}

func TestCollectorsHandlerServesMetrics(t *testing.T) {
	c := NewCollectors("test-version")
	c.VMRunning.Set(3)
	c.VMStartsTotal.Add(5)

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "uni_vms_running_total 3")
	require.Contains(t, body, "uni_vm_starts_total 5")
	require.Contains(t, body, "uni_build_info")
}

func TestCollectorsCounters(t *testing.T) {
	c := NewCollectors("test-version")

	c.VMStartsTotal.Add(1)
	c.VMStopsTotal.Add(2)
	c.VMRestartsTotal.Add(1)
	c.VMErrorsTotal.Add(3)

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	require.Contains(t, body, "uni_vm_starts_total 1")
	require.Contains(t, body, "uni_vm_stops_total 2")
	require.Contains(t, body, "uni_vm_restarts_total 1")
	require.Contains(t, body, "uni_vm_errors_total 3")
}

func TestCollectorsBuildInfo(t *testing.T) {
	c := NewCollectors("v0.21.0")

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	require.Contains(t, body, `uni_build_info{version="v0.21.0"} 1`)
}

func TestCollectorsStartTime(t *testing.T) {
	c := NewCollectors("test-version")

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	require.Contains(t, body, "uni_start_time_seconds")
}

func TestCollectorsRegistryGauges(t *testing.T) {
	c := NewCollectors("test-version")

	c.ImagesTotal.Set(7)
	c.PortForwardsActive.Set(12)
	c.BridgeCount.Set(3)

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	require.Contains(t, body, "uni_images_total 7")
	require.Contains(t, body, "uni_port_forwards_active 12")
	require.Contains(t, body, "uni_bridge_count 3")
}

func TestCollectorsPushPullCounters(t *testing.T) {
	c := NewCollectors("test-version")

	c.PushTotal.Add(4)
	c.PullTotal.Add(10)
	c.PushErrorsTotal.Add(1)
	c.PullErrorsTotal.Add(2)

	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	require.Contains(t, body, "uni_push_total 4")
	require.Contains(t, body, "uni_pull_total 10")
	require.Contains(t, body, "uni_push_errors_total 1")
	require.Contains(t, body, "uni_pull_errors_total 2")
}

func TestServeHealthEndpoint(t *testing.T) {
	c := NewCollectors("test-version")
	mux := http.NewServeMux()
	mux.Handle("/metrics", c.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())
}
