package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/spf13/cobra"
)

func newImagesCmd(endpoint *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:   "images",
		Short: "List unikernel images stored by the daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("images: connect to daemon: %w", err)
			}
			defer func() { _ = client.Close() }()

			list, err := client.ImageList(cmd.Context())
			if err != nil {
				return fmt.Errorf("images: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "DIGEST\tNAME\tTAG\tCREATED\tSIZE")
			for _, m := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					shortDigest(m.DiskDigest),
					m.Name,
					m.Tag,
					m.Created,
					formatSize(m.DiskSize),
				)
			}
			return w.Flush()
		},
	}
}

func newRmiCmd(endpoint *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rmi <ref>",
		Short: "Remove a unikernel image from the daemon store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("rmi: connect to daemon: %w", err)
			}
			defer func() { _ = client.Close() }()

			if err := client.ImageRemove(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("rmi: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), args[0])
			return nil
		},
	}
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func formatSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
