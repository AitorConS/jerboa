//go:build !darwin

package main

import (
	"fmt"
	"github.com/spf13/cobra"
)

func nativeDaemonCommand(*cobra.Command, string, daemonOpts) error {
	return fmt.Errorf("native macOS lifecycle requires Darwin")
}
