package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
		socketPath string
		storePath  string
		outputFmt  string
		verbose    bool
	)

	root := &cobra.Command{
		Use:     "uni",
		Short:   "Unikernel engine CLI",
		Version: version,
	}
	root.PersistentFlags().StringVar(&socketPath, "socket", defaultSocketPath(),
		"unid daemon socket path")
	root.PersistentFlags().StringVar(&storePath, "store",
		defaultStorePath(), "local image store path")
	root.PersistentFlags().StringVar(&outputFmt, "output", "table",
		"output format: table or json")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "V", false,
		"show raw build and download output (useful for debugging)")

	root.AddCommand(
		newRunCmd(&socketPath, &storePath),
		newBuildCmd(&storePath, &verbose),
		newImagesCmd(&storePath, &outputFmt),
		newRmiCmd(&storePath),
		newSignCmd(&storePath),
		newVerifyCmd(&storePath),
		newPsCmd(&socketPath, &outputFmt),
		newStatusCmd(&socketPath, &outputFmt),
		newLogsCmd(&socketPath),
		newStopCmd(&socketPath),
		newRmCmd(&socketPath),
		newInspectCmd(&socketPath),
		newExecCmd(&socketPath),
		newComposeCmd(&socketPath, &storePath, &outputFmt),
		newVolumeCmd(&storePath, &outputFmt),
		newKernelCmd(&verbose),
		newPkgCmd(),
		newUpgradeCmd(&socketPath, &verbose),
		newNetworkCmd(&socketPath, &outputFmt),
		newDNSCmd(&socketPath, &outputFmt),
		newStatsCmd(&socketPath, &outputFmt),
		newNodeCmd(&socketPath, &outputFmt),
		newServiceCmd(&socketPath, &outputFmt),
	)
	return root
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
