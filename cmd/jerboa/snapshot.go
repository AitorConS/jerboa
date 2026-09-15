package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newSnapshotCmd(socketPath *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Create and restore in-place VM snapshots (macOS Firecracker/HVF)",
		Long: `Snapshots capture a running Firecracker/HVF VM attached to a named network
(RAM, vCPU and device state and its ephemeral root disk) into the daemon's
private store. Restore is in place: the same stopped VM resumes with its ID,
MAC, IP, name, aliases and published ports under the daemon's current policy.
VMs with volumes are rejected. Established TCP connections are not preserved,
and snapshots are not portable across hosts, macOS builds or VMM builds.`,
	}
	cmd.AddCommand(
		newSnapshotCreateCmd(socketPath, outputFmt),
		newSnapshotRestoreCmd(socketPath, outputFmt),
		newSnapshotListCmd(socketPath, outputFmt),
		newSnapshotInspectCmd(socketPath),
		newSnapshotRemoveCmd(socketPath),
	)
	return cmd
}

func dialSnapshot(socketPath *string, verb string) (*api.Client, error) {
	client, err := api.Dial(*socketPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: connect to daemon: %w", verb, err)
	}
	return client, nil
}

func newSnapshotCreateCmd(socketPath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:   "create <vm> <name>",
		Short: "Pause a running VM, capture a snapshot and resume it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialSnapshot(socketPath, "create")
			if err != nil {
				return err
			}
			defer client.Close()
			fmt.Fprintf(cmd.ErrOrStderr(), "Capturing snapshot %s of VM %s (guest paused during capture)...\n", args[1], args[0])
			info, err := client.SnapshotCreate(context.Background(), args[0], args[1])
			if err != nil {
				return fmt.Errorf("snapshot create: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, info)
			}
			fmt.Printf("created %s (%d bytes) from VM %s\n", info.Name, info.SizeBytes, info.VMID)
			return nil
		},
	}
}

func newSnapshotRestoreCmd(socketPath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <vm> <name>",
		Short: "Restore a stopped VM in place from one of its snapshots",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialSnapshot(socketPath, "restore")
			if err != nil {
				return err
			}
			defer client.Close()
			fmt.Fprintf(cmd.ErrOrStderr(), "Restoring VM %s from snapshot %s...\n", args[0], args[1])
			info, err := client.SnapshotRestore(context.Background(), args[0], args[1])
			if err != nil {
				return fmt.Errorf("snapshot restore: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, info)
			}
			fmt.Println(info.ID)
			return nil
		},
	}
}

func newSnapshotListCmd(socketPath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List snapshots",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			client, err := dialSnapshot(socketPath, "ls")
			if err != nil {
				return err
			}
			defer client.Close()
			list, err := client.SnapshotList(context.Background())
			if err != nil {
				return fmt.Errorf("snapshot ls: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, list)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVM\tNETWORK\tIP\tSIZE\tCREATED\tERROR")
			for _, s := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", s.Name, s.VMID, s.Network, s.IPAddress, s.SizeBytes, s.CreatedAt, s.Error)
			}
			return w.Flush()
		},
	}
}

func newSnapshotInspectCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show snapshot metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := dialSnapshot(socketPath, "inspect")
			if err != nil {
				return err
			}
			defer client.Close()
			info, err := client.SnapshotInspect(context.Background(), args[0])
			if err != nil {
				return fmt.Errorf("snapshot inspect: %w", err)
			}
			return printJSON(os.Stdout, info)
		},
	}
}

func newSnapshotRemoveCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a snapshot",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := dialSnapshot(socketPath, "rm")
			if err != nil {
				return err
			}
			defer client.Close()
			if err := client.SnapshotRemove(context.Background(), args[0]); err != nil {
				return fmt.Errorf("snapshot rm: %w", err)
			}
			fmt.Println("removed", args[0])
			return nil
		},
	}
}
