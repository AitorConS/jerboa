package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/network"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/AitorConS/unikernel-engine/internal/vm"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		socketPath   string
		qemuBin      string
		registryAddr string
		storePath    string
	)
	root := &cobra.Command{
		Use:     "unid",
		Short:   "Unikernel engine daemon",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), socketPath, qemuBin, registryAddr, storePath)
		},
	}
	root.Flags().StringVar(&socketPath, "socket", defaultSocketPath(),
		"Unix socket path for VM management API")
	root.Flags().StringVar(&qemuBin, "qemu", "qemu-system-x86_64",
		"QEMU binary to use")
	root.Flags().StringVar(&registryAddr, "registry-addr", "",
		"HTTP address for image registry (e.g. :5000); empty disables it")
	root.Flags().StringVar(&storePath, "store", defaultStorePath(),
		"image store root directory")
	return root
}

func serve(ctx context.Context, socketPath, qemuBin, registryAddr, storePath string) error {
	mgr := vm.NewQEMUManager(qemuBin, vm.WithStore(vm.NewFileStore(vmsDir(storePath))))

	netStore, err := network.NewStore(networksDir())
	if err != nil {
		return fmt.Errorf("unid: network store: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	store := mgr.Store()
	if err := store.Restore(); err != nil {
		slog.Warn("unid: failed to restore VMs from disk", "err", err)
	}

	vmSrv, err := api.NewServer(mgr, netStore, socketPath, stop, version)
	if err != nil {
		return fmt.Errorf("unid: vm server: %w", err)
	}

	slog.Info("unid listening", "socket", socketPath, "qemu", qemuBin)

	if registryAddr != "" {
		imgStore, err := image.NewStore(storePath)
		if err != nil {
			return fmt.Errorf("unid: image store: %w", err)
		}
		blobStore, err := ociblob.NewStore(blobsDir())
		if err != nil {
			return fmt.Errorf("unid: blob store: %w", err)
		}
		ociStore, err := registry.NewOCIStore(ociDir())
		if err != nil {
			return fmt.Errorf("unid: OCI store: %w", err)
		}
		regSrv := &http.Server{
			Addr:    registryAddr,
			Handler: registry.NewServer(imgStore, registry.WithBlobStore(blobStore), registry.WithOCIStore(ociStore)).Handler(),
		}
		go func() {
			slog.Info("registry listening", "addr", registryAddr)
			if err := regSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("registry server", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			if err := regSrv.Shutdown(context.Background()); err != nil {
				slog.Warn("registry shutdown", "err", err)
			}
		}()
	}

	if err := vmSrv.Serve(ctx); err != nil {
		return fmt.Errorf("unid serve: %w", err)
	}
	slog.Info("unid shutdown complete")
	return nil
}

func defaultSocketPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.TempDir(), "unid.sock")
	}
	return "/var/run/unid.sock"
}

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/images"
	}
	return home + "/.uni/images"
}

func vmsDir(storePath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/vms"
	}
	return home + "/.uni/vms"
}

func networksDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/networks"
	}
	return home + "/.uni/networks"
}

func blobsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/blobs"
	}
	return home + "/.uni/blobs"
}

func ociDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/oci"
	}
	return home + "/.uni/oci"
}
