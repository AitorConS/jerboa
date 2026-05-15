package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/vm"
	"github.com/stretchr/testify/require"
)

func newTestManager(t *testing.T) vm.Manager {
	t.Helper()
	return vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit 0")
		}
		return exec.Command("true")
	}))
}

func TestNewHandler(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")
	require.NotNil(t, h)
}

func TestHandler_Dashboard(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "Uni Dashboard")
	require.Contains(t, w.Body.String(), "No VMs registered")
}

func TestHandler_DashboardRoot(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestHandler_API_VMs(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "[]")
}

func TestHandler_API_VMDetail_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vm/nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "vm not found", body["error"])
}

func TestHandler_API_VMLogs_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vm/nonexistent/logs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "vm not found", body["error"])
}

func TestHandler_VMDetailPage_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/vm/nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandler_VMDetailPage_WithVM(t *testing.T) {
	mgr := newTestManager(t)
	cfg := vm.Config{ImagePath: "test.img", Memory: "256M", CPUs: 1, Name: "myvm"}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/vm/"+v.ID, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, v.ID)
	require.Contains(t, body, "myvm")
	require.Contains(t, body, "test.img")
	require.Contains(t, body, "Back to Dashboard")
}

func TestHandler_API_VMDetail_WithVM(t *testing.T) {
	mgr := newTestManager(t)
	cfg := vm.Config{ImagePath: "test.img", Memory: "256M", CPUs: 2, Name: "api-vm"}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vm/"+v.ID, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &detail))
	require.Equal(t, v.ID, detail["id"])
	require.Equal(t, "api-vm", detail["name"])
	require.Equal(t, "created", detail["state"])
}

func TestHandler_API_VMLogs_WithVM(t *testing.T) {
	mgr := newTestManager(t)
	cfg := vm.Config{ImagePath: "logtest.img", Memory: "128M", CPUs: 1}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vm/"+v.ID+"/logs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, v.ID, body["id"])
}

func TestHandler_API_VMStats_WithVM(t *testing.T) {
	mgr := newTestManager(t)
	cfg := vm.Config{ImagePath: "statstest.img", Memory: "512M", CPUs: 1}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vm/"+v.ID+"/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, v.ID, body["id"])
	require.Equal(t, "created", body["state"])
	require.Contains(t, body, "cpu_pct")
	require.Contains(t, body, "mem_bytes")
	require.Contains(t, body, "net_rx_bytes")
	require.Contains(t, body, "net_tx_bytes")
}

func TestHandler_API_VMStats_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vm/nonexistent/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "vm not found", body["error"])
}

func TestHandler_VMDetailPage_ContainsStatsSection(t *testing.T) {
	mgr := newTestManager(t)
	cfg := vm.Config{ImagePath: "test.img", Memory: "256M", CPUs: 1, Name: "statsvm"}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/vm/"+v.ID, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, "Live Stats")
	require.Contains(t, body, "stat-cpu")
	require.Contains(t, body, "stat-mem")
}

func TestHandler_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
}
