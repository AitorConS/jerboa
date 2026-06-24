package main

import (
	"fmt"
	"strings"

	"github.com/AitorConS/jerboa/internal/config"
	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage jerboa configuration",
	}
	cmd.AddCommand(
		newConfigSetCmd(),
		newConfigGetCmd(),
	)
	return cmd
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Long: `Set a configuration value.

Available keys:

  hypervisor   VM backend to use: qemu (default) or firecracker`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], strings.ToLower(args[1])
			cfgPath := config.DefaultPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			switch strings.ToLower(key) {
			case "hypervisor":
				if value != "qemu" && value != "firecracker" {
					return fmt.Errorf("hypervisor must be qemu or firecracker, got %q", value)
				}
				cfg.Hypervisor = value
			default:
				return fmt.Errorf("unknown config key %q", key)
			}
			if err := config.Save(cfgPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", key, value)
			return nil
		},
	}
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Get a configuration value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := strings.ToLower(args[0])
			cfg, err := config.Load(config.DefaultPath())
			if err != nil {
				return err
			}
			switch key {
			case "hypervisor":
				fmt.Fprintln(cmd.OutOrStdout(), cfg.Hypervisor)
			default:
				return fmt.Errorf("unknown config key %q", key)
			}
			return nil
		},
	}
}
