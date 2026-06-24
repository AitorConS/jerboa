package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newNetworkCmd(socketPath *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Manage networks",
	}
	cmd.AddCommand(
		newNetworkCreateCmd(socketPath, outputFmt),
		newNetworkListCmd(socketPath, outputFmt),
		newNetworkInspectCmd(socketPath),
		newNetworkRemoveCmd(socketPath),
	)
	return cmd
}

func newNetworkCreateCmd(socketPath *string, outputFmt *string) *cobra.Command {
	var (
		subnet string
		driver string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a network",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("network create: connect to daemon: %w", err)
			}
			defer client.Close()
			info, err := client.NetworkCreate(context.Background(), args[0], subnet, driver)
			if err != nil {
				return fmt.Errorf("network create: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, info)
			}
			printNetworkInfo(info)
			return nil
		},
	}
	cmd.Flags().StringVar(&subnet, "subnet", "", "CIDR subnet (e.g. 10.100.0.0/24); auto-assigned if empty")
	cmd.Flags().StringVar(&driver, "driver", "bridge", "network driver (default: bridge)")
	return cmd
}

func newNetworkListCmd(socketPath *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List networks",
		Aliases: []string{"list"},
		RunE: func(_ *cobra.Command, _ []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("network ls: connect to daemon: %w", err)
			}
			defer client.Close()
			nets, err := client.NetworkList(context.Background())
			if err != nil {
				return fmt.Errorf("network ls: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, nets)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDRIVER\tSUBNET\tGATEWAY\tBRIDGE")
			for _, n := range nets {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n.Name, n.Driver, n.Subnet, n.Gateway, n.Bridge)
			}
			return w.Flush()
		},
	}
	return cmd
}

func newNetworkInspectCmd(socketPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show network details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("network inspect: connect to daemon: %w", err)
			}
			defer client.Close()
			info, err := client.NetworkGet(context.Background(), args[0])
			if err != nil {
				return fmt.Errorf("network inspect: %w", err)
			}
			return printJSON(os.Stdout, info)
		},
	}
	return cmd
}

func newNetworkRemoveCmd(socketPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a network",
		Args:    cobra.ExactArgs(1),
		Aliases: []string{"remove"},
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("network rm: connect to daemon: %w", err)
			}
			defer client.Close()
			if err := client.NetworkRemove(context.Background(), args[0]); err != nil {
				return fmt.Errorf("network rm: %w", err)
			}
			fmt.Println("removed", args[0])
			return nil
		},
	}
	return cmd
}

func printNetworkInfo(info api.NetworkInfo) {
	fmt.Printf("Network:    %s\n", info.Name)
	fmt.Printf("Driver:     %s\n", info.Driver)
	fmt.Printf("Subnet:     %s\n", info.Subnet)
	fmt.Printf("Gateway:    %s\n", info.Gateway)
	fmt.Printf("Bridge:     %s\n", info.Bridge)
	fmt.Printf("Created:    %s\n", info.CreatedAt)
}
