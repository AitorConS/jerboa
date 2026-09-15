//go:build darwin && arm64

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	nativeFCGuestShutdownTimeout = 35 * time.Second
	nativeFCStopGracePeriod      = 40 * time.Second
	nativeFCControlTimeout       = 2 * time.Second
)

func platformInitFC(m *FirecrackerManager) {
	m.vmSockPath = nativeFCSocketPath
	m.shutdownAPI = nativeFCShutdown
	m.shutdownGrace = nativeFCStopGracePeriod
}

func nativeFCSocketPath(id string) string { return filepath.Join(qmpRuntimeDir(), "fc-"+id+".sock") }

func (m *FirecrackerManager) RestoreHostRuntime(ctx context.Context) error {
	for _, v := range m.List() {
		if v.GetState() != StateRunning {
			continue
		}
		socket := m.vmSockPath(v.ID)
		if err := awaitFCReachable(ctx, socket); err != nil {
			return fmt.Errorf("restore Firecracker %s: %w", v.ID, err)
		}
		cleanup, err := m.prepareFCHost(v)
		if err != nil {
			return err
		}
		prepareNativeFCHealth(v)
		m.hchecker.Start(ctx, v)
		v.SetStatsProvider(newStatsCollector(v.pid, v).Collect)
		v.mu.Lock()
		v.hostCleanup = func() {
			if cleanup != nil {
				cleanup()
			}
			m.hchecker.Stop(v.ID)
			_ = os.Remove(socket)
			_ = os.Remove(fcRootfsPath(v.ID))
			_ = os.Remove(m.vmmLogPath(v.ID))
			paths, _ := filepath.Glob(filepath.Join(os.TempDir(), "jerboa-fc-"+v.ID+"-*", "config.json"))
			for _, p := range paths {
				cleanupFCConfig(p)
			}
		}
		proc := v.proc
		v.mu.Unlock()
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-v.Done():
					return
				case <-ticker.C:
					s, err := readNativeFCState(ctx, socket)
					if err == nil && (s.State == "Exited" || s.State == "Failed") {
						_ = proc.signal(syscall.SIGTERM)
						return
					}
				}
			}
		}()
	}
	return nil
}

type nativeFCState struct {
	State     string          `json:"state"`
	ExitCode  *int            `json:"exit_code"`
	LastError json.RawMessage `json:"last_error"`
}

// nativeFCShutdown requests the fork's guest-visible VirtIO power button and
// asks its supervisor to terminate a non-cooperative VMM after a bounded grace
// period. The outer manager retains its own kill fallback as a second bound.
func nativeFCShutdown(socket string) error {
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	body := strings.NewReader(fmt.Sprintf(`{"action_type":"Shutdown","timeout_ms":%d,"force_on_timeout":true}`,
		nativeFCGuestShutdownTimeout.Milliseconds()))
	ctx, cancel := context.WithTimeout(context.Background(), nativeFCControlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/actions", body)
	if err != nil {
		return fmt.Errorf("HVF shutdown request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("HVF shutdown: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HVF shutdown: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (m *FirecrackerManager) checkFCConfig(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := m.mkCmd(ctx, m.fcBin, "--no-api", "--config-file", path, "--check-config").CombinedOutput()
	if err != nil {
		return fmt.Errorf("HVF configuration rejected: %w: %s", err, out)
	}
	return nil
}

func readNativeFCState(ctx context.Context, socket string) (nativeFCState, error) {
	var state nativeFCState
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if err != nil {
		return state, err
	}
	resp, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(req)
	if err != nil {
		return state, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return state, fmt.Errorf("HVF status: HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&state)
	return state, err
}

// Adoption only requires a reachable supervisor: a user may have paused the
// guest through the VMM API while the daemon was down.
func awaitFCReachable(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		_, last = readNativeFCState(ctx, socket)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("HVF supervisor unavailable: %w", last)
		case <-ticker.C:
		}
	}
}

func awaitFCReady(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err := readNativeFCState(ctx, socket)
		if err == nil {
			switch s.State {
			case "Running", "Exited":
				return nil
			case "Failed":
				return fmt.Errorf("HVF boot failed: %s", s.LastError)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for HVF boot: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// The API supervisor outlives the guest. Reap it after guest exit and retain
// the guest's result so RestartOnFailure does not use the supervisor's status.
func waitFCProcess(cmd *exec.Cmd, socket string) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var guestErr error
	terminal := false
	for {
		select {
		case err := <-done:
			if terminal {
				return guestErr
			}
			return err
		case <-ticker.C:
			if terminal {
				continue
			}
			s, err := readNativeFCState(context.Background(), socket)
			if err != nil || (s.State != "Exited" && s.State != "Failed") {
				continue
			}
			terminal = true
			if s.State == "Failed" || s.ExitCode == nil || *s.ExitCode != 0 {
				code := -1
				if s.ExitCode != nil {
					code = *s.ExitCode
				}
				guestErr = fmt.Errorf("HVF guest exit: %s, code %d, error %s", s.State, code, s.LastError)
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}
}

func (m *FirecrackerManager) validateFCPlatform(cfg Config) error {
	if cfg.EmulateX86 {
		return fmt.Errorf("Firecracker/HVF does not support x86 emulation")
	}
	if err := validateHostConfig(cfg, m.kernelImage); err != nil {
		return err
	}
	mem, err := parseMiB(cfg.Memory)
	if err != nil {
		return err
	}
	if cfg.CPUs > 4 || mem < 64 || mem > 2048 {
		return fmt.Errorf("Firecracker/HVF requires 1-4 CPUs and 64-2048 MiB RAM")
	}
	if len(cfg.Volumes) > 3 {
		return fmt.Errorf("Firecracker/HVF supports at most three volumes plus the root disk")
	}
	if cfg.NetworkName == "" && cfg.IPAddress != "" {
		return fmt.Errorf("custom guest IP requires a named network")
	}
	if cfg.NetworkName != "" {
		mask := cfg.SubnetMask
		if mask == "" {
			mask = "24"
		}
		ip, subnet, err := net.ParseCIDR(cfg.IPAddress + "/" + mask)
		if err != nil || ip.To4() == nil || !subnet.Contains(net.ParseIP(cfg.GatewayIP)) {
			return fmt.Errorf("shared network requires a valid IPv4 address, subnet and gateway")
		}
	}
	if cfg.TapName != "" {
		return fmt.Errorf("Firecracker/HVF does not support TAP")
	}
	if len(cfg.PortMaps) > 64 {
		return fmt.Errorf("Firecracker/HVF supports at most 64 port mappings")
	}
	if cfg.CPUShares > 0 || cfg.MemoryMax > 0 {
		return fmt.Errorf("Firecracker/HVF does not support Jerboa host CPU/memory limits")
	}
	if cfg.DiskIOPS > 1000000 || cfg.DiskBPS < 0 || (cfg.DiskBPS > 0 && (cfg.DiskBPS < 1<<20 || cfg.DiskBPS > 1<<40)) {
		return fmt.Errorf("Firecracker/HVF disk limits require 1-1000000 IOPS and 1 MiB/s-1 TiB/s")
	}
	if cfg.HealthCheck != nil && cfg.NetworkName == "" {
		for _, p := range cfg.PortMaps {
			if int(p.GuestPort) == cfg.HealthCheck.Port && p.Protocol != ProtocolUDP {
				return nil
			}
		}
		return fmt.Errorf("Firecracker/HVF health checks require a published TCP guest port")
	}
	return nil
}

type nativeFCForward struct {
	Protocol  string `json:"protocol"`
	HostAddr  string `json:"host_addr"`
	HostPort  uint16 `json:"host_port"`
	GuestAddr string `json:"guest_addr"`
	GuestPort uint16 `json:"guest_port"`
}

type nativeFCListener struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     uint16 `json:"port"`
}

type nativeFCNetwork struct {
	IfaceID    string            `json:"iface_id"`
	Backend    string            `json:"backend"`
	SocketPath string            `json:"socket_path,omitempty"`
	GuestMAC   string            `json:"guest_mac"`
	Forwards   []nativeFCForward `json:"forwards,omitempty"`
}

type nativeFCConfig struct {
	BootSource struct {
		Kernel   string `json:"kernel_image_path"`
		Protocol string `json:"boot_protocol"`
	} `json:"boot-source"`
	Machine struct {
		CPUs        int  `json:"vcpu_count"`
		Memory      int  `json:"mem_size_mib"`
		PowerButton bool `json:"power_button"`
	} `json:"machine-config"`
	Drives   []fcDrive                  `json:"drives"`
	Firmware map[string]string          `json:"firmware"`
	Networks []nativeFCNetwork          `json:"network-interfaces"`
	Security map[string]json.RawMessage `json:"security"`
	Limits   struct {
		Version  int   `json:"version"`
		DiskBPS  int64 `json:"disk_bytes_per_second,omitempty"`
		DiskIOPS int64 `json:"disk_operations_per_second,omitempty"`
	} `json:"limits"`
}

func (m *FirecrackerManager) writeNativeFCConfig(id string, cfg Config, rootfs string) (path string, err error) {
	dir, err := os.MkdirTemp("", "jerboa-fc-"+id+"-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	c := nativeFCConfig{Firmware: map[string]string{}}
	c.BootSource.Kernel, err = filepath.Abs(m.kernelImage)
	if err != nil {
		return "", err
	}
	c.BootSource.Protocol = "elf"
	c.Machine.CPUs = cfg.CPUs
	if c.Machine.CPUs == 0 {
		c.Machine.CPUs = 1
	}
	c.Machine.Memory, err = parseMiB(cfg.Memory)
	if err != nil {
		return "", err
	}
	c.Machine.PowerButton = true
	c.Drives = []fcDrive{{DriveID: "rootfs", PathOnHost: rootfs, IsRootDevice: true}}
	for i, vol := range cfg.Volumes {
		p, err := filepath.Abs(vol.DiskPath)
		if err != nil {
			return "", err
		}
		c.Drives = append(c.Drives, fcDrive{DriveID: fmt.Sprintf("vol%d", i), PathOnHost: p, IsReadOnly: vol.ReadOnly})
	}
	var mounts []string
	for _, vol := range cfg.Volumes {
		if vol.Label != "" && vol.GuestPath != "" {
			mounts = append(mounts, vol.Label+":"+vol.GuestPath)
		}
	}
	netcfg := "10.0.2.15/24,10.0.2.2"
	if cfg.NetworkName != "" {
		mask := cfg.SubnetMask
		if mask == "" {
			mask = "24"
		}
		netcfg = cfg.IPAddress + "/" + mask + "," + cfg.GatewayIP
	}
	for name, data := range map[string]string{"env": strings.Join(cfg.Env, "\n"), "mounts": strings.Join(mounts, "\n"), "network": netcfg} {
		if len(data) > 65536 {
			return "", fmt.Errorf("Firecracker/HVF firmware %s exceeds 64 KiB", name)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			return "", err
		}
		c.Firmware["opt/uni/"+name] = p
	}
	n := nativeFCNetwork{IfaceID: "eth0", Backend: "slirp", GuestMAC: guestMACFromIP("10.0.2.15")}
	c.Security = map[string]json.RawMessage{"version": json.RawMessage("1")}
	if len(m.fcSecurity) > 0 {
		if err := json.Unmarshal(m.fcSecurity, &c.Security); err != nil {
			return "", fmt.Errorf("HVF security: %w", err)
		}
		if c.Security == nil {
			return "", fmt.Errorf("HVF security must be a JSON object")
		}
	}
	var listeners []nativeFCListener
	if data := c.Security["listeners"]; len(data) > 0 {
		if err := json.Unmarshal(data, &listeners); err != nil {
			return "", fmt.Errorf("HVF listeners: %w", err)
		}
	}
	for _, p := range cfg.PortMaps {
		proto := string(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		addr := p.BindAddr
		if addr == "" {
			// Match the CLI/shared-network contract: omitted bind publishes on
			// all IPv4 interfaces. The listener grants only this exact bind;
			// it does not grant any egress or DNS authority.
			addr = "0.0.0.0"
		}
		n.Forwards = append(n.Forwards, nativeFCForward{proto, addr, p.HostPort, "10.0.2.15", p.GuestPort})
		listeners = append(listeners, nativeFCListener{proto, addr, p.HostPort})
	}
	if len(listeners) > 0 {
		c.Security["listeners"], err = json.Marshal(listeners)
		if err != nil {
			return "", err
		}
	}
	if cfg.NetworkName != "" {
		if cfg.nativeSocket == "" {
			return "", fmt.Errorf("shared network has no prepared socket")
		}
		n = nativeFCNetwork{IfaceID: "eth0", Backend: "unix-stream", GuestMAC: cfg.nativeMAC, SocketPath: cfg.nativeSocket}
		socket, _ := json.Marshal(cfg.nativeSocket)
		c.Security = map[string]json.RawMessage{"version": json.RawMessage("1"), "unix_stream": socket}
	}
	c.Networks = []nativeFCNetwork{n}
	c.Limits.Version = 1
	c.Limits.DiskBPS = int64(cfg.DiskBPS)
	c.Limits.DiskIOPS = int64(cfg.DiskIOPS)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	path = filepath.Join(dir, "config.json")
	err = os.WriteFile(path, data, 0600)
	return path, err
}

func cleanupFCConfig(path string) { _ = os.RemoveAll(filepath.Dir(path)) }

func prepareNativeFCHealth(v *VM) {
	v.AddWarning("Firecracker/HVF stop requests guest shutdown; forced timeout or kill cannot guarantee guest filesystem flush")
	if v.Cfg.HealthCheck == nil || v.Cfg.NetworkName != "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.healthAddress = "10.0.2.15"
	ports := append([]PortMap(nil), v.Cfg.PortMaps...)
	v.healthDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		for _, p := range ports {
			if strconv.Itoa(int(p.GuestPort)) != port || p.Protocol == ProtocolUDP {
				continue
			}
			addr := p.BindAddr
			if addr == "" || addr == "0.0.0.0" {
				addr = "127.0.0.1"
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(addr, strconv.Itoa(int(p.HostPort))))
		}
		return nil, fmt.Errorf("health port %s is not published", port)
	}
}
