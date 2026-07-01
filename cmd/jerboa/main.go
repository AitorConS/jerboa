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
			// On Windows the daemon lives in the dedicated WSL2 distro: auto-start
			// it for any daemon-backed command, like Docker Desktop, and dial the
			// distro's VM IP (loopback does not reach a secondary distro).
			if runtime.GOOS == "windows" && strings.HasPrefix(endpoint, "tcp://") && needsDaemon(cmd) {
				dial, err := ensureDaemon(cmd.Context(), override)
				if err != nil {
					return err
				}
				endpoint = dial
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
		newVolumeCmd(&endpoint, &storePath, &outputFmt, &verbose),
		newKernelCmd(&verbose),
		newPkgCmd(&endpoint),
		newNetworkCmd(&endpoint, &outputFmt),
		newDNSCmd(&endpoint, &outputFmt),
		newStatsCmd(&endpoint, &outputFmt),
		newNodeCmd(&endpoint, &outputFmt),
		newConfigCmd(),
	)
	// The daemon command group manages the dedicated WSL2 distro that hosts
	// jerboad on Windows (import/start/stop). On Linux jerboad runs natively
	// (see scripts/install.sh), so the group has nothing to do and is hidden.
	if runtime.GOOS == "windows" {
		root.AddCommand(newDaemonCmd())
	}
	return root
}

// needsDaemon reports whether the command (or any ancestor) talks to the
// daemon. Local-only command groups and the bare root are excluded so that
// e.g. `jerboa config` or `jerboa kernel` never spin up WSL.
func needsDaemon(cmd *cobra.Command) bool {
	// `volume seed` is the one volume subcommand that talks to the daemon (it
	// streams seed files to mkfs), so it must trigger daemon auto-boot even
	// though the rest of the volume group is local-only.
	if cmd.Name() == "seed" {
		return true
	}
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

// ensureDaemon makes sure the WSL2 daemon is running and returns the address the
// client should dial. override, when non-empty, is an explicit --host that wins
// over the distro's auto-discovered VM IP.
func ensureDaemon(ctx context.Context, override string) (string, error) {
	token := config.ResolveToken()
	if token == "" {
		t, err := wslboot.LoadOrCreateToken(daemonJSONPath())
		if err != nil {
			return "", fmt.Errorf("daemon token: %w", err)
		}
		token = t
		_ = os.Setenv("JERBOA_AUTH_TOKEN", token)
	}
	// The daemon lives in the dedicated jerboa distro; auto-boot requires it.
	installed, err := wsldistro.Exists()
	if err != nil {
		return "", err
	}
	if !installed {
		return "", fmt.Errorf("the %q WSL2 distro is not installed — run: jerboa daemon install", wsldistro.Name)
	}

	dial := override
	if dial == "" {
		d, derr := distroDialEndpoint()
		if derr != nil {
			return "", derr
		}
		dial = d
	}

	var hypervisor string
	if cfg, cerr := config.Load(config.DefaultPath()); cerr == nil {
		hypervisor = cfg.Hypervisor
	}
	if err := wslboot.EnsureDaemon(ctx, wslboot.Config{
		Endpoint:       dial,
		ListenEndpoint: distroListenEndpoint(),
		Distro:         wsldistro.Name,
		User:           daemonLaunchUser,
		Token:          token,
		Hypervisor:     hypervisor,
	}); err != nil {
		return "", err
	}
	// Record the rendezvous so later runs and `jerboa daemon` reach the same
	// daemon with the same secret.
	_ = wslboot.SaveDaemonFile(daemonJSONPath(), token, dial)
	return dial, nil
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
