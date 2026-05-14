package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/vm"
	"github.com/stretchr/testify/require"
)

func TestNewHandler(t *testing.T) {
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit 0")
		}
		return exec.Command("true")
	}))
	h := NewHandler(mgr, ":8080", "test")
	require.NotNil(t, h)
}

func TestHandler_Dashboard(t *testing.T) {
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit 0")
		}
		return exec.Command("true")
	}))
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "Uni Dashboard")
	require.Contains(t, w.Body.String(), "No VMs registered")
}

func TestHandler_DashboardRoot(t *testing.T) {
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit 0")
		}
		return exec.Command("true")
	}))
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestHandler_API_VMs(t *testing.T) {
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit 0")
		}
		return exec.Command("true")
	}))
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "[]")
}

func TestHandler_NotFound(t *testing.T) {
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit 0")
		}
		return exec.Command("true")
	}))
	h := NewHandler(mgr, ":8080", "test")

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
}
