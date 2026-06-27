package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newLogsCmd(socketPath *string) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
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

			if follow {
				return followLogs(cmd.Context(), client, args[0], cmd.OutOrStdout())
			}

			resp, err := client.Logs(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("logs: %w", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), resp.Logs)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false,
		"stream new serial output until the VM stops (Ctrl-C to detach)")
	return cmd
}

// followLogs polls the VM's captured serial console and prints newly appended
// output until the VM stops or the context is canceled. The daemon keeps logs
// in a bounded ring buffer; we track how much we have already printed by length
// and reprint from the start if the buffer rotated (its content shrank).
func followLogs(ctx context.Context, client *api.Client, id string, out io.Writer) error {
	const pollInterval = 500 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	printed := 0
	for {
		resp, err := client.Logs(ctx, id)
		if err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		logs := resp.Logs
		if len(logs) < printed {
			// Ring buffer rotated out earlier bytes; restart from the top.
			printed = 0
		}
		if len(logs) > printed {
			fmt.Fprint(out, logs[printed:])
			printed = len(logs)
		}

		// Stop following once the VM has exited (drain happened above).
		if info, err := client.Get(ctx, id); err == nil && info.State == "stopped" {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
