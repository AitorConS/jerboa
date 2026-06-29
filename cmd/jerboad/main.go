//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/cluster"
	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/metrics"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/service"
	"github.com/AitorConS/jerboa/internal/slogformat"
	"github.com/AitorConS/jerboa/internal/tools"
	"github.com/AitorConS/jerboa/internal/tracing"
	"github.com/AitorConS/jerboa/internal/ui"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
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
		hostFlag      string
		socketFlag    string
		authTokenFlag string
		qemuBin       string
		storePath     string
		vmStoreType   string
		metricsAddr   string
		uiAddr        string
		logFormat     string
		traceAddr     string
		clusterAddr   string
		joinAddrs     string
		hypervisor    string
		fcBin         string
		fcKernelPath  string
		toolsDir      string
		vmLogMaxBytes int64
	)
	root := &cobra.Command{
		Use:     "jerboad",
		Short:   "Unikernel engine daemon",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint := hostFlag
			if endpoint == "" {
				endpoint = socketFlag
			}
			if endpoint == "" {
				endpoint = config.DefaultEndpoint()
			}
			authToken := authTokenFlag
			if authToken == "" {
				authToken = os.Getenv("JERBOA_AUTH_TOKEN")
			}
			vm.SetVMLogMaxBytes(vmLogMaxBytes)
			return serve(cmd.Context(), endpoint, authToken, qemuBin, storePath, vmStoreType, metricsAddr, uiAddr, logFormat, traceAddr, clusterAddr, joinAddrs, hypervisor, fcBin, fcKernelPath, toolsDir)
		},
	}
	root.Flags().StringVarP(&hostFlag, "host", "H", "",
		"daemon listen endpoint (unix:///path or tcp://host:port)")
	root.Flags().StringVar(&socketFlag, "socket", "",
		"Unix socket path for VM management API (deprecated: use --host)")
	_ = root.Flags().MarkDeprecated("socket", "use --host instead")
	root.Flags().StringVar(&authTokenFlag, "auth-token", "",
		"shared secret required from clients via Auth.Hello (env: JERBOA_AUTH_TOKEN); empty disables auth")
	root.Flags().StringVar(&qemuBin, "qemu", "qemu-system-x86_64",
		"QEMU binary to use")
	root.Flags().StringVar(&hypervisor, "hypervisor", "",
		"Hypervisor backend: qemu or firecracker (overrides ~/.jerboa/config.toml)")
	root.Flags().StringVar(&fcBin, "fc-bin", "firecracker",
		"Firecracker binary to use (only with --hypervisor=firecracker)")
	root.Flags().StringVar(&fcKernelPath, "fc-kernel", "",
		"Path to Firecracker-compatible kernel (auto-downloaded if omitted)")
	root.Flags().StringVar(&toolsDir, "tools-dir", "",
		"directory holding the kernel build toolchain (mkfs, boot.img, kernel.img); empty downloads/caches under ~/.jerboa/tools")
	root.Flags().StringVar(&storePath, "store", defaultStorePath(),
		"image store root directory")
	root.Flags().StringVar(&vmStoreType, "vm-store", "file",
		"VM state store backend: file (default) or sqlite")
	root.Flags().Int64Var(&vmLogMaxBytes, "vm-log-max-bytes", 0,
		"max in-memory serial log bytes retained per VM (0 uses the 4 MiB default)")
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
	return root
}

func serve(ctx context.Context, endpoint, authToken, qemuBin, storePath, vmStoreType, metricsAddr, uiAddr, logFormat, traceAddr, clusterAddr, joinAddrs, hypervisor, fcBin, fcKernelPath, toolsDir string) error {
	setupLogger(logFormat)

	// Where the kernel build toolchain (mkfs, boot.img, kernel.img) lives. An
	// explicit --tools-dir wins; otherwise fall back to the cached default,
	// which is downloaded on first use.
	if toolsDir == "" {
		toolsDir = defaultToolsPath()
	}

	vmStore, err := newVMStore(vmStoreType, vmsDir(storePath))
	if err != nil {
		return fmt.Errorf("jerboad: vm store: %w", err)
	}

	// Resolve hypervisor: flag > config file > default "qemu".
	if hypervisor == "" {
		cfg, err := config.Load(config.DefaultPath())
		if err != nil {
			slog.Warn("jerboad: could not read config, defaulting to qemu", "err", err)
		} else {
			hypervisor = cfg.Hypervisor
		}
	}
	if hypervisor == "" {
		hypervisor = "qemu"
	}

	var mgr interface {
		vm.Manager
		Store() vm.Store
	}
	switch hypervisor {
	case "firecracker":
		if fcKernelPath == "" {
			slog.Info("jerboad: ensuring Firecracker kernel is available", "dir", toolsDir)
			dlCtx, dlCancel := context.WithTimeout(ctx, 5*time.Minute)
			defer dlCancel()
			var err error
			fcKernelPath, err = tools.EnsureFCKernel(dlCtx, toolsDir)
			if err != nil {
				return fmt.Errorf("jerboad: download Firecracker kernel: %w", err)
			}
		}
		slog.Info("jerboad: using Firecracker hypervisor", "fc-bin", fcBin, "fc-kernel", fcKernelPath)
		mgr = vm.NewFirecrackerManager(fcBin, fcKernelPath, vm.WithFCStore(vmStore))
	case "qemu":
		mgr = vm.NewQEMUManager(qemuBin, vm.WithStore(vmStore))
	default:
		return fmt.Errorf("jerboad: unknown hypervisor %q (valid: qemu, firecracker)", hypervisor)
	}

	netStore, err := network.NewStore(networksDir())
	if err != nil {
		return fmt.Errorf("jerboad: network store: %w", err)
	}

	svcStore, err := service.NewFileStore(servicesDir())
	if err != nil {
		return fmt.Errorf("jerboad: service store: %w", err)
	}
	svcMgr := service.NewManager(mgr, svcStore)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	traceProvider, err := tracing.NewProvider(ctx, traceAddr, version)
	if err != nil {
		return fmt.Errorf("jerboad: tracing: %w", err)
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
		slog.Warn("jerboad: failed to restore VMs from disk", "err", err)
	}

	var clusterLister apiserver.ClusterMemberLister
	var swimCluster *cluster.SwimCluster
	if clusterAddr != "" {
		swimCluster = cluster.NewSwimCluster(cluster.ParseAddr(clusterAddr), 0, 0, 0)
		mux := http.NewServeMux()
		cluster.RegisterGossipHandler(mux, swimCluster)
		clusterSrv := &http.Server{Addr: clusterAddr, Handler: mux, ReadHeaderTimeout: 30 * time.Second}
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
				slog.Warn("jerboad: cluster join errors", "err", err)
			}
		}
		swimCluster.Start(ctx)
		slog.Info("cluster started", "node_id", swimCluster.LocalID(), "addr", clusterAddr)
		clusterLister = &clusterMemberAdapter{cluster: swimCluster}
	}

	vmSrv, err := apiserver.NewServer(mgr, netStore, svcMgr, endpoint, stop, version, clusterLister)
	if err != nil {
		return fmt.Errorf("jerboad: vm server: %w", err)
	}

	// Require a token on every connection when one is configured. A TCP
	// endpoint without a token is reachable by any local process, so warn.
	if authToken != "" {
		vmSrv.SetAuthToken(authToken)
		slog.Info("jerboad: client authentication enabled")
	} else if strings.HasPrefix(endpoint, "tcp://") {
		slog.Warn("jerboad: TCP endpoint without --auth-token is reachable by any local process; set a token")
	}

	// Attach the daemon's image store so it can resolve image references for
	// VM.Run and serve Image.List/Image.Remove.
	if imgStore, err := image.NewStore(storePath); err != nil {
		slog.Warn("jerboad: image store unavailable", "err", err)
	} else {
		vmSrv.SetImageStore(imgStore)
		// Enable server-side image builds (Image.Build) with a lazy mkfs
		// resolver: the kernel toolchain is downloaded (and cached) on the first
		// build instead of blocking startup.
		vmSrv.EnableImageBuildResolver(func(rctx context.Context) (image.MkfsFunc, error) {
			slog.Info("jerboad: resolving mkfs toolchain for image build", "tools_dir", toolsDir)
			return tools.ResolveMkfs(rctx, toolsDir, "")
		})
		slog.Info("jerboad: image build enabled", "store", storePath)
	}

	// Enable on-demand TFS formatting of attached volumes (lazy mkfs resolver).
	// Volumes are created as sparse files on the client (mkfs is Linux-only);
	// the daemon formats them with a label the first time they are attached.
	vmSrv.EnableVolumeFormatResolver(func(rctx context.Context) (volume.Formatter, error) {
		slog.Info("jerboad: resolving mkfs toolchain for volume format", "tools_dir", toolsDir)
		return tools.ResolveVolumeFormatter(rctx, toolsDir, "")
	})

	slog.Info("jerboad listening", "endpoint", endpoint, "hypervisor", hypervisor)

	if err := vmSrv.Serve(ctx); err != nil {
		return fmt.Errorf("jerboad serve: %w", err)
	}
	slog.Info("jerboad shutdown complete")
	return nil
}

func defaultToolsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".jerboa", "tools")
	}
	return filepath.Join(home, ".jerboa", "tools")
}

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jerboa/images"
	}
	return home + "/.jerboa/images"
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
			slog.Warn("jerboad: file-to-sqlite migration errors", "err", err)
		}
		return sqliteStore, nil
	case "file", "":
		return vm.NewFileStore(dir), nil
	default:
		return nil, fmt.Errorf("unknown vm-store backend %q (use file or sqlite)", storeType)
	}
}

func vmsDir(_ string) string { //nolint:unparam // storePath reserved for configurable store locations
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jerboa/vms"
	}
	return home + "/.jerboa/vms"
}

func networksDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jerboa/networks"
	}
	return home + "/.jerboa/networks"
}

func servicesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jerboa/services"
	}
	return home + "/.jerboa/services"
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

func (a *clusterMemberAdapter) Members() []apiserver.ClusterMember {
	members := a.cluster.Members()
	out := make([]apiserver.ClusterMember, len(members))
	for i, m := range members {
		out[i] = apiserver.ClusterMember{
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
