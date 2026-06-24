package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

func newServiceCmd(socketPath *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage services",
	}
	cmd.AddCommand(
		newServiceRunCmd(socketPath, outputFmt),
		newServiceScaleCmd(socketPath, outputFmt),
		newServiceUpdateCmd(socketPath, outputFmt),
		newServiceListCmd(socketPath, outputFmt),
		newServiceInspectCmd(socketPath),
		newServiceRemoveCmd(socketPath),
	)
	return cmd
}

func newServiceRunCmd(socketPath *string, outputFmt *string) *cobra.Command {
	var (
		replicas      int
		memory        string
		cpus          int
		env           []string
		networkName   string
		strategy      string
		healthTimeout int
	)
	cmd := &cobra.Command{
		Use:   "run <name> <image>",
		Short: "Create and start a service",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("service run: connect to daemon: %w", err)
			}
			defer client.Close()

			p := api.ServiceRunParams{
				Name:          args[0],
				Image:         args[1],
				Replicas:      replicas,
				Memory:        memory,
				CPUs:          cpus,
				Env:           env,
				NetworkName:   networkName,
				Strategy:      strategy,
				HealthTimeout: healthTimeout,
			}
			info, err := client.ServiceRun(context.Background(), p)
			if err != nil {
				return fmt.Errorf("service run: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, info)
			}
			printServiceInfo(info)
			return nil
		},
	}
	cmd.Flags().IntVar(&replicas, "replicas", 1, "number of replicas")
	cmd.Flags().StringVar(&memory, "memory", "", "memory per replica (e.g. 256M)")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "number of CPUs per replica")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "environment variables (KEY=VAL)")
	cmd.Flags().StringVar(&networkName, "network", "", "network name for replicas")
	cmd.Flags().StringVar(&strategy, "strategy", "RollingUpdate", "update strategy: RollingUpdate or Recreate")
	cmd.Flags().IntVar(&healthTimeout, "health-timeout", 0, "seconds to wait for healthy replicas (0 = no wait)")
	return cmd
}

func newServiceScaleCmd(socketPath *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale <name> <replicas>",
		Short: "Scale a service to the desired number of replicas",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			var replicas int
			if _, err := fmt.Sscanf(args[1], "%d", &replicas); err != nil {
				return fmt.Errorf("service scale: invalid replicas %q", args[1])
			}
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("service scale: connect to daemon: %w", err)
			}
			defer client.Close()
			info, err := client.ServiceScale(context.Background(), args[0], replicas)
			if err != nil {
				return fmt.Errorf("service scale: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, info)
			}
			printServiceInfo(info)
			return nil
		},
	}
	return cmd
}

func newServiceUpdateCmd(socketPath *string, outputFmt *string) *cobra.Command {
	var healthTimeout int
	cmd := &cobra.Command{
		Use:   "update <name> <image>",
		Short: "Update a service to a new image",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("service update: connect to daemon: %w", err)
			}
			defer client.Close()
			info, err := client.ServiceUpdate(context.Background(), args[0], args[1], healthTimeout)
			if err != nil {
				return fmt.Errorf("service update: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, info)
			}
			printServiceInfo(info)
			return nil
		},
	}
	cmd.Flags().IntVar(&healthTimeout, "health-timeout", 0, "seconds to wait for healthy replicas during update (0 = no wait)")
	return cmd
}

func newServiceListCmd(socketPath *string, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List services",
		Aliases: []string{"list"},
		RunE: func(_ *cobra.Command, _ []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("service ls: connect to daemon: %w", err)
			}
			defer client.Close()
			services, err := client.ServiceList(context.Background())
			if err != nil {
				return fmt.Errorf("service ls: %w", err)
			}
			if *outputFmt == "json" {
				return printJSON(os.Stdout, services)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tIMAGE\tDESIRED\tREADY\tSTRATEGY\tHEALTH")
			for _, s := range services {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n", s.Name, s.Image, s.DesiredReplicas, s.ReadyReplicas, s.Strategy, s.Health)
			}
			return w.Flush()
		},
	}
	return cmd
}

func newServiceInspectCmd(socketPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show service details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("service inspect: connect to daemon: %w", err)
			}
			defer client.Close()
			info, err := client.ServiceGet(context.Background(), args[0])
			if err != nil {
				return fmt.Errorf("service inspect: %w", err)
			}
			return printJSON(os.Stdout, info)
		},
	}
	return cmd
}

func newServiceRemoveCmd(socketPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Short:   "Remove a service",
		Args:    cobra.ExactArgs(1),
		Aliases: []string{"remove"},
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("service rm: connect to daemon: %w", err)
			}
			defer client.Close()
			if err := client.ServiceRemove(context.Background(), args[0]); err != nil {
				return fmt.Errorf("service rm: %w", err)
			}
			fmt.Println("removed", args[0])
			return nil
		},
	}
	return cmd
}

func printServiceInfo(info api.ServiceInfoResult) {
	fmt.Printf("Service:     %s\n", info.Name)
	fmt.Printf("Image:       %s\n", info.Image)
	fmt.Printf("Replicas:    %d desired, %d ready\n", info.DesiredReplicas, info.ReadyReplicas)
	fmt.Printf("Strategy:    %s\n", info.Strategy)
	fmt.Printf("Health:      %s\n", info.Health)
	fmt.Printf("Created:     %s\n", info.CreatedAt)
	fmt.Printf("Updated:     %s\n", info.UpdatedAt)
	if len(info.ReplicaIDs) > 0 {
		fmt.Printf("Replicas:    %v\n", info.ReplicaIDs)
	}
}
