//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/tools"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/require"
)

// shutdownGuest writes a marker to the volume without fsync and keeps the file
// open, so only the kernel's shutdown sync can make it durable. On a boot that
// finds the marker it reports it instead.
const shutdownGuest = `package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	const path = "/data/shutdown-marker"
	if b, err := os.ReadFile(path); err == nil {
		fmt.Printf("MARKER_STATE=%s\n", b)
	} else {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			panic(err)
		}
		if _, err := f.WriteString(os.Getenv("MARKER")); err != nil {
			panic(err)
		}
		fmt.Println("MARKER_WRITTEN")
	}
	for {
		time.Sleep(time.Hour)
	}
}
`

// TestFirecrackerStopSyncsVolume verifies that jerboa stop on Linux
// Firecracker shuts the Nanos guest down through the vsock power-down request,
// so data the guest wrote but never synced reaches the volume, and that the
// next boot reads it back.
func TestFirecrackerStopSyncsVolume(t *testing.T) {
	fcBin, kernel := requireFirecracker(t)

	storeDir := t.TempDir()
	imgStore, err := image.NewStore(filepath.Join(storeDir, "images"))
	require.NoError(t, err)

	src := filepath.Join(t.TempDir(), "main.go")
	require.NoError(t, os.WriteFile(src, []byte(shutdownGuest), 0o600))
	guestBin := filepath.Join(t.TempDir(), "shutdown-guest")
	require.NoError(t, buildLinuxBinary(src, guestBin), "build guest binary")

	toolsDir := localToolsDir(t, storeDir)
	mkfsRun, err := tools.ResolveMkfs(context.Background(), toolsDir, "")
	require.NoError(t, err, "resolve mkfs")
	volFmt, err := tools.ResolveVolumeFormatter(context.Background(), toolsDir, "")
	require.NoError(t, err, "resolve volume formatter")

	_, err = image.NewBuilder(imgStore).Build(context.Background(), image.BuildConfig{
		Name:       "fc-shutdown",
		Tag:        "latest",
		BinaryPath: guestBin,
		PkgFiles:   []pkg.File{{GuestPath: "data", IsDir: true}},
		MkfsRun:    mkfsRun,
		Memory:     "256M",
		CPUs:       1,
	})
	require.NoError(t, err, "build guest image")
	_, diskPath, err := imgStore.Get("fc-shutdown:latest")
	require.NoError(t, err)

	volStore, err := volume.NewStore(filepath.Join(storeDir, "volumes"))
	require.NoError(t, err)
	_, err = volStore.Create("fcdata", 256<<20)
	require.NoError(t, err)

	mgr := vm.NewFirecrackerManager(fcBin, kernel, vm.WithFCDiskDir(t.TempDir()))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("jerboad-fc-shutdown-%d.sock", os.Getpid()))
	srv, err := apiserver.NewServer(mgr, netStore, socket, nil, "", nil)
	require.NoError(t, err)
	srv.SetVolumeStore(volStore)
	srv.EnableVolumeFormatResolver(func(context.Context) (volume.Formatter, error) { return volFmt, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	client := dialWithRetry(t, socket)
	defer func() { _ = client.Close() }()

	marker := fmt.Sprintf("fc-shutdown-%d", time.Now().UnixNano())
	run := func() string {
		info, err := client.Run(ctx, api.RunParams{
			ImagePath: diskPath,
			Memory:    "256M",
			CPUs:      1,
			Env:       []string{"MARKER=" + marker},
			Volumes:   []api.VolumeMountSpec{{Name: "fcdata", GuestPath: "/data"}},
		})
		require.NoError(t, err)
		return info.ID
	}
	waitLog := func(id, want string) string {
		var logs string
		ok := assertEventually(func() bool {
			l, err := client.Logs(ctx, id)
			if err != nil {
				return false
			}
			logs = l.Logs
			return strings.Contains(logs, want)
		}, 90*time.Second)
		if !ok {
			t.Fatalf("guest %s never logged %q; console:\n%s", id, want, logs)
		}
		return logs
	}
	stop := func(id string) {
		start := time.Now()
		require.NoError(t, client.Stop(ctx, id, false))
		require.Eventually(t, func() bool {
			g, err := client.Get(ctx, id)
			return err == nil && g.State == "stopped"
		}, 60*time.Second, 100*time.Millisecond)
		t.Logf("stop of %s took %s", id, time.Since(start).Round(time.Millisecond))
	}

	first := run()
	waitLog(first, "MARKER_WRITTEN")
	// Stop at once: the marker sits only in the guest page cache.
	stop(first)
	logs, err := client.Logs(ctx, first)
	require.NoError(t, err)
	require.Contains(t, logs.Logs, "virtio socket: host power-down requested",
		"the guest must take the vsock power-down request instead of being killed; console:\n%s", logs.Logs)
	require.NoError(t, client.Remove(ctx, first))

	second := run()
	waitLog(second, "MARKER_STATE="+marker)
	stop(second)
	require.NoError(t, client.Remove(ctx, second))
}

// requireFirecracker returns the firecracker binary and the in-tree Firecracker
// kernel. Outside CI a missing prerequisite skips the test; on the CI KVM
// runner it fails, so the test cannot pass by not running.
func requireFirecracker(t *testing.T) (string, string) {
	t.Helper()
	missing := func(format string, args ...any) {
		if os.Getenv("CI") != "" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	fcBin, err := exec.LookPath("firecracker")
	if err != nil {
		missing("firecracker not found in PATH")
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		missing("/dev/kvm unavailable: %v", err)
	} else {
		_ = f.Close()
	}
	kernel, err := filepath.Abs(filepath.Join("..", "..", "kernel", "output", "platform", "pc", "bin", "kernel.elf"))
	require.NoError(t, err)
	if _, err := os.Stat(kernel); err != nil {
		missing("in-tree Firecracker kernel missing (run make kernel): %v", err)
	}
	return fcBin, kernel
}

func assertEventually(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
