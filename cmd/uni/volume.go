package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/AitorConS/unikernel-engine/internal/volume"
	"github.com/spf13/cobra"
)

func newVolumeCmd(storePath *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage persistent volumes",
	}
	cmd.AddCommand(
		newVolumeCreateCmd(storePath),
		newVolumeLsCmd(storePath, outputFmt),
		newVolumeRmCmd(storePath),
		newVolumeInspectCmd(storePath),
	)
	return cmd
}

func newVolumeCreateCmd(storePath *string) *cobra.Command {
	var size string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new named volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			sizeBytes, err := volume.ParseSize(size)
			if err != nil {
				return fmt.Errorf("volume create: invalid size: %w", err)
			}
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume create: %w", err)
			}
			v, err := store.Create(name, sizeBytes)
			if err != nil {
				return fmt.Errorf("volume create: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), v.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&size, "size", "1G", "volume size (e.g. 512M, 1G, 2G)")
	return cmd
}

func newVolumeLsCmd(storePath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List volumes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume ls: %w", err)
			}
			vols, err := store.List()
			if err != nil {
				return fmt.Errorf("volume ls: %w", err)
			}
			if *outputFmt == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(vols)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSIZE\tCREATED")
			for _, v := range vols {
				fmt.Fprintf(w, "%s\t%s\t%s\n",
					v.ID,
					humanBytes(v.SizeBytes),
					v.CreatedAt.Format("2006-01-02 15:04:05"),
				)
			}
			return w.Flush()
		},
	}
}

func newVolumeRmCmd(storePath *string) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a volume",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume rm: %w", err)
			}
			if err := store.Remove(args[0]); err != nil {
				return fmt.Errorf("volume rm: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), args[0])
			return nil
		},
	}
}

func newVolumeInspectCmd(storePath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show detailed information about a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume inspect: %w", err)
			}
			v, err := store.Get(args[0])
			if err != nil {
				return fmt.Errorf("volume inspect: %w", err)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(v)
		},
	}
}

func humanBytes(b int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1fG", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.1fM", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.1fK", float64(b)/KB)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
