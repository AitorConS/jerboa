package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newStatsCmd(socketPath *string, outputFmt *string) *cobra.Command {
	var watch bool
	var interval string

	cmd := &cobra.Command{
		Use:   "stats <id>",
		Short: "Show resource usage for a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("stats: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			if watch {
				dur, err := time.ParseDuration(interval)
				if err != nil {
					return fmt.Errorf("stats: invalid interval %q: %w", interval, err)
				}
				if dur < time.Second {
					dur = time.Second
				}
				return runStatsWatch(cmd, client, args[0], *outputFmt, dur)
			}

			stats, err := client.Stats(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("stats: %w", err)
			}

			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), stats)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\t%s\n", stats.ID)
			fmt.Fprintf(w, "State\t%s\n", stats.State)
			fmt.Fprintf(w, "CPU\t%.1f%%\n", stats.CPUPct)
			fmt.Fprintf(w, "Memory\t%s\n", formatBytes(stats.MemBytes))
			fmt.Fprintf(w, "Net RX\t%s\n", formatBytes(stats.NetRxBytes))
			fmt.Fprintf(w, "Net TX\t%s\n", formatBytes(stats.NetTxBytes))
			if stats.DiskBytes > 0 {
				fmt.Fprintf(w, "Disk\t%s\n", formatBytes(stats.DiskBytes))
			}
			fmt.Fprintf(w, "Source\t%s\n", stats.Source)
			return w.Flush()
		},
	}

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "continuously watch stats")
	cmd.Flags().StringVarP(&interval, "interval", "i", "2s", "watch interval (e.g. 2s, 5s)")
	return cmd
}

func runStatsWatch(cmd *cobra.Command, client *api.Client, id, outputFmt string, interval time.Duration) error {
	for {
		stats, err := client.Stats(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("stats watch: %w", err)
		}

		if outputFmt == "json" {
			if err := printJSON(cmd.OutOrStdout(), stats); err != nil {
				return err
			}
		} else {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\t%s\n", stats.ID)
			fmt.Fprintf(w, "State\t%s\n", stats.State)
			fmt.Fprintf(w, "CPU\t%.1f%%\n", stats.CPUPct)
			fmt.Fprintf(w, "Memory\t%s\n", formatBytes(stats.MemBytes))
			fmt.Fprintf(w, "Net RX\t%s\n", formatBytes(stats.NetRxBytes))
			fmt.Fprintf(w, "Net TX\t%s\n", formatBytes(stats.NetTxBytes))
			if stats.DiskBytes > 0 {
				fmt.Fprintf(w, "Disk\t%s\n", formatBytes(stats.DiskBytes))
			}
			fmt.Fprintf(w, "Source\t%s\n", stats.Source)
			w.Flush()
		}
		fmt.Fprintln(cmd.OutOrStdout())

		select {
		case <-time.After(interval):
		case <-cmd.Context().Done():
			return nil
		}
	}
}

func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
