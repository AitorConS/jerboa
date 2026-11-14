package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newSystemCmd(endpoint *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Disk usage and cleanup of what the daemon stores",
	}
	cmd.AddCommand(newSystemDFCmd(endpoint, outputFmt), newSystemPruneCmd(endpoint, outputFmt))
	return cmd
}

// errOldDaemon turns "method not found" into what the user can do about it: a
// daemon from before these commands is still running.
func errOldDaemon(cmd string, err error) error {
	var rpc *api.RPCError
	if errors.As(err, &rpc) && rpc.Code == -32601 {
		return fmt.Errorf("%s: the running daemon is older than this CLI and does not support it; restart the daemon", cmd)
	}
	return fmt.Errorf("%s: %w", cmd, err)
}

func newSystemDFCmd(endpoint *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:   "df",
		Short: "Show the disk space images, VMs, volumes, snapshots and caches take",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("system df: connect to daemon: %w", err)
			}
			defer func() { _ = client.Close() }()
			df, err := client.SystemDF(cmd.Context())
			if err != nil {
				return errOldDaemon("system df", err)
			}
			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), df)
			}
			return renderDF(cmd.OutOrStdout(), df)
		},
	}
}

// usageLabels names each kind of usage the way the rest of the CLI does.
var usageLabels = map[string]string{
	"images":      "Images",
	"vms":         "VMs",
	"volumes":     "Volumes",
	"snapshots":   "Snapshots",
	"build-cache": "Build cache",
	"tools":       "Kernel tools",
}

func renderDF(out io.Writer, df api.SystemDF) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tTOTAL\tACTIVE\tSIZE\tRECLAIMABLE")
	var size, reclaimable int64
	for _, u := range df.Usage {
		label := usageLabels[u.Kind]
		if label == "" {
			label = u.Kind
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", label, u.Count, u.Active, formatSize(u.Bytes), reclaimableCell(u.Reclaimable, u.Bytes))
		size += u.Bytes
		reclaimable += u.Reclaimable
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("system df: %w", err)
	}
	fmt.Fprintf(out, "\nTotal: %s, of which %s can be reclaimed with `jerboa system prune`.\n", formatSize(size), formatSize(reclaimable))
	if df.Total > 0 {
		fmt.Fprintf(out, "Disk:  %s free of %s (%s)\n", formatSize(df.Free), formatSize(df.Total), df.DataDir)
	}
	return nil
}

func reclaimableCell(reclaimable, total int64) string {
	if total <= 0 || reclaimable <= 0 {
		return formatSize(reclaimable)
	}
	return fmt.Sprintf("%s (%d%%)", formatSize(reclaimable), reclaimable*100/total)
}

func newSystemPruneCmd(endpoint *string, outputFmt *string) *cobra.Command {
	var (
		p   api.SystemPruneParams
		yes bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove what nothing uses any more",
		Long: `Remove what nothing refers to any more: images left without a name by a
rebuild, boot disks of VMs that are gone, build cache no image needs, and the
leftovers of operations that were interrupted.

Nothing you can still name is removed unless you ask for it:
  --vms      also removes every stopped VM
  --all      also removes every image no VM uses
  --volumes  also removes every volume no VM mounts (their data is lost)

The command lists what it will remove and asks before doing it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("system prune: connect to daemon: %w", err)
			}
			defer func() { _ = client.Close() }()
			jsonOut := *outputFmt == "json"

			// Always look first: the user confirms a list, not a category.
			plan := p
			plan.DryRun = true
			preview, err := client.SystemPrune(cmd.Context(), plan)
			if err != nil {
				return errOldDaemon("system prune", err)
			}
			if p.DryRun {
				if jsonOut {
					return printJSON(cmd.OutOrStdout(), preview)
				}
				renderPrune(cmd.OutOrStdout(), preview)
				return nil
			}
			if len(preview.Removed) == 0 {
				if jsonOut {
					return printJSON(cmd.OutOrStdout(), preview)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to prune.")
				return nil
			}
			if !yes {
				renderPrune(cmd.ErrOrStderr(), preview)
				if !confirmPrompt("Remove these? [y/N] ") {
					return errors.New("system prune: canceled")
				}
			}
			res, err := client.SystemPrune(cmd.Context(), p)
			if err != nil {
				return errOldDaemon("system prune", err)
			}
			if jsonOut {
				if err := printJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else {
				renderPrune(cmd.OutOrStdout(), res)
			}
			if len(res.Errors) > 0 {
				return fmt.Errorf("system prune: %d of the removals failed", len(res.Errors))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&p.VMs, "vms", false, "also remove every stopped VM")
	cmd.Flags().BoolVarP(&p.All, "all", "a", false, "also remove every image no VM uses")
	cmd.Flags().BoolVar(&p.Volumes, "volumes", false, "also remove every volume no VM mounts (their data is lost)")
	cmd.Flags().BoolVar(&p.DryRun, "dry-run", false, "show what would be removed and remove nothing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// pruneLabels names each kind of removed item.
var pruneLabels = map[string]string{
	"vm":          "VM",
	"image":       "Image",
	"volume":      "Volume",
	"boot-disk":   "Boot disk",
	"build-cache": "Build cache",
	"leftover":    "Leftover",
}

func renderPrune(out io.Writer, res api.SystemPruneResult) {
	verb, total := "Removed", "Reclaimed"
	if res.DryRun {
		verb, total = "Would remove", "Would reclaim"
	}
	if len(res.Removed) == 0 && len(res.Errors) == 0 {
		fmt.Fprintln(out, "Nothing to prune.")
		return
	}
	// Build-cache blobs have no names worth reading: one line for all of them.
	var cacheCount int
	var cacheBytes int64
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(res.Removed) > 0 {
		fmt.Fprintln(out, verb+":")
	}
	for _, it := range res.Removed {
		if it.Kind == "build-cache" {
			cacheCount++
			cacheBytes += it.Bytes
			continue
		}
		label := pruneLabels[it.Kind]
		if label == "" {
			label = it.Kind
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", label, it.Name, formatSize(it.Bytes))
	}
	if cacheCount > 0 {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", pruneLabels["build-cache"], fmt.Sprintf("%d unused blobs", cacheCount), formatSize(cacheBytes))
	}
	_ = w.Flush()
	for _, e := range res.Errors {
		fmt.Fprintln(out, "  could not remove "+strings.TrimSpace(e))
	}
	fmt.Fprintf(out, "%s: %s\n", total, formatSize(res.Reclaimed))
}
