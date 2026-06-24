package main

import (
	"fmt"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newLogsCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "logs <id>",
		Short: "Print captured serial console output for a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("logs: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			resp, err := client.Logs(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("logs: %w", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), resp.Logs)
			return nil
		},
	}
}
