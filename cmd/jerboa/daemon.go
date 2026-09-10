package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/release"
	"github.com/AitorConS/jerboa/internal/wslboot"
	"github.com/AitorConS/jerboa/internal/wsldistro"
	"github.com/spf13/cobra"
)

// daemonLaunchUser is the Linux user jerboad runs as inside the dedicated distro.
// root runs privileged (firecracker networking) without any host sudo prompt,
// because the distro is isolated and contains nothing but jerboa.
const daemonLaunchUser = "root"

// newDaemonCmd builds the `jerboa daemon` command group, which manages the
// jerboad daemon hosted in the dedicated jerboa WSL2 distro.
func newDaemonCmd() *cobra.Command {
	if runtime.GOOS == "darwin" {
		c := &cobra.Command{Use: "daemon", Short: "Manage the native macOS ARM64 daemon"}
		c.AddCommand(newDaemonStartCmd(), newDaemonStopCmd(), newDaemonRestartCmd(), newDaemonStatusCmd(), newDaemonLogsCmd())
		return c
	}
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the jerboad daemon running in the dedicated WSL2 distro",
		Long: "Manage the jerboad daemon hosted inside the dedicated jerboa WSL2 distro.\n\n" +
			"`install` provisions a self-contained Linux environment (jerboad + qemu +\n" +
			"firecracker + kernel toolchain) via `wsl --import`, the way Docker Desktop\n" +
			"ships its own distro — so nothing depends on your WSL setup, jerboad being on\n" +
			"PATH, or host sudo. start/stop/restart run the daemon as root inside it.",
	}
	cmd.AddCommand(
		newDaemonInstallCmd(),
		newDaemonReinstallCmd(),
		newDaemonUninstallCmd(),
		newDaemonStartCmd(),
		newDaemonStopCmd(),
		newDaemonRestartCmd(),
		newDaemonStatusCmd(),
		newDaemonLogsCmd(),
	)
	return cmd
}

// daemonOpts are the launch flags shared by start and restart.
type daemonOpts struct {
	hypervisor  string
	metricsAddr string
	uiAddr      string
	traceAddr   string
	logFormat   string
}

func (o *daemonOpts) bind(c *cobra.Command) {
	c.Flags().StringVar(&o.hypervisor, "hypervisor", "",
		"hypervisor to run (qemu or firecracker); defaults to config")
	// Observability passthrough: these enable the managed
	// daemon's metrics/UI/tracing/log-format, which previously required
	// hand-launching jerboad inside the distro. A provided value is persisted so
	// subsequent auto-boots enable the same endpoints.
	c.Flags().StringVar(&o.metricsAddr, "metrics-addr", "",
		"serve Prometheus metrics on this address (e.g. :9090); persisted")
	c.Flags().StringVar(&o.uiAddr, "ui-addr", "",
		"serve the dashboard on this address (e.g. :8080); persisted")
	c.Flags().StringVar(&o.traceAddr, "trace-addr", "",
		"export OTLP traces to this address; persisted")
	c.Flags().StringVar(&o.logFormat, "log-format", "",
		"daemon log format: text or json; persisted")
}

// resolveDaemonConfig assembles the wslboot launch config: the dedicated distro,
// run as root, binding 0.0.0.0 while the client dials loopback. It returns the
// token separately for the daemon-file rendezvous. Provided observability flags
// override and are persisted to config so the auto-boot path enables them too.
func resolveDaemonConfig(o daemonOpts) (wslboot.Config, string, error) {
	token := config.ResolveToken()
	if token == "" {
		t, err := wslboot.LoadOrCreateToken(daemonJSONPath())
		if err != nil {
			return wslboot.Config{}, "", fmt.Errorf("daemon token: %w", err)
		}
		token = t
	}

	cfg, err := config.Load(config.DefaultPath())
	if err != nil || cfg == nil {
		cfg = &config.Config{Hypervisor: "qemu"}
	}

	hyp := o.hypervisor
	if hyp == "" {
		hyp = cfg.Hypervisor
	}

	// A provided observability flag overrides the persisted value and is written
	// back, so a later `jerboa ps` (which auto-boots the daemon) launches it with
	// the same endpoints enabled. An unset flag leaves the persisted value intact.
	dirty := false
	for _, f := range []struct {
		val string
		dst *string
	}{
		{o.metricsAddr, &cfg.Daemon.MetricsAddr},
		{o.uiAddr, &cfg.Daemon.UIAddr},
		{o.traceAddr, &cfg.Daemon.TraceAddr},
		{o.logFormat, &cfg.Daemon.LogFormat},
	} {
		if f.val != "" && f.val != *f.dst {
			*f.dst = f.val
			dirty = true
		}
	}
	if dirty {
		if err := config.Save(config.DefaultPath(), cfg); err != nil {
			return wslboot.Config{}, "", fmt.Errorf("persist daemon observability config: %w", err)
		}
	}

	dial, err := distroDialEndpoint()
	if err != nil {
		return wslboot.Config{}, "", err
	}
	return wslboot.Config{
		Endpoint:       dial,
		ListenEndpoint: distroListenEndpoint(),
		Distro:         wsldistro.Name,
		User:           daemonLaunchUser,
		Token:          token,
		Hypervisor:     hyp,
		Observability:  observabilityFromConfig(cfg),
	}, token, nil
}

func newDaemonInstallCmd() *cobra.Command {
	var (
		rootfs string
		force  bool
	)
	c := &cobra.Command{
		Use:   "install",
		Short: "Provision the dedicated jerboa WSL2 distro",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" {
				return errNotWindows("install")
			}
			// Always ensure the host enables nested virtualization: firecracker
			// needs it for /dev/kvm, and it is the one piece of setup outside the
			// distro image. Doing it here also fixes an already-installed machine.
			ensureNestedVirtualization(cmd)
			exists, err := wsldistro.Exists()
			if err != nil {
				return err
			}
			if exists {
				if !force {
					fmt.Fprintf(cmd.OutOrStdout(), "distro %q already installed (use --force to reimport)\n", wsldistro.Name)
					return nil
				}
			}

			tarPath := rootfs
			if tarPath == "" {
				p, ferr := fetchRootfs(cmd.Context(), cmd)
				if ferr != nil {
					return ferr
				}
				tarPath = p
				defer func() { _ = os.Remove(p) }()
			}

			if err := validateRootfs(tarPath); err != nil {
				return err
			}
			backup := ""
			if exists {
				backup, err = backupDistro(cmd)
				if err != nil {
					return err
				}
				if err := wsldistro.Unregister(); err != nil {
					return err
				}
			}

			fmt.Fprintf(cmd.OutOrStdout(), "importing %q from %s ...\n", wsldistro.Name, tarPath)
			if err := wsldistro.Import(wsldistro.DefaultInstallDir(), tarPath); err != nil {
				return err
			}
			if backup != "" {
				_ = os.Remove(backup)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed. start it with: jerboa daemon start\n")
			return nil
		},
	}
	c.Flags().StringVar(&rootfs, "rootfs", "",
		"path to a jerboa rootfs tarball (default: download the release artifact)")
	c.Flags().BoolVar(&force, "force", false,
		"reimport even if the distro already exists (destroys its data)")
	return c
}

// newDaemonReinstallCmd swaps the distro rootfs for a fresh (or newer) one.
// With --keep-data it preserves images, VMs, networks and volumes across the
// reimport, the non-destructive alternative to `install --force`.
func newDaemonReinstallCmd() *cobra.Command {
	var (
		rootfs   string
		keepData bool
		o        daemonOpts
	)
	c := &cobra.Command{
		Use:   "reinstall",
		Short: "Reimport the distro rootfs, optionally preserving data (--keep-data)",
		Long: "Replace the jerboa distro root filesystem with a fresh (or newer) rootfs.\n\n" +
			"Unlike `install --force` (which destroys everything), `--keep-data` preserves\n" +
			"your images, VMs, networks and volumes across the reimport: they are exported before the\n" +
			"swap and restored afterwards. The kernel toolchain cache is not preserved (it\n" +
			"re-downloads on demand). The daemon is stopped and restarted around the swap.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" {
				return errNotWindows("reinstall")
			}
			if err := requireDistro(); err != nil {
				return err
			}
			ensureNestedVirtualization(cmd)

			// Resolve the launch config up front so a bad config fails before we
			// touch the distro.
			wcfg, token, err := resolveDaemonConfig(o)
			if err != nil {
				return err
			}

			// Obtain the new rootfs before destroying anything, so a download
			// failure leaves the current install intact.
			tarPath := rootfs
			if tarPath == "" {
				p, ferr := fetchRootfs(cmd.Context(), cmd)
				if ferr != nil {
					return ferr
				}
				tarPath = p
				defer func() { _ = os.Remove(p) }()
			}

			if err := validateRootfs(tarPath); err != nil {
				return err
			}
			fullBackup, err := backupDistro(cmd)
			if err != nil {
				return err
			}
			// Stop the daemon so nothing writes to the data dirs during the swap.
			if err := wslboot.Stop(wcfg.Distro, wcfg.User); err != nil && !errors.Is(err, wslboot.ErrNoDaemon) {
				return err
			}

			// Snapshot data before the reimport (nothing to restore if empty).
			// The backup is deliberately NOT deferred-deleted: once the distro is
			// unregistered it is the only copy of the user's data, so it must
			// survive any later failure. It is removed only on full success; on
			// any error after export its path is reported for manual recovery.
			var dataPath string
			if keepData {
				dp, derr := exportDistroData(cmd)
				if derr != nil {
					return derr
				}
				dataPath = dp // "" when there was nothing to preserve
			}

			// Swap the rootfs.
			if err := wsldistro.Unregister(); err != nil {
				preserveDataBackup(cmd, dataPath)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "importing %q from %s ...\n", wsldistro.Name, tarPath)
			if err := wsldistro.Import(wsldistro.DefaultInstallDir(), tarPath); err != nil {
				preserveDataBackup(cmd, dataPath)
				return err
			}

			// Restore data into the fresh rootfs.
			if dataPath != "" {
				if err := importDistroData(cmd, dataPath); err != nil {
					preserveDataBackup(cmd, dataPath)
					return err
				}
			}

			// Bring the daemon back up on the new rootfs.
			if err := launchAndWait(cmd, wcfg, token); err != nil {
				// Data is already restored into the distro at this point, but keep
				// the backup too until the user confirms the machine is healthy.
				preserveDataBackup(cmd, dataPath)
				return err
			}
			if dataPath != "" {
				_ = os.Remove(dataPath)
			}
			_ = os.Remove(fullBackup)
			return nil
		},
	}
	c.Flags().StringVar(&rootfs, "rootfs", "",
		"path to a jerboa rootfs tarball (default: download the release artifact)")
	c.Flags().BoolVar(&keepData, "keep-data", false,
		"preserve images, VMs, networks and volumes across the reimport")
	o.bind(c)
	return c
}

// exportDistroData snapshots the distro's persistent data to a temp gzip
// tarball, returning its path. An empty archive (a fresh distro with no data)
// yields "" with no error — the caller then skips the restore.
func exportDistroData(cmd *cobra.Command) (string, error) {
	tmp, err := os.CreateTemp("", "jerboa-data-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("daemon reinstall: temp data file: %w", err)
	}
	path := tmp.Name()
	fmt.Fprintf(cmd.OutOrStdout(), "exporting data (%s) ...\n", strings.Join(wsldistro.DataDirs, ", "))
	if err := wsldistro.ExportData(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("daemon reinstall: close data file: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("daemon reinstall: stat data file: %w", err)
	}
	if fi.Size() == 0 {
		_ = os.Remove(path)
		fmt.Fprintln(cmd.OutOrStdout(), "no existing data to preserve")
		return "", nil
	}
	return path, nil
}

// preserveDataBackup tells the user where the data backup was left after a
// failed reinstall, so they can recover manually instead of losing it. No-op
// when there was no data to preserve.
func preserveDataBackup(cmd *cobra.Command, dataPath string) {
	if dataPath != "" {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: reinstall failed after data was exported; your data backup is preserved at:\n  %s\n"+
				"restore it into the distro under ~/.jerboa once the machine is healthy.\n", dataPath)
	}
}

// importDistroData restores a tarball produced by exportDistroData into the
// freshly imported distro.
func importDistroData(cmd *cobra.Command, path string) error {
	f, err := os.Open(path) //nolint:gosec // caller-owned temp file we just wrote
	if err != nil {
		return fmt.Errorf("daemon reinstall: open data file: %w", err)
	}
	defer func() { _ = f.Close() }()
	fmt.Fprintln(cmd.OutOrStdout(), "restoring data ...")
	return wsldistro.ImportData(f)
}

func newDaemonUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the jerboa WSL2 distro and all its data",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" {
				return errNotWindows("uninstall")
			}
			exists, err := wsldistro.Exists()
			if err != nil {
				return err
			}
			if !exists {
				fmt.Fprintf(cmd.OutOrStdout(), "distro %q not installed\n", wsldistro.Name)
				return nil
			}
			if err := wsldistro.Unregister(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "uninstalled")
			return nil
		},
	}
}

func newDaemonStartCmd() *cobra.Command {
	var o daemonOpts
	c := &cobra.Command{
		Use:   "start",
		Short: "Start the managed jerboad daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS == "darwin" {
				return nativeDaemonCommand(cmd, "start", o)
			}
			if runtime.GOOS != "windows" {
				return errNotWindows("start")
			}
			if err := requireDistro(); err != nil {
				return err
			}
			wcfg, token, err := resolveDaemonConfig(o)
			if err != nil {
				return err
			}
			if wslboot.Healthy(cmd.Context(), wcfg.Endpoint, token) {
				fmt.Fprintf(cmd.OutOrStdout(), "daemon already running at %s\n", wcfg.Endpoint)
				return nil
			}
			return launchAndWait(cmd, wcfg, token)
		},
	}
	o.bind(c)
	return c
}

func newDaemonRestartCmd() *cobra.Command {
	var o daemonOpts
	c := &cobra.Command{
		Use:   "restart",
		Short: "Restart the managed jerboad daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS == "darwin" {
				return nativeDaemonCommand(cmd, "restart", o)
			}
			if runtime.GOOS != "windows" {
				return errNotWindows("restart")
			}
			if err := requireDistro(); err != nil {
				return err
			}
			wcfg, token, err := resolveDaemonConfig(o)
			if err != nil {
				return err
			}
			if err := wslboot.Stop(wcfg.Distro, wcfg.User); err != nil && !errors.Is(err, wslboot.ErrNoDaemon) {
				return err
			}
			waitPortReleased(cmd.Context(), wcfg.Endpoint, token, 5*time.Second)
			return launchAndWait(cmd, wcfg, token)
		},
	}
	o.bind(c)
	return c
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the managed jerboad daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS == "darwin" {
				return nativeDaemonCommand(cmd, "stop", daemonOpts{})
			}
			if runtime.GOOS != "windows" {
				return errNotWindows("stop")
			}
			err := wslboot.Stop(wsldistro.Name, daemonLaunchUser)
			if errors.Is(err, wslboot.ErrNoDaemon) {
				fmt.Fprintln(cmd.OutOrStdout(), "no running daemon")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon stopped")
			return nil
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the jerboad daemon is reachable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			endpoint := config.ResolveEndpoint("")
			if runtime.GOOS == "windows" {
				installed, err := wsldistro.Exists()
				if err != nil {
					return err
				}
				if !installed {
					fmt.Fprintf(out, "distro:   not installed (run: jerboa daemon install)\n")
					return nil
				}
				// Dial the distro's VM IP; loopback does not reach it.
				if dial, derr := distroDialEndpoint(); derr == nil {
					endpoint = dial
				}
			}
			token := config.ResolveToken()
			if token == "" {
				if t, _, err := wslboot.LoadDaemonFile(daemonJSONPath()); err == nil {
					token = t
				}
			}
			client, err := api.DialWithToken(endpoint, token)
			if err != nil {
				fmt.Fprintf(out, "daemon: not running (%s)\n", endpoint)
				return nil
			}
			defer func() { _ = client.Close() }()
			ver, err := client.DaemonVersion(cmd.Context())
			if err != nil {
				fmt.Fprintf(out, "daemon: unreachable (%s): %v\n", endpoint, err)
				return nil
			}
			fmt.Fprintf(out, "daemon:   running\nendpoint: %s\nversion:  %s\n", endpoint, ver)
			return nil
		},
	}
}

func newDaemonLogsCmd() *cobra.Command {
	var follow bool
	c := &cobra.Command{
		Use:   "logs",
		Short: "Show the daemon launch log",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := daemonLogPath()
			f, err := os.Open(path) //nolint:gosec // fixed, client-owned log path
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("daemon logs: no log yet at %s (start it: jerboa daemon start)", path)
				}
				return fmt.Errorf("daemon logs: %w", err)
			}
			defer func() { _ = f.Close() }()

			if _, err := io.Copy(cmd.OutOrStdout(), f); err != nil {
				return fmt.Errorf("daemon logs: %w", err)
			}
			if !follow {
				return nil
			}
			for {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(500 * time.Millisecond):
				}
				if _, err := io.Copy(cmd.OutOrStdout(), f); err != nil {
					return fmt.Errorf("daemon logs: %w", err)
				}
			}
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log lines until interrupted")
	return c
}

// fetchRootfs downloads the dedicated-distro rootfs named by the signed release
// manifest (SHA-256 verified) into a temp file and returns its path. The caller
// removes it after import. R2 is the single source of truth — there is no
// GitHub fallback.
func fetchRootfs(ctx context.Context, cmd *cobra.Command) (string, error) {
	tmp, err := os.CreateTemp("", "jerboa-rootfs-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("daemon install: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// DownloadArtifact installs via rename(dest.tmp -> dest), which fails on
	// Windows when dest already exists, so remove the placeholder now.
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("daemon install: close temp: %w", err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return "", fmt.Errorf("daemon install: remove temp placeholder: %w", err)
	}

	cl, err := release.Default()
	if err != nil {
		return "", fmt.Errorf("daemon install: release client: %w", err)
	}
	m, err := cl.FetchManifest(ctx, release.ChannelStable)
	if err != nil {
		return "", fmt.Errorf("daemon install: fetch manifest: %w", err)
	}
	d, ok := m.Component(release.ComponentDistro)
	if !ok {
		return "", fmt.Errorf("daemon install: manifest has no distro component")
	}
	a, err := d.Asset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", fmt.Errorf("daemon install: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "downloading %s (%s, verified) ...\n",
		wsldistro.RootfsArtifact, d.Version)
	if err := cl.DownloadArtifact(ctx, a, tmpPath); err != nil {
		return "", fmt.Errorf("daemon install: download rootfs: %w "+
			"(if no release is published, build it with distro/build.sh and pass --rootfs)", err)
	}
	return tmpPath, nil
}

// launchAndWait starts the daemon, waits for it to answer, then records the
// rendezvous file so later runs reach the same daemon.
func launchAndWait(cmd *cobra.Command, wcfg wslboot.Config, token string) error {
	if err := wslboot.Launch(wcfg); err != nil {
		return err
	}
	if err := wslboot.WaitHealthy(cmd.Context(), wcfg); err != nil {
		return fmt.Errorf("%w (check: jerboa daemon logs)", err)
	}
	if err := wslboot.SaveDaemonFile(daemonJSONPath(), token, wcfg.Endpoint); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: persist daemon file: %v\n", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "daemon running at %s (hypervisor=%s)\n", wcfg.Endpoint, wcfg.Hypervisor)
	return nil
}

// ensureNestedVirtualization makes sure the host's .wslconfig enables nested
// virtualization, which firecracker needs for /dev/kvm. Best-effort: a failure
// only prints a hint, since the user can set it manually. When the file changed,
// it must be picked up by `wsl --shutdown`, so we say so.
func ensureNestedVirtualization(cmd *cobra.Command) {
	changed, path, err := wsldistro.EnsureNestedVirtualization()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: could not enable nested virtualization in .wslconfig: %v\n"+
				"  firecracker needs it for /dev/kvm; set [wsl2] nestedVirtualization=true manually\n", err)
		return
	}
	if changed {
		fmt.Fprintf(cmd.OutOrStdout(),
			"enabled nested virtualization in %s (firecracker needs it for /dev/kvm)\n"+
				"  run `wsl --shutdown` for it to take effect\n", path)
	}
}

// requireDistro errors with an install hint when the dedicated distro is absent.
func requireDistro() error {
	exists, err := wsldistro.Exists()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the %q WSL2 distro is not installed — run: jerboa daemon install", wsldistro.Name)
	}
	return nil
}

// daemonPort returns the TCP port the daemon uses, taken from the configured
// endpoint or the 7890 default.
func daemonPort() string {
	if rest, ok := strings.CutPrefix(config.ResolveEndpoint(""), "tcp://"); ok {
		if _, port, found := strings.Cut(rest, ":"); found && port != "" {
			return port
		}
	}
	return "7890"
}

// distroListenEndpoint is the --host the daemon binds inside the distro: 0.0.0.0
// so it accepts the Windows host across the WSL2 boundary.
func distroListenEndpoint() string { return "tcp://0.0.0.0:" + daemonPort() }

// distroDialEndpoint is the address the Windows client dials. WSL2 loopback
// forwarding does not reach a secondary distro, so we target its VM IP directly.
// Resolving the IP starts the distro if it is stopped.
func distroDialEndpoint() (string, error) {
	ip, err := wsldistro.IP()
	if err != nil {
		return "", err
	}
	return "tcp://" + ip + ":" + daemonPort(), nil
}

// waitPortReleased blocks until the old daemon stops answering or d elapses, so a
// restart does not race the previous listener on the same port.
func waitPortReleased(ctx context.Context, endpoint, token string, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !wslboot.Healthy(ctx, endpoint, token) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func errNotWindows(action string) error {
	return fmt.Errorf("daemon %s manages a WSL2 distro and only runs on Windows; "+
		"on Linux run jerboad directly", action)
}

// daemonLogPath mirrors wslboot's launch log location (~/.jerboa/jerboad-wsl.log).
func daemonLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".jerboa", "jerboad-wsl.log")
	}
	return filepath.Join(home, ".jerboa", daemonLogName())
}

func daemonLogName() string {
	if runtime.GOOS == "darwin" {
		return "jerboad.log"
	}
	return "jerboad-wsl.log"
}

// validateRootfs reads the entire archive before an installed distro is changed.
func validateRootfs(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("rootfs: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("rootfs: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("rootfs must be a regular tar file")
	}
	r := bufio.NewReader(f)
	var input io.Reader = r
	magic, _ := r.Peek(2)
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("rootfs: %w", err)
		}
		defer func() { _ = gz.Close() }()
		input = gz
	}
	tr := tar.NewReader(input)
	entries := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid rootfs: %w", err)
		}
		entries++
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return fmt.Errorf("rootfs: %w", err)
		}
	}
	if entries == 0 {
		return fmt.Errorf("rootfs archive is empty")
	}
	if _, err := io.Copy(io.Discard, input); err != nil {
		return fmt.Errorf("rootfs: %w", err)
	}
	return nil
}
func backupDistro(cmd *cobra.Command) (string, error) {
	f, err := os.CreateTemp("", "jerboa-recovery-*.tar")
	if err != nil {
		return "", fmt.Errorf("backup distro: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	out, err := exec.CommandContext(cmd.Context(), "wsl", "--export", wsldistro.Name, name).CombinedOutput() //nolint:gosec // fixed executable and distro; destination is an owned temporary file
	if err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("backup distro: %w: %s", err, out)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Recovery backup: %s (kept if replacement fails; restore with wsl --import)\n", name)
	return name, nil
}
