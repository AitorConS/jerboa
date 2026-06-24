package main

import (
	"fmt"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newExecCmd(socketPath *string) *cobra.Command {
	var sig string
	cmd := &cobra.Command{
		Use:   "exec <id>",
		Short: "Send a signal to a running VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("exec: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			if err := client.Signal(cmd.Context(), args[0], sig); err != nil {
				return fmt.Errorf("exec: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sig, "signal", "SIGTERM", "signal to send (name or number)")
	return cmd
}
