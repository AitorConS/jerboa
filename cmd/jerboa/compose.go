package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/compose"
	"github.com/AitorConS/jerboa/internal/naming"
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

func newComposeCmd(socketPath, outputFmt *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Manage multi-service unikernel applications",
	}
	cmd.AddCommand(
		newComposeUpCmd(socketPath),
		newComposeDownCmd(socketPath),
		newComposePsCmd(socketPath, outputFmt),
		newComposeLogsCmd(socketPath),
	)
	return cmd
}

func newComposeUpCmd(socketPath *string) *cobra.Command {
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
			if err := scopeComposeResources(&f, composeFile); err != nil {
				return fmt.Errorf("compose up: %w", err)
			}
			resolveComposeImages(&f, composeFile)
			order, err := compose.TopologicalSort(f.Services)
			if err != nil {
				return fmt.Errorf("compose up: %w", err)
			}

			client, err := api.Dial(*socketPath)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			// A state file of a project brought up by an earlier version is
			// taken over only if it is this project's; one that is not was put
			// there by someone else and is reported as such, not as a stack
			// to take down.
			if existing, legacy, err := loadState(composeFile); err == nil {
				if legacy {
					if _, err := openState(cmd.Context(), client, composeFile, existing, legacy); err != nil {
						return fmt.Errorf("compose up: %w", err)
					}
				}
				return fmt.Errorf("compose project already has state; run down first")
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("compose up: %w", err)
			}
			state := compose.State{Project: composeProject(composeFile), Services: map[string]string{}, ServiceNetworks: map[string]string{}, ServiceIPs: map[string]string{}}
			if err := writeState(composeFile, state); err != nil {
				return fmt.Errorf("compose: %w", err)
			}
			defer func() {
				if err := writeState(composeFile, state); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("persist compose state: %w", err))
				}
			}()
			for volName, volCfg := range f.Volumes {
				if volCfg.External {
					if _, getErr := client.VolumeGet(cmd.Context(), volName); getErr != nil {
						return fmt.Errorf("compose up: external volume %q: %w", volName, getErr)
					}
					continue
				}
				if _, getErr := client.VolumeGet(cmd.Context(), volName); getErr == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "volume %s already exists, skipping\n", volName)
					continue
				} else if !api.IsNotFound(getErr) {
					return fmt.Errorf("compose up: volume %q: %w", volName, getErr)
				}
				sizeBytes, parseErr := volume.ParseSize(volCfg.DefaultSize())
				if parseErr != nil {
					return fmt.Errorf("compose up: volume %q: %w", volName, parseErr)
				}
				if _, createErr := client.VolumeCreate(cmd.Context(), volName, sizeBytes); createErr != nil {
					return fmt.Errorf("compose up: create volume %q: %w", volName, createErr)
				}
				state.CreatedVolumes = append(state.CreatedVolumes, volName)
				if err := writeState(composeFile, state); err != nil {
					return fmt.Errorf("compose: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created volume %s\n", volName)
			}

			for netName, netCfg := range f.Networks {
				driver := netCfg.Driver
				if driver == "" {
					driver = "bridge"
				}
				_, getErr := client.NetworkGet(cmd.Context(), netName)
				if netCfg.External {
					if getErr != nil {
						return fmt.Errorf("compose up: external network %q: %w", netName, getErr)
					}
					continue
				}
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
				params, buildErr := buildServiceRunParams(svc, mem)
				if buildErr != nil {
					return fmt.Errorf("compose up: service %q: %w", name, buildErr)
				}
				params.Name = name
				params.NetworkAliases = append([]string(nil), svc.Aliases...)

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
				fmt.Fprintf(cmd.OutOrStdout(), "started %s → %s\n", termField(name), termField(info.ID))

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

func newComposeDownCmd(socketPath *string) *cobra.Command {
	var force bool
	var removeVolumes bool
	cmd := &cobra.Command{
		Use:   "down <compose-file>",
		Short: "Stop all services from a compose file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, legacy, err := loadState(args[0])
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

			// Every VM is checked before any is stopped.
			vms, err := openState(cmd.Context(), client, args[0], state, legacy)
			if err != nil {
				return fmt.Errorf("compose down: %w", err)
			}
			var failures []error
			for _, name := range stateServiceNames(state) {
				id := state.Services[name]
				if err := vms[name].err; err != nil {
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
				fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", termField(name))
				delete(state.Services, name)
				delete(state.ServiceIPs, name)
				delete(state.ServiceNetworks, name)
				if err := writeState(args[0], state); err != nil {
					return fmt.Errorf("compose: %w", err)
				}
			}
			if removeVolumes {
				remaining := []string{}
				for _, name := range state.CreatedVolumes {
					err := client.VolumeRemove(cmd.Context(), name, "")
					if err != nil && !api.IsNotFound(err) {
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
			state, legacy, err := loadState(args[0])
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
			vms, err := openState(cmd.Context(), client, args[0], state, legacy)
			if err != nil {
				return fmt.Errorf("compose ps: %w", err)
			}

			type row struct {
				Service string `json:"service"`
				ID      string `json:"id"`
				State   string `json:"state"`
			}

			rows := make([]row, 0, len(state.Services))
			for _, name := range stateServiceNames(state) {
				vmState := "unknown"
				if vm := vms[name]; vm.err == nil {
					vmState = vm.info.State
				}
				rows = append(rows, row{Service: name, ID: state.Services[name], State: vmState})
			}

			if *outputFmt == "json" {
				return printJSON(cmd.OutOrStdout(), rows)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SERVICE\tID\tSTATE")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\n", termField(r.Service), termField(r.ID), termField(r.State))
			}
			return w.Flush()
		},
	}
}

func newComposeLogsCmd(socketPath *string) *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "logs <compose-file> <service>",
		Short: "Print captured serial output for a compose service",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, legacy, err := loadState(args[0])
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
			if _, err := openState(cmd.Context(), client, args[0], state, legacy); err != nil {
				return fmt.Errorf("compose logs: %w", err)
			}

			resp, err := client.Logs(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("compose logs: %w", err)
			}
			fmt.Fprint(guestOutput(cmd.OutOrStdout(), raw), resp.Logs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false,
		"print the output unfiltered, terminal control sequences included")
	return cmd
}

// --- state helpers ---

// errForeignState marks a state file that compose up did not write for the
// project it is found for. Nothing it names may be stopped or removed.
var errForeignState = errors.New("not written by compose up for this project")

func canonicalComposePath(composeFile string) string {
	canonical, err := filepath.Abs(composeFile)
	if err != nil {
		canonical = filepath.Clean(composeFile)
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}
	return canonical
}

// composeProject identifies a compose project. It is the path where earlier
// versions kept the state, next to the compose file; it is recorded as
// State.Project and hashed into the names of the project's networks and
// volumes, so it must not change.
func composeProject(composeFile string) string {
	canonical := canonicalComposePath(composeFile)
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(filepath.Dir(canonical), fmt.Sprintf(".jerboa-compose-%x.json", sum[:16]))
}

// legacyStateFilePath is where earlier versions kept a project's state: in
// the project directory, where whoever wrote the project can put one too.
func legacyStateFilePath(composeFile string) string { return composeProject(composeFile) }

// composeScopePrefix is what the project puts before the names of the
// networks and volumes it owns.
func composeScopePrefix(composeFile string) string {
	sum := sha256.Sum256([]byte(composeProject(composeFile)))
	return naming.ComposeScopedPrefix(sum[:])
}

// composeStateDir holds the state of every compose project of this user,
// where project content cannot put a file.
func composeStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".jerboa", "compose")
	}
	return filepath.Join(home, ".jerboa", "compose")
}

// stateFilePath is where compose keeps a project's state.
func stateFilePath(composeFile string) string {
	sum := sha256.Sum256([]byte(canonicalComposePath(composeFile)))
	return filepath.Join(composeStateDir(), fmt.Sprintf("%x.json", sum[:16]))
}

func writeState(composeFile string, state compose.State) error {
	state.Project = composeProject(composeFile)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("compose: marshal state: %w", err)
	}
	target := stateFilePath(composeFile)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("compose: %w", err)
	}
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

// readState reads a project's state, see loadState.
func readState(composeFile string) (compose.State, error) {
	state, _, err := loadState(composeFile)
	return state, err
}

// loadState reads a project's state from the user's compose state
// directory or, for a project brought up by an earlier version, from the
// state file next to the compose file; legacy reports the latter. Either
// file must pass checkStateFile. A legacy state is taken over (moved to the
// state directory) only once its VMs have passed checkStateVMs too.
func loadState(composeFile string) (state compose.State, legacy bool, err error) {
	state, err = readStateFile(stateFilePath(composeFile), composeFile)
	if !errors.Is(err, os.ErrNotExist) {
		return state, false, err
	}
	state, err = readStateFile(legacyStateFilePath(composeFile), composeFile)
	if errors.Is(err, os.ErrNotExist) {
		return compose.State{}, false, fmt.Errorf("no state for %s (run 'jerboa compose up' first): %w", composeFile, os.ErrNotExist)
	}
	if err != nil {
		return compose.State{}, false, err
	}
	return state, true, nil
}

func readStateFile(path, composeFile string) (compose.State, error) {
	foreign := func(reason string) error {
		return fmt.Errorf("state file %s is %w: %s; remove it if you did not create it", path, errForeignState, reason)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return compose.State{}, fmt.Errorf("read state: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return compose.State{}, foreign("it is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return compose.State{}, fmt.Errorf("read state: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Check the file that was opened, not one swapped in after the Lstat.
	opened, err := f.Stat()
	if err != nil {
		return compose.State{}, fmt.Errorf("read state: %w", err)
	}
	if !os.SameFile(fi, opened) {
		return compose.State{}, foreign("it changed while being read")
	}
	if err := checkStateFileOwner(opened); err != nil {
		return compose.State{}, foreign(err.Error())
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return compose.State{}, fmt.Errorf("read state: %w", err)
	}
	var state compose.State
	if err := json.Unmarshal(data, &state); err != nil {
		return compose.State{}, fmt.Errorf("parse state: %w", err)
	}
	if err := checkState(composeFile, state); err != nil {
		return compose.State{}, foreign(err.Error())
	}
	return state, nil
}

// checkState accepts a state only if it records this project and names no
// network or volume outside the project's scope, so that compose down cannot
// be pointed at another project's resources.
func checkState(composeFile string, state compose.State) error {
	if state.Project != composeProject(composeFile) {
		return fmt.Errorf("it records project %q", state.Project)
	}
	prefix := composeScopePrefix(composeFile)
	scoped := func(kind string, names []string) error {
		for _, name := range names {
			if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) || naming.ValidateResourceName(kind, name) != nil {
				return fmt.Errorf("%s %q is not one of this project's", kind, name)
			}
		}
		return nil
	}
	if err := scoped("volume", state.CreatedVolumes); err != nil {
		return err
	}
	if err := scoped("network", state.CreatedNetworks); err != nil {
		return err
	}
	for name, id := range state.Services {
		if id == "" {
			return fmt.Errorf("service %q has no VM", name)
		}
	}
	return nil
}

// stateVM is what the daemon answered for a VM a state names.
type stateVM struct {
	info api.VMInfo
	err  error
}

// checkStateVMs asks the daemon about each VM the state names. compose up
// records the full ID of a VM it named after a service of the compose file;
// a reference that is anything else (another VM's name or an ID prefix, or a
// service the file does not have) means the state was not written by
// compose up for this project, and nothing in it may be acted on. A VM the
// daemon does not answer for is returned with the error.
func checkStateVMs(ctx context.Context, client *api.Client, f compose.File, path string, state compose.State) (map[string]stateVM, error) {
	vms := make(map[string]stateVM, len(state.Services))
	for _, name := range stateServiceNames(state) {
		id := state.Services[name]
		if _, ok := f.Services[name]; !ok {
			return nil, fmt.Errorf("state file %s is %w: service %q is not in the compose file", path, errForeignState, name)
		}
		info, err := client.Get(ctx, id)
		if err == nil && (info.ID != id || info.Name != name) {
			return nil, fmt.Errorf("state file %s is %w: %q is not the ID of the VM of service %q", path, errForeignState, id, name)
		}
		vms[name] = stateVM{info: info, err: err}
	}
	return vms, nil
}

// openState checks the VMs a state loaded by loadState names against the
// compose file and the daemon. A legacy state that passes is moved to the
// state directory.
func openState(ctx context.Context, client *api.Client, composeFile string, state compose.State, legacy bool) (map[string]stateVM, error) {
	f, err := readComposeFile(composeFile)
	if err != nil {
		return nil, err
	}
	path := stateFilePath(composeFile)
	if legacy {
		path = legacyStateFilePath(composeFile)
	}
	vms, err := checkStateVMs(ctx, client, f, path, state)
	if err != nil {
		return nil, err
	}
	if legacy {
		if err := writeState(composeFile, state); err != nil {
			return nil, err
		}
		_ = os.Remove(legacyStateFilePath(composeFile))
	}
	return vms, nil
}

// readComposeFile reads and parses a compose file.
func readComposeFile(composeFile string) (compose.File, error) {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return compose.File{}, fmt.Errorf("read file: %w", err)
	}
	return compose.Parse(data)
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
func buildServiceRunParams(svc compose.Service, mem string) (api.RunParams, error) {
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
	volSpecs, err := resolveVolumes(svc.Volumes)
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
func parseComposePortSpec(s string) (api.PortMapSpec, error) { return api.ParsePortMap(s) }

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

// resolveComposeImages makes image file paths relative to the compose file,
// not to the current directory, as Docker Compose does for paths.
func resolveComposeImages(f *compose.File, path string) {
	dir := filepath.Dir(path)
	for key, svc := range f.Services {
		img := svc.Image
		if img == "" || filepath.IsAbs(img) {
			continue
		}
		candidate := filepath.Join(dir, img)
		if _, err := os.Stat(candidate); isFilePath(img) || err == nil {
			if abs, err := filepath.Abs(candidate); err == nil {
				svc.Image = abs
				f.Services[key] = svc
			}
		}
	}
}

// scopeComposeResources renames the networks and volumes the compose file
// owns to names scoped to its project, as Docker Compose does, so a compose
// file cannot attach a volume or join a network of another project by naming
// it. Only entries marked external keep their name and must already exist.
func scopeComposeResources(f *compose.File, path string) error {
	prefix := composeScopePrefix(path)
	scoped := func(kind, name string) (string, error) {
		n := prefix + name
		if err := naming.ValidateResourceName(kind, n); err != nil {
			return "", fmt.Errorf("%s %q: name too long for a project-scoped name: %w", kind, name, err)
		}
		return n, nil
	}
	networks := map[string]string{}
	for name, n := range f.Networks {
		if n.External {
			continue
		}
		s, err := scoped("network", name)
		if err != nil {
			return err
		}
		networks[name] = s
	}
	volumes := map[string]string{}
	for name, v := range f.Volumes {
		if v.External {
			continue
		}
		s, err := scoped("volume", name)
		if err != nil {
			return err
		}
		volumes[name] = s
	}
	for from, to := range networks {
		f.Networks[to] = f.Networks[from]
		delete(f.Networks, from)
	}
	for from, to := range volumes {
		f.Volumes[to] = f.Volumes[from]
		delete(f.Volumes, from)
	}
	for key, svc := range f.Services {
		for i, network := range svc.Networks {
			if to, ok := networks[network]; ok {
				svc.Networks[i] = to
			}
		}
		for i, spec := range svc.Volumes {
			name, rest, _ := strings.Cut(spec, ":")
			if to, ok := volumes[name]; ok {
				svc.Volumes[i] = to + ":" + rest
			}
		}
		f.Services[key] = svc
	}
	return nil
}
