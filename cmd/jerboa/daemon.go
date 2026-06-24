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
	"github.com/spf13/cobra"
)

// newDaemonCmd builds the `jerboa daemon` command group, which manages the
// jerboad daemon that runs inside WSL2 on Windows. start/stop/restart go through
// the same token + endpoint as the client's auto-boot, so they always agree.
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the jerboad daemon running in WSL2",
		Long: "Manage the jerboad daemon hosted inside WSL2.\n\n" +
			"start/stop/restart reuse the client-owned token and loopback TCP endpoint\n" +
			"(~/.jerboa/daemon.json), so the daemon and every client run match by\n" +
			"construction. Use --hypervisor firecracker --sudo for the privileged path.",
	}
	cmd.AddCommand(
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
	sudo       bool
	distro     string
}

func (o *daemonOpts) bind(c *cobra.Command) {
	c.Flags().StringVar(&o.hypervisor, "hypervisor", "",
		"hypervisor to run (qemu or firecracker); defaults to config")
	c.Flags().BoolVar(&o.sudo, "sudo", false,
		"run jerboad under sudo (needed for firecracker networking)")
	c.Flags().StringVar(&o.distro, "distro", "",
		"WSL2 distro to host the daemon (defaults to config or the WSL default)")
}

// resolveDaemonConfig assembles the wslboot launch config from flags, config
// file, and the client-owned token (created on first use). It returns the token
// separately for the daemon-file rendezvous.
func resolveDaemonConfig(o daemonOpts) (wslboot.Config, string, error) {
	token := config.ResolveToken()
	if token == "" {
		t, err := wslboot.LoadOrCreateToken(daemonJSONPath())
		if err != nil {
			return wslboot.Config{}, "", fmt.Errorf("daemon token: %w", err)
		}
		token = t
	}

	hyp, distro, jerboadPath := o.hypervisor, o.distro, ""
	if cfg, err := config.Load(config.DefaultPath()); err == nil {
		if hyp == "" {
			hyp = cfg.Hypervisor
		}
		if distro == "" {
			distro = cfg.Daemon.Distro
		}
		jerboadPath = cfg.Daemon.JerboadPath
	}

	return wslboot.Config{
		Endpoint:    config.ResolveEndpoint(""),
		Distro:      distro,
		Token:       token,
		JerboadPath: jerboadPath,
		Hypervisor:  hyp,
		Sudo:        o.sudo,
	}, token, nil
}

func newDaemonStartCmd() *cobra.Command {
	var o daemonOpts
	c := &cobra.Command{
		Use:   "start",
		Short: "Start the jerboad daemon in WSL2",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" {
				return errNotWindows("start")
			}
			wcfg, token, err := resolveDaemonConfig(o)
			if err != nil {
				return err
			}
			if err := requireTCPEndpoint(wcfg.Endpoint); err != nil {
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
		Short: "Restart the jerboad daemon in WSL2",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" {
				return errNotWindows("restart")
			}
			wcfg, token, err := resolveDaemonConfig(o)
			if err != nil {
				return err
			}
			if err := requireTCPEndpoint(wcfg.Endpoint); err != nil {
				return err
			}
			if err := wslboot.Stop(wcfg.Distro, wcfg.Sudo); err != nil && !errors.Is(err, wslboot.ErrNoDaemon) {
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
	var (
		sudo   bool
		distro string
	)
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop the jerboad daemon in WSL2",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "windows" {
				return errNotWindows("stop")
			}
			if distro == "" {
				if cfg, err := config.Load(config.DefaultPath()); err == nil {
					distro = cfg.Daemon.Distro
				}
			}
			err := wslboot.Stop(distro, sudo)
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
	c.Flags().BoolVar(&sudo, "sudo", false, "use sudo to signal a daemon started under sudo")
	c.Flags().StringVar(&distro, "distro", "", "WSL2 distro hosting the daemon")
	return c
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the jerboad daemon is reachable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint := config.ResolveEndpoint("")
			token := config.ResolveToken()
			if token == "" {
				if t, _, err := wslboot.LoadDaemonFile(daemonJSONPath()); err == nil {
					token = t
				}
			}
			out := cmd.OutOrStdout()
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

// requireTCPEndpoint rejects non-TCP endpoints: the WSL2 daemon must listen on
// loopback TCP so the Windows client can reach it across the VM boundary.
func requireTCPEndpoint(ep string) error {
	if !strings.HasPrefix(ep, "tcp://") {
		return fmt.Errorf("daemon: endpoint %q is not tcp:// — the WSL2 daemon must listen on loopback TCP "+
			"so the Windows client can reach it (set [daemon] endpoint or pass --host)", ep)
	}
	return nil
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
	return fmt.Errorf("daemon %s manages a WSL2 daemon and only runs on Windows; "+
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
