package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/spf13/cobra"
)

func newNodeCmd(socketPath, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage cluster nodes",
	}
	cmd.AddCommand(newNodeListCmd(socketPath, outputFmt))
	return cmd
}

func newNodeListCmd(socketPath, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List cluster members",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("node ls: connect to daemon: %w", err)
			}
			defer func() {
				_ = client.Close()
			}()

			resp, err := client.NodeList(cmd.Context())
			if err != nil {
				return fmt.Errorf("node ls: %w", err)
			}

			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), resp.Nodes)
			}

			if len(resp.Nodes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No cluster nodes.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tADDR\tSTATUS\tVMS\tCPU\tMEM\tSEEN")
			for _, n := range resp.Nodes {
				memStr := formatBytes(n.MemCap)
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", n.ID, n.Addr, n.Status, n.VMCount, n.CPUCap, memStr, n.LastSeen)
			}
			return w.Flush()
		},
	}
	return cmd
}
