package main

import (
	"fmt"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newInspectCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <id>",
		Short: "Display full details for a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("inspect: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			detail, err := client.Inspect(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("inspect: %w", err)
			}
			return printJSON(cmd.OutOrStdout(), detail)
		},
	}
}
