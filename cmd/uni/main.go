package main

import (
	"fmt"
	"os"

	"github.com/AitorConS/unikernel-engine/internal/config"
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
		Use:     "uni",
		Short:   "Unikernel engine CLI",
		Version: version,
		// Resolve the daemon endpoint before any subcommand runs:
		// --host > --socket > UNI_HOST > config file > platform default.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			override := hostFlag
			if override == "" {
				override = socketFlag
			}
			endpoint = config.ResolveEndpoint(override)
			// Surface a config-file token through the environment so api.Dial
			// (which reads UNI_AUTH_TOKEN) authenticates transparently.
			if tok := config.ResolveToken(); tok != "" {
				_ = os.Setenv("UNI_AUTH_TOKEN", tok)
			}
			return nil
		},
	}
	root.PersistentFlags().StringVarP(&hostFlag, "host", "H", "",
		"unid daemon endpoint (unix:///path or tcp://host:port)")
	root.PersistentFlags().StringVar(&socketFlag, "socket", "",
		"unid daemon socket path (deprecated: use --host)")
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
		newSignCmd(&storePath),
		newVerifyCmd(&storePath),
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
		newPkgCmd(),
		newUpgradeCmd(&endpoint, &verbose),
		newNetworkCmd(&endpoint, &outputFmt),
		newDNSCmd(&endpoint, &outputFmt),
		newStatsCmd(&endpoint, &outputFmt),
		newNodeCmd(&endpoint, &outputFmt),
		newServiceCmd(&endpoint, &outputFmt),
		newConfigCmd(),
	)
	return root
}

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/images"
	}
	return home + "/.uni/images"
}
