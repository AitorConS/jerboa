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
	"time"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/AitorConS/unikernel-engine/internal/autotls"
	"github.com/AitorConS/unikernel-engine/internal/cluster"
	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/metrics"
	"github.com/AitorConS/unikernel-engine/internal/network"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/AitorConS/unikernel-engine/internal/slogformat"
	"github.com/AitorConS/unikernel-engine/internal/tracing"
	"github.com/AitorConS/unikernel-engine/internal/ui"
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
		vmStoreType     string
		metricsAddr     string
		uiAddr          string
		logFormat       string
		traceAddr       string
		clusterAddr     string
		joinAddrs       string
	)
	root := &cobra.Command{
		Use:     "unid",
		Short:   "Unikernel engine daemon",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), socketPath, qemuBin, registryAddr, registryToken, registryJWT, registryJWTIss, registryJWTAud, registryTLSCert, registryTLSKey, storePath, vmStoreType, metricsAddr, uiAddr, logFormat, traceAddr, clusterAddr, joinAddrs)
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
	root.Flags().StringVar(&vmStoreType, "vm-store", "file",
		"VM state store backend: file (default) or sqlite")
	root.Flags().StringVar(&metricsAddr, "metrics-addr", "",
		"HTTP address for Prometheus metrics (e.g. :9090); empty disables metrics")
	root.Flags().StringVar(&uiAddr, "ui-addr", "",
		"HTTP address for web dashboard (e.g. :8080); empty disables dashboard")
	root.Flags().StringVar(&logFormat, "log-format", "text",
		"log format: text (default) or json")
	root.Flags().StringVar(&traceAddr, "trace-addr", "",
		"OTLP gRPC address for trace export (e.g. localhost:4317); empty disables tracing")
	root.Flags().StringVar(&clusterAddr, "cluster-addr", "",
		"HTTP address for cluster gossip endpoint (e.g. :7946); empty disables cluster")
	root.Flags().StringVar(&joinAddrs, "join", "",
		"Comma-separated list of seed node addresses to join (e.g. 10.0.0.2:7946,10.0.0.3:7946)")
	root.AddCommand(newRegistryGCCmd())
	return root
}

func newRegistryGCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage collect unreferenced registry blobs",
		RunE: func(_ *cobra.Command, _ []string) error {
			blobStore, err := ociblob.NewStore(blobsDir())
			if err != nil {
				return fmt.Errorf("unid gc: blob store: %w", err)
			}
			ociStore, err := registry.NewOCIStore(ociDir())
			if err != nil {
				return fmt.Errorf("unid gc: OCI store: %w", err)
			}
			result, err := registry.GarbageCollect(blobStore, ociStore)
			if err != nil {
				return fmt.Errorf("unid gc: %w", err)
			}
			slog.Info("registry gc complete", "removed", result.Removed, "kept", result.Kept)
			return nil
		},
	}
	return cmd
}

func serve(ctx context.Context, socketPath, qemuBin, registryAddr, registryToken, registryJWT, registryJWTIss, registryJWTAud, registryTLSCert, registryTLSKey, storePath, vmStoreType, metricsAddr, uiAddr, logFormat, traceAddr, clusterAddr, joinAddrs string) error {
	setupLogger(logFormat)

	vmStore, err := newVMStore(vmStoreType, vmsDir(storePath))
	if err != nil {
		return fmt.Errorf("unid: vm store: %w", err)
	}
	mgr := vm.NewQEMUManager(qemuBin, vm.WithStore(vmStore))

	netStore, err := network.NewStore(networksDir())
	if err != nil {
		return fmt.Errorf("unid: network store: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	traceProvider, err := tracing.NewProvider(ctx, traceAddr, version)
	if err != nil {
		return fmt.Errorf("unid: tracing: %w", err)
	}
	defer func() {
		if err := traceProvider.Shutdown(context.Background()); err != nil {
			slog.Warn("trace provider shutdown", "err", err)
		}
	}()

	collectors := metrics.NewCollectors(version)

	if metricsAddr != "" {
		go func() {
			if err := metrics.Serve(ctx, metricsAddr, collectors); err != nil {
				slog.Error("metrics server", "err", err)
			}
		}()
		go metrics.NewVMStateUpdater(collectors, mgr, 5*time.Second).Run(ctx)
	}

	if uiAddr != "" {
		go func() {
			if err := ui.Serve(ctx, uiAddr, mgr, version); err != nil {
				slog.Error("dashboard server", "err", err)
			}
		}()
	}

	store := mgr.Store()
	if err := store.Restore(); err != nil {
		slog.Warn("unid: failed to restore VMs from disk", "err", err)
	}

	var clusterLister api.ClusterMemberLister

	vmSrv, err := api.NewServer(mgr, netStore, socketPath, stop, version, clusterLister)
	if err != nil {
		return fmt.Errorf("unid: vm server: %w", err)
	}

	slog.Info("unid listening", "socket", socketPath, "qemu", qemuBin)

	var swimCluster *cluster.SwimCluster
	if clusterAddr != "" {
		swimCluster = cluster.NewSwimCluster(cluster.ParseAddr(clusterAddr), 0, 0, 0)
		mux := http.NewServeMux()
		cluster.RegisterGossipHandler(mux, swimCluster)
		clusterSrv := &http.Server{Addr: clusterAddr, Handler: mux}
		go func() {
			slog.Info("cluster gossip listening", "addr", clusterAddr)
			if err := clusterSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("cluster server", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			swimCluster.Leave()
			if err := clusterSrv.Shutdown(context.Background()); err != nil {
				slog.Warn("cluster shutdown", "err", err)
			}
		}()

		if joinAddrs != "" {
			seeds := splitCommaList(joinAddrs)
			if err := swimCluster.Join(ctx, seeds...); err != nil {
				slog.Warn("unid: cluster join errors", "err", err)
			}
		}
		swimCluster.Start(ctx)
		slog.Info("cluster started", "node_id", swimCluster.LocalID(), "addr", clusterAddr)
		clusterLister = &clusterMemberAdapter{cluster: swimCluster}
	}

	if registryAddr != "" {
		if err := validateRegistryTLSConfig(registryTLSCert, registryTLSKey); err != nil {
			return fmt.Errorf("unid: registry TLS config: %w", err)
		}

		useTLS := false
		if strings.TrimSpace(registryTLSCert) != "" {
			useTLS = true
		} else {
			selfSignedCert := filepath.Join(autotls.DefaultCertDir(), "cert.pem")
			selfSignedKey := filepath.Join(autotls.DefaultCertDir(), "key.pem")
			_, err := autotls.EnsureCert(selfSignedCert, selfSignedKey)
			if err != nil {
				return fmt.Errorf("unid: generate self-signed cert: %w", err)
			}
			slog.Info("registry using auto-generated self-signed TLS certificate", "cert", selfSignedCert)
			registryTLSCert = selfSignedCert
			registryTLSKey = selfSignedKey
			useTLS = true
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
			slog.Info("registry listening", "addr", registryAddr, "tls", useTLS)
			var err error
			if useTLS {
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

func newVMStore(storeType, dir string) (vm.Store, error) {
	switch storeType {
	case "sqlite":
		sqliteStore, err := vm.NewSQLiteStore(filepath.Join(dir, "vms.db"))
		if err != nil {
			return nil, fmt.Errorf("sqlite store: %w", err)
		}
		m := vm.NewMigrator(dir, sqliteStore)
		if _, err := m.Migrate(); err != nil {
			slog.Warn("unid: file-to-sqlite migration errors", "err", err)
		}
		return sqliteStore, nil
	case "file", "":
		return vm.NewFileStore(dir), nil
	default:
		return nil, fmt.Errorf("unknown vm-store backend %q (use file or sqlite)", storeType)
	}
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

func setupLogger(format string) {
	switch format {
	case "json":
		slog.SetDefault(slog.New(slogformat.NewJSONHandler(os.Stderr)))
	default:
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))
	}
}

func ociDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/oci"
	}
	return home + "/.uni/oci"
}

func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

type clusterMemberAdapter struct {
	cluster *cluster.SwimCluster
}

func (a *clusterMemberAdapter) Members() []api.ClusterMember {
	members := a.cluster.Members()
	out := make([]api.ClusterMember, len(members))
	for i, m := range members {
		out[i] = api.ClusterMember{
			ID:       m.ID,
			Addr:     m.Addr,
			Status:   string(m.Status),
			VMCount:  m.VMCount,
			CPUCap:   m.CPUCap,
			MemCap:   m.MemCap,
			LastSeen: m.LastSeen,
		}
	}
	return out
}
