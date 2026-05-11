package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

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
		socketPath       string
		storePath        string
		outputFmt        string
		registryToken    string
		registryCACert   string
		registryInsecure bool
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
	root.PersistentFlags().StringVar(&registryToken, "registry-token", os.Getenv("UNI_REGISTRY_TOKEN"),
		"Optional bearer/JWT token for registry requests (or set UNI_REGISTRY_TOKEN)")
	root.PersistentFlags().StringVar(&registryCACert, "registry-ca-cert", os.Getenv("UNI_REGISTRY_CA_CERT"),
		"Optional CA certificate file for registry TLS (or set UNI_REGISTRY_CA_CERT)")
	root.PersistentFlags().BoolVar(&registryInsecure, "registry-insecure", envBool("UNI_REGISTRY_INSECURE"),
		"Skip registry TLS certificate verification (or set UNI_REGISTRY_INSECURE=true)")

	regCfg := &registryClientConfig{token: &registryToken, caCert: &registryCACert, insecure: &registryInsecure}

	root.AddCommand(
		newRunCmd(&socketPath, &storePath),
		newBuildCmd(&storePath),
		newImagesCmd(&storePath),
		newRmiCmd(&storePath),
		newPushCmd(&storePath, regCfg),
		newPullCmd(&storePath, regCfg),
		newPsCmd(&socketPath, &outputFmt),
		newStatusCmd(&socketPath, &outputFmt),
		newLogsCmd(&socketPath),
		newStopCmd(&socketPath),
		newRmCmd(&socketPath),
		newInspectCmd(&socketPath),
		newExecCmd(&socketPath),
		newComposeCmd(&socketPath, &storePath, &outputFmt),
		newVolumeCmd(&storePath),
		newKernelCmd(),
		newPkgCmd(),
		newCpCmd(&socketPath),
		newUpgradeCmd(&socketPath),
		newNetworkCmd(&socketPath, &outputFmt),
		newDNSCmd(&socketPath, &outputFmt),
	)
	return root
}

func envBool(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
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
