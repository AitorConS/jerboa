package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/tools"
	"github.com/spf13/cobra"
)

func newKernelCmd(verbose *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kernel",
		Short: "Manage the kernel tools (kernel.img, boot.img, mkfs)",
	}
	cmd.AddCommand(
		newKernelCheckCmd(),
		newKernelUpdateCmd(verbose),
		newKernelListCmd(),
		newKernelUseCmd(verbose),
	)
	return cmd
}

// newKernelCheckCmd implements `jerboa kernel check`.
func newKernelCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check whether a newer kernel version is available",
		RunE: func(cmd *cobra.Command, _ []string) error {
			toolsDir := defaultToolsPath()
			local := tools.LocalVersion(toolsDir)
			fmt.Fprintf(cmd.OutOrStdout(), "Installed kernel: %s\n", local)

			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()

			remote, err := tools.RemoteVersion(ctx)
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Latest kernel:    (unavailable — %v)\n", err)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Latest kernel:    %s\n", remote)

			if tools.IsNewer(local, remote) {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Update available. Run `jerboa kernel update` to install %s.\n", remote)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Kernel is up to date.")
			}
			return nil
		},
	}
}

// newKernelListCmd implements `jerboa kernel list`.
func newKernelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all available kernel versions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()

			versions, err := tools.ListRemoteVersions(ctx)
			if err != nil {
				return fmt.Errorf("kernel list: %w", err)
			}
			if len(versions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No kernel releases found.")
				return nil
			}

			local := tools.LocalVersion(defaultToolsPath())
			for _, v := range versions {
				marker := "  "
				if v == local {
					marker = "* "
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s%s\n", marker, v)
			}
			return nil
		},
	}
}

// newKernelUseCmd implements `jerboa kernel use <version>`.
func newKernelUseCmd(verbose *bool) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "use <version>",
		Short: "Switch to a specific kernel version (e.g. v0.1.0)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version := args[0]
			if !strings.HasPrefix(version, "v") {
				version = "v" + version
			}
			toolsDir := defaultToolsPath()
			local := tools.LocalVersion(toolsDir)

			if local == version {
				fmt.Fprintf(cmd.OutOrStdout(), "Already on kernel %s.\n", version)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"Switching kernel: %s → %s\n", local, version)

			if !yes && !confirmPrompt("Proceed? [y/N] ") {
				fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
				return nil
			}

			if err := tools.ClearCachedTools(toolsDir); err != nil {
				return fmt.Errorf("kernel use: clear cache: %w", err)
			}

			sp := newSpinner(cmd.ErrOrStderr(), *verbose)
			sp.Start(fmt.Sprintf("Downloading kernel %s", version))
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			if err := tools.DownloadVersion(ctx, toolsDir, version); err != nil {
				sp.Fail("Download failed")
				return fmt.Errorf("kernel use: %w", err)
			}
			sp.Done(fmt.Sprintf("Kernel switched to %s", version))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	return cmd
}

// newKernelUpdateCmd implements `jerboa kernel update`.
func newKernelUpdateCmd(verbose *bool) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Download and install the latest kernel version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			toolsDir := defaultToolsPath()
			local := tools.LocalVersion(toolsDir)

			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			remote, err := tools.RemoteVersion(ctx)
			if err != nil {
				return fmt.Errorf("kernel update: check remote version: %w", err)
			}

			if !tools.IsNewer(local, remote) {
				fmt.Fprintf(cmd.OutOrStdout(), "Already on the latest kernel (%s).\n", local)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"New kernel version available: %s (installed: %s)\n", remote, local)

			if !yes && !confirmPrompt("Update? [y/N] ") {
				fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
				return nil
			}

			if err := tools.ClearCachedTools(toolsDir); err != nil {
				return fmt.Errorf("kernel update: clear cache: %w", err)
			}

			sp := newSpinner(cmd.ErrOrStderr(), *verbose)
			sp.Start(fmt.Sprintf("Downloading kernel %s", remote))
			dlCtx, dlCancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer dlCancel()
			if err := tools.DownloadVersion(dlCtx, toolsDir, "latest"); err != nil {
				sp.Fail("Download failed")
				return fmt.Errorf("kernel update: %w", err)
			}
			sp.Done(fmt.Sprintf("Kernel updated to %s", remote))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	return cmd
}

// confirmPrompt prints prompt to stderr and reads a y/Y answer from stdin.
// Any other input (including EOF) is treated as "no".
func confirmPrompt(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	ans := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return ans == "y" || ans == "yes"
}
