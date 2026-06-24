package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/compose"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/spf13/cobra"
)

const stateFileName = ".uni-compose-state.json"

func newComposeCmd(socketPath, storePath, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Manage multi-service unikernel applications",
	}
	cmd.AddCommand(
		newComposeUpCmd(socketPath, storePath),
		newComposeDownCmd(socketPath, storePath),
		newComposePsCmd(socketPath, outputFmt),
		newComposeLogsCmd(socketPath),
	)
	return cmd
}

func newComposeUpCmd(socketPath, storePath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "up <compose-file>",
		Short: "Start all services defined in a compose file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			composeFile := args[0]
			data, err := os.ReadFile(composeFile)
			if err != nil {
				return fmt.Errorf("compose up: read file: %w", err)
			}
			f, err := compose.Parse(data)
			if err != nil {
				return fmt.Errorf("compose up: %w", err)
			}
			order, err := compose.TopologicalSort(f.Services)
			if err != nil {
				return fmt.Errorf("compose up: %w", err)
			}

			volPath := volumeStorePath(*storePath)
			volStore, err := volume.NewStore(volPath)
			if err != nil {
				return fmt.Errorf("compose up: open volume store: %w", err)
			}

			var createdVolumes []string
			for volName, volCfg := range f.Volumes {
				if _, getErr := volStore.Get(volName); getErr == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "volume %s already exists, skipping\n", volName)
					continue
				}
				sizeBytes, parseErr := volume.ParseSize(volCfg.DefaultSize())
				if parseErr != nil {
					return fmt.Errorf("compose up: volume %q: %w", volName, parseErr)
				}
				if _, createErr := volStore.Create(volName, sizeBytes); createErr != nil {
					return fmt.Errorf("compose up: create volume %q: %w", volName, createErr)
				}
				createdVolumes = append(createdVolumes, volName)
				fmt.Fprintf(cmd.OutOrStdout(), "created volume %s\n", volName)
			}

			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("compose up: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			var createdNetworks []string
			for netName, netCfg := range f.Networks {
				driver := netCfg.Driver
				if driver == "" {
					driver = "bridge"
				}
				_, getErr := client.NetworkGet(cmd.Context(), netName)
				if getErr == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "network %s already exists, skipping\n", netName)
					continue
				}
				_, createErr := client.NetworkCreate(cmd.Context(), netName, netCfg.Subnet, driver)
				if createErr != nil {
					return fmt.Errorf("compose up: create network %q: %w", netName, createErr)
				}
				createdNetworks = append(createdNetworks, netName)
				fmt.Fprintf(cmd.OutOrStdout(), "created network %s\n", netName)
			}

			state := compose.State{
				Project:          filepath.Base(filepath.Dir(composeFile)),
				Services:         make(map[string]string, len(f.Services)),
				ServiceNetworks:  make(map[string]string, len(f.Services)),
				ServiceIPs:       make(map[string]string, len(f.Services)),
				CreatedVolumes:   createdVolumes,
				CreatedNetworks:  createdNetworks,
				ScalableServices: make(map[string]string, len(f.Services)),
			}

			for _, name := range order {
				svc := f.Services[name]

				if svc.Replicas > 1 {
					serviceInfo, svcErr := client.ServiceRun(cmd.Context(), api.ServiceRunParams{
						Name:        name,
						Image:       svc.Image,
						Replicas:    svc.Replicas,
						Memory:      svc.Memory,
						CPUs:        svc.CPUs,
						Env:         svc.Environment,
						NetworkName: firstNetwork(svc.Networks),
						Strategy:    svc.Strategy,
					})
					if svcErr != nil {
						return fmt.Errorf("compose up: service %q: %w", name, svcErr)
					}
					state.Services[name] = serviceInfo.Name
					state.ScalableServices[name] = name
					fmt.Fprintf(cmd.OutOrStdout(), "started service %s (%d replicas)\n", name, svc.Replicas)
					continue
				}

				mem := svc.Memory
				if mem == "" {
					mem = "256M"
				}
				params, buildErr := buildServiceRunParams(svc, mem, *storePath)
				if buildErr != nil {
					return fmt.Errorf("compose up: service %q: %w", name, buildErr)
				}
				params.Name = name

				if len(svc.Networks) > 0 {
					netName := svc.Networks[0]
					netInfo, netErr := client.NetworkGet(cmd.Context(), netName)
					if netErr != nil {
						return fmt.Errorf("compose up: service %q network %q: %w", name, netName, netErr)
					}
					params.NetworkName = netName
					params.BridgeName = netInfo.Bridge
					params.GatewayIP = netInfo.Gateway
					params.SubnetMask = extractMask(netInfo.Subnet)
					ip, allocErr := client.NetworkAllocateIP(cmd.Context(), netName)
					if allocErr != nil {
						return fmt.Errorf("compose up: service %q allocate ip: %w", name, allocErr)
					}
					params.IPAddress = ip
					state.ServiceNetworks[name] = netName
					state.ServiceIPs[name] = ip
				}

				info, runErr := client.Run(cmd.Context(), params)
				if runErr != nil {
					return fmt.Errorf("compose up: service %q: %w", name, runErr)
				}
				state.Services[name] = info.ID
				fmt.Fprintf(cmd.OutOrStdout(), "started %s → %s\n", name, info.ID)

				if svc.HealthCheck != "" {
					if err := waitForHealthy(cmd, client, info.ID, name, 60*time.Second); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: service %q not healthy: %v\n", name, err)
					}
				}
			}

			return writeState(composeFile, state)
		},
	}
}

func newComposeDownCmd(socketPath, storePath *string) *cobra.Command {
	var force bool
	var removeVolumes bool
	cmd := &cobra.Command{
		Use:   "down <compose-file>",
		Short: "Stop all services from a compose file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := readState(args[0])
			if err != nil {
				return fmt.Errorf("compose down: %w", err)
			}

			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("compose down: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			names := stateServiceNames(state)
			for i := len(names) - 1; i >= 0; i-- {
				name := names[i]

				if _, scalable := state.ScalableServices[name]; scalable {
					if rmErr := client.ServiceRemove(cmd.Context(), name); rmErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove service %s: %v\n", name, rmErr)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "removed service %s\n", name)
					releaseNetwork := state.ServiceNetworks[name]
					if releaseNetwork != "" {
						if relErr := client.NetworkRemove(cmd.Context(), releaseNetwork); relErr != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove network %s: %v\n", releaseNetwork, relErr)
						}
					}
					continue
				}

				id := state.Services[name]
				releaseNetwork := state.ServiceNetworks[name]
				releaseIP := state.ServiceIPs[name]
				if releaseNetwork == "" || releaseIP == "" {
					rec, dnsErr := client.DNSResolve(cmd.Context(), name, "")
					if dnsErr == nil {
						releaseNetwork = rec.Network
						releaseIP = rec.IP
					}
				}
				if stopErr := client.Stop(cmd.Context(), id, force); stopErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: stop %s (%s): %v\n", name, id, stopErr)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", name)
				if releaseNetwork != "" && releaseIP != "" {
					if relErr := client.NetworkReleaseIP(cmd.Context(), releaseNetwork, releaseIP); relErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: release ip for %s (%s): %v\n", name, releaseIP, relErr)
					}
				}
			}

			if removeVolumes && len(state.CreatedVolumes) > 0 {
				volPath := volumeStorePath(*storePath)
				volStore, volErr := volume.NewStore(volPath)
				if volErr != nil {
					return fmt.Errorf("compose down: open volume store: %w", volErr)
				}
				for _, volName := range state.CreatedVolumes {
					if rmErr := volStore.Remove(volName); rmErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove volume %s: %v\n", volName, rmErr)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "removed volume %s\n", volName)
				}
			}

			for _, netName := range state.CreatedNetworks {
				if rmErr := client.NetworkRemove(cmd.Context(), netName); rmErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove network %s: %v\n", netName, rmErr)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "removed network %s\n", netName)
			}

			return removeState(args[0])
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "send SIGKILL immediately")
	cmd.Flags().BoolVar(&removeVolumes, "volumes", false, "remove volumes created by compose up")
	return cmd
}

func newComposePsCmd(socketPath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ps <compose-file>",
		Short: "List services and their VM state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := readState(args[0])
			if err != nil {
				return fmt.Errorf("compose ps: %w", err)
			}

			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("compose ps: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			type row struct {
				Service string `json:"service"`
				ID      string `json:"id"`
				State   string `json:"state"`
			}

			rows := make([]row, 0, len(state.Services))
			for _, name := range stateServiceNames(state) {
				id := state.Services[name]
				info, getErr := client.Get(cmd.Context(), id)
				vmState := "unknown"
				if getErr == nil {
					vmState = info.State
				}
				rows = append(rows, row{Service: name, ID: id, State: vmState})
			}

			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), rows)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SERVICE\tID\tSTATE")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\n", r.Service, r.ID, r.State)
			}
			return w.Flush()
		},
	}
}

func newComposeLogsCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "logs <compose-file> <service>",
		Short: "Print captured serial output for a compose service",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := readState(args[0])
			if err != nil {
				return fmt.Errorf("compose logs: %w", err)
			}
			id, ok := state.Services[args[1]]
			if !ok {
				return fmt.Errorf("compose logs: service %q not found in state", args[1])
			}

			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("compose logs: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			resp, err := client.Logs(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("compose logs: %w", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), resp.Logs)
			return nil
		},
	}
}

// --- state helpers ---

func stateFilePath(composeFile string) string {
	return filepath.Join(filepath.Dir(composeFile), stateFileName)
}

func writeState(composeFile string, state compose.State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("compose: marshal state: %w", err)
	}
	if err := os.WriteFile(stateFilePath(composeFile), data, 0o600); err != nil {
		return fmt.Errorf("compose: write state: %w", err)
	}
	return nil
}

func readState(composeFile string) (compose.State, error) {
	data, err := os.ReadFile(stateFilePath(composeFile))
	if err != nil {
		return compose.State{}, fmt.Errorf("read state (run 'uni compose up' first): %w", err)
	}
	var state compose.State
	if err := json.Unmarshal(data, &state); err != nil {
		return compose.State{}, fmt.Errorf("parse state: %w", err)
	}
	return state, nil
}

func removeState(composeFile string) error {
	if err := os.Remove(stateFilePath(composeFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("compose: remove state: %w", err)
	}
	return nil
}

// stateServiceNames returns service names in a deterministic sorted order.
func stateServiceNames(state compose.State) []string {
	names := make([]string, 0, len(state.Services))
	for name := range state.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// buildServiceRunParams converts a compose.Service into an api.RunParams. The
// service image is sent as a reference (resolved by the daemon) or a direct
// path, mirroring the run command.
func buildServiceRunParams(svc compose.Service, mem, storePath string) (api.RunParams, error) {
	imageRef, imagePath, err := splitImageArg(svc.Image)
	if err != nil {
		return api.RunParams{}, fmt.Errorf("image: %w", err)
	}
	params := api.RunParams{
		Image:     imageRef,
		ImagePath: imagePath,
		Memory:    mem,
		CPUs:      svc.CPUs,
		Env:       svc.Environment,
	}
	for _, portSpec := range svc.Ports {
		pm, err := parseComposePortSpec(portSpec)
		if err != nil {
			return api.RunParams{}, fmt.Errorf("ports: %w", err)
		}
		params.PortMaps = append(params.PortMaps, pm)
	}
	volSpecs, err := resolveVolumes(svc.Volumes, storePath)
	if err != nil {
		return api.RunParams{}, fmt.Errorf("volumes: %w", err)
	}
	params.Volumes = volSpecs
	if svc.HealthCheck != "" {
		hc, err := parseHealthCheck(svc.HealthCheck)
		if err != nil {
			return api.RunParams{}, fmt.Errorf("health_check: %w", err)
		}
		params.HealthCheck = &hc
	}
	if svc.Restart != "" {
		rs, err := parseRestartPolicy(svc.Restart)
		if err != nil {
			return api.RunParams{}, fmt.Errorf("restart: %w", err)
		}
		params.Restart = &rs
	}
	return params, nil
}

// parseComposePortSpec converts "host:guest[/proto]" to a PortMapSpec.
func parseComposePortSpec(s string) (api.PortMapSpec, error) {
	pm, err := parseVolumePortString(s)
	if err != nil {
		return api.PortMapSpec{}, err
	}
	return api.PortMapSpec{
		HostPort:  pm.HostPort,
		GuestPort: pm.GuestPort,
		Protocol:  string(pm.Protocol),
	}, nil
}

const healthCheckInterval = 500 * time.Millisecond

func waitForHealthy(cmd *cobra.Command, client *api.Client, id, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := client.Get(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("get vm status: %w", err)
		}
		if info.Health == "healthy" {
			fmt.Fprintf(cmd.OutOrStdout(), "service %s is healthy\n", name)
			return nil
		}
		if info.Health == "unhealthy" {
			return fmt.Errorf("service %s is unhealthy", name)
		}
		time.Sleep(healthCheckInterval)
	}
	return fmt.Errorf("timed out waiting for %s to become healthy", name)
}

func firstNetwork(networks []string) string {
	if len(networks) > 0 {
		return networks[0]
	}
	return ""
}
