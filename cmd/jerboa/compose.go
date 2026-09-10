package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/compose"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/spf13/cobra"
)

// removeWhenStopped removes a VM, retrying briefly while a graceful stop is still
// completing. `client.Stop` can return while the VM is transitioning through
// "stopping" (the monitor flips it to "stopped" once the process exits), so an
// immediate Remove races with that and fails "must be stopped first".
func removeWhenStopped(ctx context.Context, client *api.Client, id string) error {
	var err error
	for i := 0; i < 25; i++ {
		if err = client.Remove(ctx, id); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "must be stopped first") {
			return fmt.Errorf("compose: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("compose: %w", err)
}

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
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
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

			if _, err := os.Stat(stateFilePath(composeFile)); err == nil {
				return fmt.Errorf("compose project already has state; run down first")
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("compose: %w", err)
			}
			state := compose.State{Project: stateFilePath(composeFile), Services: map[string]string{}, ServiceNetworks: map[string]string{}, ServiceIPs: map[string]string{}}
			if err := writeState(composeFile, state); err != nil {
				return fmt.Errorf("compose: %w", err)
			}
			defer func() {
				if err := writeState(composeFile, state); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("persist compose state: %w", err))
				}
			}()
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
				state.CreatedVolumes = append(state.CreatedVolumes, volName)
				if err := writeState(composeFile, state); err != nil {
					return fmt.Errorf("compose: %w", err)
				}
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
				state.CreatedNetworks = append(state.CreatedNetworks, netName)
				if err := writeState(composeFile, state); err != nil {
					return fmt.Errorf("compose: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created network %s\n", netName)
			}

			for _, name := range order {
				svc := f.Services[name]

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
					ip := svc.IP
					if ip != "" {
						if err := validateStaticIP(ip, netInfo.Subnet); err != nil {
							return fmt.Errorf("compose: %w", err)
						}
					}
					params.IPAddress = ip
					// A compose-declared static IP is reserved and conflict-checked
					// by the daemon, which also allocates dynamic addresses.
					params.StaticIP = svc.IP != ""
					state.ServiceNetworks[name] = netName
					state.ServiceIPs[name] = ip
				}

				info, runErr := client.Run(cmd.Context(), params)
				if runErr != nil {
					return fmt.Errorf("compose up: service %q: %w", name, runErr)
				}
				state.Services[name] = info.ID
				if err := writeState(composeFile, state); err != nil {
					return fmt.Errorf("compose: %w", err)
				}
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

// validateStaticIP checks that a compose service's static IP is a valid address
// inside the network's subnet (a CIDR like "10.100.0.0/24"). It does not detect
// collisions with dynamically allocated addresses — the daemon's IPAM does not
// track externally assigned IPs yet.
func validateStaticIP(ip, subnet string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("%q is not a valid IP address", ip)
	}
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("network subnet %q is not valid CIDR: %w", subnet, err)
	}
	if !ipNet.Contains(parsed) {
		return fmt.Errorf("%q is outside the network subnet %s", ip, subnet)
	}
	return nil
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
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
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

			var failures []error
			for _, name := range stateServiceNames(state) {
				id := state.Services[name]
				if _, err := client.Get(cmd.Context(), id); err != nil {
					if !strings.Contains(err.Error(), "not found") {
						failures = append(failures, err)
						continue
					}
				} else {
					if err := client.Stop(cmd.Context(), id, force); err != nil {
						failures = append(failures, err)
						continue
					}
					if err := removeWhenStopped(cmd.Context(), client, id); err != nil {
						failures = append(failures, err)
						continue
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", name)
				delete(state.Services, name)
				delete(state.ServiceIPs, name)
				delete(state.ServiceNetworks, name)
				if err := writeState(args[0], state); err != nil {
					return fmt.Errorf("compose: %w", err)
				}
			}
			if removeVolumes {
				store, err := volume.NewStore(volumeStorePath(*storePath))
				if err != nil {
					return fmt.Errorf("compose: %w", err)
				}
				remaining := []string{}
				for _, name := range state.CreatedVolumes {
					vol, err := store.Get(name)
					if err == nil {
						err = client.VolumeRemove(cmd.Context(), name, hostPathForDaemon(vol.DiskPath))
					}
					if err != nil && !os.IsNotExist(err) {
						remaining = append(remaining, name)
						failures = append(failures, err)
					}
				}
				state.CreatedVolumes = remaining
			}
			remaining := []string{}
			for _, name := range state.CreatedNetworks {
				if err := client.NetworkRemove(cmd.Context(), name); err != nil && !strings.Contains(err.Error(), "not found") {
					remaining = append(remaining, name)
					failures = append(failures, err)
				}
			}
			state.CreatedNetworks = remaining
			if err := writeState(args[0], state); err != nil {
				return fmt.Errorf("compose: %w", err)
			}
			if len(failures) > 0 {
				return errors.Join(failures...)
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
	canonical, err := filepath.Abs(composeFile)
	if err != nil {
		canonical = filepath.Clean(composeFile)
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(filepath.Dir(canonical), fmt.Sprintf(".jerboa-compose-%x.json", sum[:16]))
}

func writeState(composeFile string, state compose.State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("compose: marshal state: %w", err)
	}
	target := stateFilePath(composeFile)
	f, err := os.CreateTemp(filepath.Dir(target), ".compose-state-*")
	if err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("compose: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("compose: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	return nil
}

func readState(composeFile string) (compose.State, error) {
	data, err := os.ReadFile(stateFilePath(composeFile))
	if err != nil {
		return compose.State{}, fmt.Errorf("read state (run 'jerboa compose up' first): %w", err)
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
