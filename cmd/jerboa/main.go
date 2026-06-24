package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/wslboot"
	"github.com/AitorConS/jerboa/internal/wsldistro"
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
		endpoint   string // resolved daemon endpoint passed to subcommands
		hostFlag   string
		socketFlag string
		storePath  string
		outputFmt  string
		verbose    bool
	)

	root := &cobra.Command{
		Use:     "jerboa",
		Short:   "Unikernel engine CLI",
		Version: version,
		// Resolve the daemon endpoint before any subcommand runs:
		// --host > --socket > JERBOA_HOST > config file > platform default.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			override := hostFlag
			if override == "" {
				override = socketFlag
			}
			endpoint = config.ResolveEndpoint(override)
			// Surface a config-file token through the environment so api.Dial
			// (which reads JERBOA_AUTH_TOKEN) authenticates transparently.
			if tok := config.ResolveToken(); tok != "" {
				_ = os.Setenv("JERBOA_AUTH_TOKEN", tok)
			}
			// On Windows the daemon lives in WSL2: auto-start it for any
			// daemon-backed command, like Docker Desktop.
			if runtime.GOOS == "windows" && strings.HasPrefix(endpoint, "tcp://") && needsDaemon(cmd) {
				if err := ensureDaemon(cmd.Context(), endpoint); err != nil {
					return err
				}
			}
			return nil
		},
	}
	root.PersistentFlags().StringVarP(&hostFlag, "host", "H", "",
		"jerboad daemon endpoint (unix:///path or tcp://host:port)")
	root.PersistentFlags().StringVar(&socketFlag, "socket", "",
		"jerboad daemon socket path (deprecated: use --host)")
	_ = root.PersistentFlags().MarkDeprecated("socket", "use --host instead")
	root.PersistentFlags().StringVar(&storePath, "store",
		defaultStorePath(), "local image store path")
	root.PersistentFlags().StringVar(&outputFmt, "output", "table",
		"output format: table or json")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "V", false,
		"show raw build and download output (useful for debugging)")

	root.AddCommand(
		newRunCmd(&endpoint, &storePath),
		newBuildCmd(&endpoint, &verbose),
		newImagesCmd(&endpoint, &outputFmt),
		newRmiCmd(&endpoint),
		newSignCmd(&endpoint),
		newVerifyCmd(&endpoint),
		newPsCmd(&endpoint, &outputFmt),
		newStatusCmd(&endpoint, &outputFmt),
		newLogsCmd(&endpoint),
		newStopCmd(&endpoint),
		newRmCmd(&endpoint),
		newInspectCmd(&endpoint),
		newExecCmd(&endpoint),
		newComposeCmd(&endpoint, &storePath, &outputFmt),
		newVolumeCmd(&storePath, &outputFmt),
		newKernelCmd(&verbose),
		newPkgCmd(&endpoint),
		newUpgradeCmd(&endpoint, &verbose),
		newNetworkCmd(&endpoint, &outputFmt),
		newDNSCmd(&endpoint, &outputFmt),
		newStatsCmd(&endpoint, &outputFmt),
		newNodeCmd(&endpoint, &outputFmt),
		newServiceCmd(&endpoint, &outputFmt),
		newConfigCmd(),
		newDaemonCmd(),
	)
	return root
}

// needsDaemon reports whether the command (or any ancestor) talks to the
// daemon. Local-only command groups and the bare root are excluded so that
// e.g. `jerboa config` or `jerboa kernel` never spin up WSL.
func needsDaemon(cmd *cobra.Command) bool {
	localGroups := map[string]bool{
		"config": true, "kernel": true, "pkg": true, "volume": true,
		"sign": true, "verify": true, "completion": true, "help": true,
		"daemon": true,
	}
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "jerboa" && c.Parent() == nil {
			break // reached root
		}
		if localGroups[c.Name()] {
			return false
		}
	}
	return cmd.Name() != "jerboa"
}

// ensureDaemon resolves (or generates) the auth token and makes sure the WSL2
// daemon is running before a command connects.
func ensureDaemon(ctx context.Context, endpoint string) error {
	token := config.ResolveToken()
	if token == "" {
		t, err := wslboot.LoadOrCreateToken(daemonJSONPath())
		if err != nil {
			return fmt.Errorf("daemon token: %w", err)
		}
		token = t
		_ = os.Setenv("JERBOA_AUTH_TOKEN", token)
	}
	// The daemon lives in the dedicated jerboa distro; auto-boot requires it.
	installed, err := wsldistro.Exists()
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("the %q WSL2 distro is not installed — run: jerboa daemon install", wsldistro.Name)
	}
	var hypervisor string
	if cfg, err := config.Load(config.DefaultPath()); err == nil {
		hypervisor = cfg.Hypervisor
	}
	if err := wslboot.EnsureDaemon(ctx, wslboot.Config{
		Endpoint:       endpoint,
		ListenEndpoint: listenEndpointFor(endpoint),
		Distro:         wsldistro.Name,
		User:           daemonLaunchUser,
		Token:          token,
		Hypervisor:     hypervisor,
	}); err != nil {
		return err
	}
	// Record the rendezvous so later runs and `jerboa daemon` reach the same
	// daemon with the same secret.
	_ = wslboot.SaveDaemonFile(daemonJSONPath(), token, endpoint)
	return nil
}

// daemonJSONPath returns the path to the client-owned daemon secret file.
func daemonJSONPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".jerboa", "daemon.json")
	}
	return filepath.Join(home, ".jerboa", "daemon.json")
}

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jerboa/images"
	}
	return home + "/.jerboa/images"
}
