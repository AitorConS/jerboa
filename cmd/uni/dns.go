package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/spf13/cobra"
)

func newDNSCmd(socketPath, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Resolve and inspect internal DNS records",
	}
	cmd.AddCommand(newDNSResolveCmd(socketPath, outputFmt), newDNSListCmd(socketPath, outputFmt))
	return cmd
}

func newDNSResolveCmd(socketPath, outputFmt *string) *cobra.Command {
	var network string
	cmd := &cobra.Command{
		Use:   "resolve <name>",
		Short: "Resolve a service/VM name to an IP",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("dns resolve: connect to daemon: %w", err)
			}
			defer func() {
				_ = client.Close()
			}()

			rec, err := client.DNSResolve(cmd.Context(), args[0], network)
			if err != nil {
				return fmt.Errorf("dns resolve: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), rec)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tNETWORK\tIP\tVM")
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", rec.Name, rec.Network, rec.IP, rec.VMID)
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&network, "network", "", "network scope for resolution")
	return cmd
}

func newDNSListCmd(socketPath, outputFmt *string) *cobra.Command {
	var network string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List resolvable internal DNS records",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("dns list: connect to daemon: %w", err)
			}
			defer func() {
				_ = client.Close()
			}()

			recs, err := client.DNSList(cmd.Context(), network)
			if err != nil {
				return fmt.Errorf("dns list: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), recs)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tNETWORK\tIP\tVM")
			for _, rec := range recs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", rec.Name, rec.Network, rec.IP, rec.VMID)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&network, "network", "", "filter by network")
	return cmd
}
