package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
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
	hypervisor string
}

func (o *daemonOpts) bind(c *cobra.Command) {
	c.Flags().StringVar(&o.hypervisor, "hypervisor", "",
		"hypervisor to run (qemu or firecracker); defaults to config")
}

// resolveDaemonConfig assembles the wslboot launch config: the dedicated distro,
// run as root, binding 0.0.0.0 while the client dials loopback. It returns the
// token separately for the daemon-file rendezvous.
func resolveDaemonConfig(o daemonOpts) (wslboot.Config, string, error) {
	token := config.ResolveToken()
	if token == "" {
		t, err := wslboot.LoadOrCreateToken(daemonJSONPath())
		if err != nil {
			return wslboot.Config{}, "", fmt.Errorf("daemon token: %w", err)
		}
		token = t
	}

	hyp := o.hypervisor
	if hyp == "" {
		if cfg, err := config.Load(config.DefaultPath()); err == nil {
			hyp = cfg.Hypervisor
		}
	}

	endpoint := config.ResolveEndpoint("")
	return wslboot.Config{
		Endpoint:       endpoint,
		ListenEndpoint: listenEndpointFor(endpoint),
		Distro:         wsldistro.Name,
		User:           daemonLaunchUser,
		Token:          token,
		Hypervisor:     hyp,
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
			exists, err := wsldistro.Exists()
			if err != nil {
				return err
			}
			if exists {
				if !force {
					fmt.Fprintf(cmd.OutOrStdout(), "distro %q already installed (use --force to reimport)\n", wsldistro.Name)
					return nil
				}
				if uerr := wsldistro.Unregister(); uerr != nil {
					return uerr
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

			fmt.Fprintf(cmd.OutOrStdout(), "importing %q from %s ...\n", wsldistro.Name, tarPath)
			if err := wsldistro.Import(wsldistro.DefaultInstallDir(), tarPath); err != nil {
				return err
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
		Short: "Start the jerboad daemon in the jerboa WSL2 distro",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
		Short: "Restart the jerboad daemon in the jerboa WSL2 distro",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
		Short: "Stop the jerboad daemon in the jerboa WSL2 distro",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			if runtime.GOOS == "windows" {
				if installed, err := wsldistro.Exists(); err == nil && !installed {
					fmt.Fprintf(out, "distro:   not installed (run: jerboa daemon install)\n")
					return nil
				}
			}
			endpoint := config.ResolveEndpoint("")
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
		Short: "Show the WSL2 daemon launch log",
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

// fetchRootfs downloads the dedicated-distro rootfs for the latest release into a
// temp file and returns its path. The caller removes it after import.
func fetchRootfs(ctx context.Context, cmd *cobra.Command) (string, error) {
	ver, err := latestCLIVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("daemon install: resolve latest version: %w", err)
	}
	url := fmt.Sprintf("%s/%s/%s", cliReleaseBase, ver, wsldistro.RootfsArtifact)

	tmp, err := os.CreateTemp("", "jerboa-rootfs-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("daemon install: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	fmt.Fprintf(cmd.OutOrStdout(), "downloading %s (%s) ...\n", wsldistro.RootfsArtifact, ver)
	if err := downloadToVerified(ctx, url, tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("daemon install: download rootfs: %w "+
			"(if no release is published, build it with distro/build.sh and pass --rootfs)", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("daemon install: close temp: %w", err)
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

// listenEndpointFor maps the client dial endpoint to the address the daemon
// binds: tcp://127.0.0.1:PORT -> tcp://0.0.0.0:PORT, so the daemon accepts the
// Windows host across the WSL2 boundary regardless of which distro forwards.
func listenEndpointFor(dial string) string {
	rest, ok := strings.CutPrefix(dial, "tcp://")
	if !ok {
		return dial
	}
	if _, port, found := strings.Cut(rest, ":"); found {
		return "tcp://0.0.0.0:" + port
	}
	return dial
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
	return filepath.Join(home, ".jerboa", "jerboad-wsl.log")
}
