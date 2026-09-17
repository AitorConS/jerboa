//go:build darwin && arm64

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNativeFCConfig(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "/kernel.img")
	cfg := Config{Memory: "256M", CPUs: 4, Env: []string{"MARKER=hvf-smoke"},
		PortMaps: []PortMap{{HostPort: 18080, GuestPort: 8080, BindAddr: "127.0.0.1"}, {HostPort: 18081, GuestPort: 8081, Protocol: ProtocolUDP}},
		Volumes:  []VolumeMount{{DiskPath: "/data.img", Label: "data", GuestPath: "/data", ReadOnly: true}}}
	p, err := m.writeFCConfig("test", cfg, "/private-root.img")
	require.NoError(t, err)
	t.Cleanup(func() { cleanupFCConfig(p) })
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	var c nativeFCConfig
	require.NoError(t, json.Unmarshal(data, &c))
	require.Equal(t, "elf", c.BootSource.Protocol)
	require.True(t, c.Machine.PowerButton)
	require.NotContains(t, string(data), "boot_args")
	require.NotContains(t, string(data), "host_dev_name")
	require.Equal(t, "/private-root.img", c.Drives[0].PathOnHost)
	require.True(t, c.Drives[1].IsReadOnly)
	require.Equal(t, "udp", c.Networks[0].Forwards[1].Protocol)
	var listeners []nativeFCListener
	require.NoError(t, json.Unmarshal(c.Security["listeners"], &listeners))
	require.Equal(t, uint16(18080), listeners[0].Port)
	for key, want := range map[string]string{"env": "MARKER=hvf-smoke", "mounts": "data:/data", "network": "10.0.2.15/24,10.0.2.2"} {
		b, err := os.ReadFile(c.Firmware["opt/uni/"+key])
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	}
	info, err := os.Stat(filepath.Dir(p))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	cleanupFCConfig(p)
	_, err = os.Stat(filepath.Dir(p))
	require.True(t, os.IsNotExist(err))
}

func TestNativeFCShutdown(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "fc.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan map[string]any, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/actions", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		received <- body
		w.WriteHeader(http.StatusAccepted)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	require.NoError(t, nativeFCShutdown(socket))
	select {
	case body := <-received:
		require.Equal(t, "Shutdown", body["action_type"])
		require.Equal(t, float64(nativeFCGuestShutdownTimeout.Milliseconds()), body["timeout_ms"])
		require.Equal(t, true, body["force_on_timeout"])
		require.Greater(t, nativeFCStopGracePeriod, nativeFCGuestShutdownTimeout)
	case <-time.After(time.Second):
		t.Fatal("shutdown request not received")
	}
}

func TestNativeFCNetStatsWithoutNetwork(t *testing.T) {
	// t.TempDir embeds this long test name, which overflows the 104-byte
	// Unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "fcnet")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "fc.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"net_rx_bytes_total":8776,"net_tx_bytes_total":7599,"uptime_seconds":1.5}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	m := &FirecrackerManager{vmSockPath: func(string) string { return socket }}
	v := &VM{ID: "netstats"}
	cleanup, err := m.prepareFCHost(v)
	require.NoError(t, err)
	require.Nil(t, cleanup)
	rx, tx := v.networkStats()
	require.Equal(t, int64(8776), rx)
	require.Equal(t, int64(7599), tx)

	rx, tx = readNativeFCNetStats(filepath.Join(t.TempDir(), "missing.sock"))
	require.Zero(t, rx)
	require.Zero(t, tx)
}

func TestNativeFCX86EmulationExplainsQEMU(t *testing.T) {
	err := (&FirecrackerManager{}).ValidateImagePlatform("amd64", true)
	require.ErrorContains(t, err, "jerboa config set hypervisor qemu")
	require.ErrorContains(t, err, "--platform linux/arm64")
}

func TestNativeFCSecurityPolicy(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "/kernel", WithFCSecurity([]byte(`{"version":1,"egress":[{"protocol":"tcp","address":"192.0.2.10","port":443}]}`)))
	p, err := m.writeFCConfig("security", Config{Memory: "128M"}, "/root")
	require.NoError(t, err)
	t.Cleanup(func() { cleanupFCConfig(p) })
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Contains(t, string(data), "192.0.2.10")
	WithFCSecurity([]byte("null"))(m)
	_, err = m.writeFCConfig("invalid", Config{Memory: "128M"}, "/root")
	require.ErrorContains(t, err, "JSON object")
}

func TestNativeFCUnsupportedConfigs(t *testing.T) {
	kernel := filepath.Join(t.TempDir(), "kernel.img")
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(header[16:], 2)
	binary.LittleEndian.PutUint16(header[18:], 183)
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[52:], 64)
	require.NoError(t, os.WriteFile(kernel, header, 0600))
	m := NewFirecrackerManager("firecracker", kernel)
	base := Config{Architecture: "arm64", Memory: "128M", CPUs: 1}
	require.NoError(t, m.validateFCPlatform(base))
	for name, change := range map[string]func(*Config){
		"x86":          func(c *Config) { c.EmulateX86 = true },
		"architecture": func(c *Config) { c.Architecture = "amd64" },
		"cpus":         func(c *Config) { c.CPUs = 5 },
		"memory":       func(c *Config) { c.Memory = "32M" },
		"tap":          func(c *Config) { c.TapName = "tap0" },
		"volumes":      func(c *Config) { c.Volumes = make([]VolumeMount, 4) },
		"limits":       func(c *Config) { c.MemoryMax = 128 << 20 },
		"disk":         func(c *Config) { c.DiskBPS = 1024 },
		"health":       func(c *Config) { c.HealthCheck = &HealthCheckConfig{Type: "tcp", Port: 8080} },
	} {
		t.Run(name, func(t *testing.T) { cfg := base; change(&cfg); require.Error(t, m.validateFCPlatform(cfg)) })
	}
}

// Opt-in integration test for the Jerboa fixture described in docs/macos-firecracker.md.
func TestNativeFCGuest(t *testing.T) {
	bin, kernel, disk := os.Getenv("JERBOA_TEST_FC_BIN"), os.Getenv("JERBOA_TEST_FC_KERNEL"), os.Getenv("JERBOA_TEST_FC_DISK")
	if bin == "" || kernel == "" || disk == "" {
		t.Skip("set JERBOA_TEST_FC_BIN, JERBOA_TEST_FC_KERNEL and JERBOA_TEST_FC_DISK")
	}
	base, err := os.ReadFile(disk)
	require.NoError(t, err)
	hash := sha256.Sum256(base)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	require.NoError(t, l.Close())
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	udpPort := uint16(udp.LocalAddr().(*net.UDPAddr).Port)
	require.NoError(t, udp.Close())
	m := NewFirecrackerManager(bin, kernel)
	cfg := Config{ImagePath: disk, Architecture: "arm64", Memory: "256M", CPUs: 4, Env: []string{"MARKER=hvf-smoke"},
		PortMaps:    []PortMap{{HostPort: port, GuestPort: 8080, BindAddr: "127.0.0.1"}, {HostPort: udpPort, GuestPort: 8081, BindAddr: "127.0.0.1", Protocol: ProtocolUDP}},
		HealthCheck: &HealthCheckConfig{Type: "http", Port: 8080, Interval: 100 * time.Millisecond, Timeout: time.Second}}
	p, err := m.writeFCConfig("check", cfg, disk)
	require.NoError(t, err)
	out, err := exec.Command(bin, "--no-api", "--config-file", p, "--check-config").CombinedOutput()
	cleanupFCConfig(p)
	require.NoError(t, err, string(out))
	v, err := m.Create(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("guest logs: %s", v.logBuf.Bytes())
		}
		_ = m.Kill(context.Background(), v.ID)
		select {
		case <-v.Done():
		case <-time.After(5 * time.Second):
			t.Error("VMM failed to stop")
		}
	})
	require.NoError(t, m.Start(context.Background(), v.ID))
	client := &http.Client{Timeout: time.Second}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	require.Eventually(t, func() bool {
		r, err := client.Get(url)
		if err != nil {
			return false
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		var result struct {
			Marker string
			Arch   string
			CPUs   int
		}
		return json.Unmarshal(b, &result) == nil && result.Marker == "hvf-smoke" && result.Arch == "arm64" && result.CPUs == 4
	}, 40*time.Second, 100*time.Millisecond)
	c, err := net.Dial("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(udpPort))))
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.SetDeadline(time.Now().Add(3*time.Second)))
	_, err = c.Write([]byte("jerboa-hvf"))
	require.NoError(t, err)
	b := make([]byte, 32)
	n, err := c.Read(b)
	require.NoError(t, err)
	require.Equal(t, "jerboa-hvf", string(b[:n]))
	require.Eventually(t, func() bool { return v.GetHealthStatus() == HealthHealthy }, 5*time.Second, 100*time.Millisecond)
	r, err := client.Get(url + "/shutdown")
	require.NoError(t, err)
	r.Body.Close()
	select {
	case <-v.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("guest did not exit: %s", v.logBuf.Bytes())
	}
	base, err = os.ReadFile(disk)
	require.NoError(t, err)
	require.Equal(t, hash, sha256.Sum256(base), "base disk changed")
	_, err = os.Stat(m.rootfsPath(v.ID))
	require.True(t, os.IsNotExist(err))
}

func TestNativeFCSharedConfig(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "/kernel")
	cfg := Config{Memory: "128M", NetworkName: "app", IPAddress: "172.25.0.2", GatewayIP: "172.25.0.1", SubnetMask: "24", nativeSocket: "/private/net.sock", nativeMAC: "02:00:00:00:00:02"}
	p, err := m.writeFCConfig("shared", cfg, "/root")
	require.NoError(t, err)
	defer cleanupFCConfig(p)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	var c nativeFCConfig
	require.NoError(t, json.Unmarshal(data, &c))
	require.Equal(t, "unix-stream", c.Networks[0].Backend)
	require.Equal(t, cfg.nativeSocket, c.Networks[0].SocketPath)
	require.JSONEq(t, `{"version":1,"unix_stream":"/private/net.sock"}`, string(mustJSON(t, c.Security)))
	b, err := os.ReadFile(c.Firmware["opt/uni/network"])
	require.NoError(t, err)
	require.Equal(t, "172.25.0.2/24,172.25.0.1", string(b))
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestNativeFCSharedPolicyRejectsAmbiguousAuthority(t *testing.T) {
	for _, policy := range []string{`{"version":2}`, `{"version":1,"mode":"development"}`, `{"version":1,"egress":[{"protocol":"icmp","address":"127.0.0.1","port":1}]}`, `{"version":1,"listeners":[{"protocol":"tcp","address":"127.0.0.1","port":80}]}`, `{"version":1,"unknown":true}`} {
		m := NewFirecrackerManager("fc", "/kernel", WithFCSecurity([]byte(policy)))
		_, err := m.sharedPolicy()
		require.Error(t, err)
	}
}

func TestNativeFCSlirpBindContract(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "127.0.0.1"} {
		for _, proto := range []PortProtocol{"", ProtocolTCP, ProtocolUDP} {
			t.Run(bind+"/"+string(proto), func(t *testing.T) {
				bin, kernel, disk := os.Getenv("JERBOA_TEST_FC_BIN"), os.Getenv("JERBOA_TEST_FC_KERNEL"), os.Getenv("JERBOA_TEST_FC_DISK")
				if kernel == "" {
					kernel = "/kernel"
				}
				if disk == "" {
					disk = "/root"
				}
				m := NewFirecrackerManager(bin, kernel, WithFCSecurity([]byte(`{"version":1,"egress":[{"protocol":"tcp","address":"192.0.2.10","port":443}]}`)))
				p, err := m.writeFCConfig("bind", Config{Memory: "128M", PortMaps: []PortMap{{HostPort: 18993, GuestPort: 8080, BindAddr: bind, Protocol: proto}}}, disk)
				require.NoError(t, err)
				defer cleanupFCConfig(p)
				data, err := os.ReadFile(p)
				require.NoError(t, err)
				var c nativeFCConfig
				require.NoError(t, json.Unmarshal(data, &c))
				want := bind
				if want == "" {
					want = "0.0.0.0"
				}
				protocol := string(proto)
				if protocol == "" {
					protocol = "tcp"
				}
				require.Equal(t, []nativeFCForward{{protocol, want, 18993, "10.0.2.15", 8080}}, c.Networks[0].Forwards)
				var listeners []nativeFCListener
				require.NoError(t, json.Unmarshal(c.Security["listeners"], &listeners))
				require.Equal(t, []nativeFCListener{{protocol, want, 18993}}, listeners)
				require.JSONEq(t, `[{"protocol":"tcp","address":"192.0.2.10","port":443}]`, string(c.Security["egress"]))
				require.Empty(t, c.Security["dns"])
				if bin != "" && os.Getenv("JERBOA_TEST_FC_KERNEL") != "" && os.Getenv("JERBOA_TEST_FC_DISK") != "" {
					require.NoError(t, m.checkFCConfig(context.Background(), p))
				}
			})
		}
	}
}
