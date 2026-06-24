package main

import (
	"fmt"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newRmCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove a stopped VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("rm: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			if err := client.Remove(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("rm: %w", err)
			}
			return nil
		},
	}
}
