package main

import (
	"fmt"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newStopCmd(socketPath *string) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a running VM (graceful by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("stop: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			if err := client.Stop(cmd.Context(), args[0], force); err != nil {
				return fmt.Errorf("stop: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "send SIGKILL immediately")
	return cmd
}
