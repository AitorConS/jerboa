package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/spf13/cobra"
)

func newImagesCmd(storePath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:   "images",
		Short: "List locally stored unikernel images",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := image.NewStore(*storePath)
			if err != nil {
				return fmt.Errorf("images: open store: %w", err)
			}
			list, err := store.List()
			if err != nil {
				return fmt.Errorf("images: list: %w", err)
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
					m.Created.Format(time.RFC3339),
					formatSize(m.DiskSize),
				)
			}
			return w.Flush()
		},
	}
}

func newRmiCmd(storePath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rmi <ref>",
		Short: "Remove a locally stored unikernel image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := image.NewStore(*storePath)
			if err != nil {
				return fmt.Errorf("rmi: open store: %w", err)
			}
			if err := store.Remove(args[0]); err != nil {
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
