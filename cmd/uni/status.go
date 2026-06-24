package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newStatusCmd(socketPath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon and VM status overview",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("status: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			vms, err := client.List(cmd.Context())
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}

			running := 0
			stopped := 0
			other := 0
			healthy := 0
			unhealthy := 0
			for _, v := range vms {
				switch v.State {
				case "running":
					running++
				case "stopped":
					stopped++
				default:
					other++
				}
				switch v.Health {
				case "healthy":
					healthy++
				case "unhealthy":
					unhealthy++
				}
			}

			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"total":     len(vms),
					"running":   running,
					"stopped":   stopped,
					"other":     other,
					"healthy":   healthy,
					"unhealthy": unhealthy,
					"vms":       vms,
				})
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Total:\t%d\n", len(vms))
			fmt.Fprintf(w, "Running:\t%d\n", running)
			fmt.Fprintf(w, "Stopped:\t%d\n", stopped)
			if other > 0 {
				fmt.Fprintf(w, "Other:\t%d\n", other)
			}
			fmt.Fprintf(w, "Healthy:\t%d\n", healthy)
			fmt.Fprintf(w, "Unhealthy:\t%d\n", unhealthy)
			w.Flush()
			fmt.Fprintln(cmd.OutOrStdout())

			if len(vms) > 0 {
				w2 := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w2, "ID\tNAME\tSTATE\tHEALTH\tRESTARTS\tIMAGE")
				for _, v := range vms {
					detail, err := client.Inspect(cmd.Context(), v.ID)
					if err != nil {
						continue
					}
					name := v.Name
					if name == "" {
						name = "-"
					}
					health := v.Health
					if health == "" {
						health = "-"
					}
					fmt.Fprintf(w2, "%s\t%s\t%s\t%s\t%d\t%s\n", v.ID, name, v.State, health, detail.RestartCount, v.Image)
				}
				return w2.Flush()
			}
			return nil
		},
	}
}
