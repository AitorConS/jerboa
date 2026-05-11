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
	"strings"
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
		socketPath      string
		qemuBin         string
		registryAddr    string
		registryToken   string
		registryJWT     string
		registryJWTIss  string
		registryJWTAud  string
		registryTLSCert string
		registryTLSKey  string
		storePath       string
	)
	root := &cobra.Command{
		Use:     "unid",
		Short:   "Unikernel engine daemon",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), socketPath, qemuBin, registryAddr, registryToken, registryJWT, registryJWTIss, registryJWTAud, registryTLSCert, registryTLSKey, storePath)
		},
	}
	root.Flags().StringVar(&socketPath, "socket", defaultSocketPath(),
		"Unix socket path for VM management API")
	root.Flags().StringVar(&qemuBin, "qemu", "qemu-system-x86_64",
		"QEMU binary to use")
	root.Flags().StringVar(&registryAddr, "registry-addr", "",
		"HTTP address for image registry (e.g. :5000); empty disables it")
	root.Flags().StringVar(&registryToken, "registry-token", os.Getenv("UNI_REGISTRY_TOKEN"),
		"Optional bearer token for registry auth (or set UNI_REGISTRY_TOKEN)")
	root.Flags().StringVar(&registryJWT, "registry-jwt-secret", os.Getenv("UNI_REGISTRY_JWT_SECRET"),
		"Optional JWT HMAC secret for scoped registry auth (or set UNI_REGISTRY_JWT_SECRET)")
	root.Flags().StringVar(&registryJWTIss, "registry-jwt-issuer", os.Getenv("UNI_REGISTRY_JWT_ISSUER"),
		"Optional expected JWT issuer for registry auth (or set UNI_REGISTRY_JWT_ISSUER)")
	root.Flags().StringVar(&registryJWTAud, "registry-jwt-audience", os.Getenv("UNI_REGISTRY_JWT_AUDIENCE"),
		"Optional expected JWT audience for registry auth (or set UNI_REGISTRY_JWT_AUDIENCE)")
	root.Flags().StringVar(&registryTLSCert, "registry-tls-cert", os.Getenv("UNI_REGISTRY_TLS_CERT"),
		"Optional TLS cert file for registry HTTPS (or set UNI_REGISTRY_TLS_CERT)")
	root.Flags().StringVar(&registryTLSKey, "registry-tls-key", os.Getenv("UNI_REGISTRY_TLS_KEY"),
		"Optional TLS key file for registry HTTPS (or set UNI_REGISTRY_TLS_KEY)")
	root.Flags().StringVar(&storePath, "store", defaultStorePath(),
		"image store root directory")
	return root
}

func serve(ctx context.Context, socketPath, qemuBin, registryAddr, registryToken, registryJWT, registryJWTIss, registryJWTAud, registryTLSCert, registryTLSKey, storePath string) error {
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
		if err := validateRegistryTLSConfig(registryTLSCert, registryTLSKey); err != nil {
			return fmt.Errorf("unid: registry TLS config: %w", err)
		}
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
		opts := []registry.Option{registry.WithBlobStore(blobStore), registry.WithOCIStore(ociStore)}
		if registryToken != "" {
			opts = append(opts, registry.WithBearerToken(registryToken, "uni-registry"))
		}
		if registryJWT != "" {
			opts = append(opts, registry.WithJWTAuth(registryJWT, "uni-registry"))
			opts = append(opts, registry.WithJWTValidation(registryJWTIss, registryJWTAud))
		}
		regSrv := &http.Server{
			Addr:    registryAddr,
			Handler: registry.NewServer(imgStore, opts...).Handler(),
		}
		go func() {
			slog.Info("registry listening", "addr", registryAddr)
			var err error
			if strings.TrimSpace(registryTLSCert) != "" {
				err = regSrv.ListenAndServeTLS(registryTLSCert, registryTLSKey)
			} else {
				err = regSrv.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
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

func validateRegistryTLSConfig(certPath, keyPath string) error {
	cert := strings.TrimSpace(certPath)
	key := strings.TrimSpace(keyPath)
	if cert == "" && key == "" {
		return nil
	}
	if cert == "" || key == "" {
		return fmt.Errorf("both registry TLS cert and key are required")
	}
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
