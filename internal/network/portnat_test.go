package network

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeNAT struct {
	mu        sync.Mutex
	bridges   []string
	added     []natEntry
	removed   []natEntry
	failAdd   bool
	shutdowns int
}

func (f *fakeNAT) prepareBridge(b string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bridges = append(f.bridges, b)
	return nil
}
func (f *fakeNAT) add(e []natEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAdd {
		return errors.New("nft: injected failure")
	}
	f.added = append(f.added, e...)
	return nil
}
func (f *fakeNAT) remove(e []natEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, e...)
	return nil
}
func (f *fakeNAT) shutdown() { f.mu.Lock(); f.shutdowns++; f.mu.Unlock() }

func withNAT(t *testing.T, n portNAT) {
	t.Helper()
	natMu.Lock()
	prev := activeNAT
	activeNAT = n
	natMu.Unlock()
	t.Cleanup(func() { natMu.Lock(); activeNAT = prev; natMu.Unlock() })
}

func freePort(t *testing.T, network string) uint16 {
	t.Helper()
	if network == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		defer pc.Close()
		return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func TestNATEntries(t *testing.T) {
	entries, err := natEntries("10.100.0.7", []PortForward{
		{HostPort: 8080, GuestPort: 80, Protocol: "tcp"},
		{HostPort: 6379, GuestPort: 6379, Protocol: "tcp", BindAddr: "127.0.0.1"},
		{HostPort: 5353, GuestPort: 53, Protocol: "UDP", BindAddr: "0.0.0.0"},
		{HostPort: 9000, GuestPort: 9000, Protocol: "tcp", BindAddr: "::1"}, // IPv6: proxy only
	})
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Nil(t, entries[0].bind)
	require.Equal(t, "tcp", entries[0].proto)
	require.Equal(t, net.IPv4(127, 0, 0, 1).To4(), entries[1].bind)
	require.Equal(t, "udp", entries[2].proto)
	require.Nil(t, entries[2].bind)
	require.Equal(t, net.IPv4(10, 100, 0, 7).To4(), entries[2].guest)

	_, err = natEntries("fd00::1", nil)
	require.Error(t, err)
}

func TestInitPortPublishingModes(t *testing.T) {
	t.Cleanup(ShutdownPortPublishing)
	_, err := InitPortPublishing("iptables")
	require.ErrorContains(t, err, "invalid")
	mode, err := InitPortPublishing(PublishProxy)
	require.NoError(t, err)
	require.Equal(t, PublishProxy, mode)
	require.Nil(t, currentNAT())
}

func TestPublishPortsInstallsAndRemovesNAT(t *testing.T) {
	n := &fakeNAT{}
	withNAT(t, n)
	tcp, udp := freePort(t, "tcp"), freePort(t, "udp")
	f, err := PublishPorts("10.100.0.9", "jerboa-br0", []PortForward{
		{HostPort: tcp, GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"},
		{HostPort: udp, GuestPort: 53, Protocol: "udp", BindAddr: "127.0.0.1"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"jerboa-br0"}, n.bridges)
	require.Len(t, n.added, 2)
	// The UDP port is reserved while published.
	_, err = net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", itoa(udp)))
	require.Error(t, err)

	f.Close()
	require.Len(t, n.removed, 2)
	pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", itoa(udp)))
	require.NoError(t, err)
	pc.Close()
}

func TestPublishPortsFallsBackToProxy(t *testing.T) {
	n := &fakeNAT{failAdd: true}
	withNAT(t, n)
	f, err := PublishPorts("10.100.0.9", "jerboa-br0", []PortForward{{HostPort: freePort(t, "tcp"), GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"}})
	require.NoError(t, err)
	f.Close()
	require.Empty(t, n.removed)

	// Without a bridge (StartForwarder) kernel NAT is never used.
	n2 := &fakeNAT{}
	withNAT(t, n2)
	f, err = StartForwarder("10.100.0.9", []PortForward{{HostPort: freePort(t, "tcp"), GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"}})
	require.NoError(t, err)
	f.Close()
	require.Empty(t, n2.added)
}

func itoa(p uint16) string { return strconv.Itoa(int(p)) }
