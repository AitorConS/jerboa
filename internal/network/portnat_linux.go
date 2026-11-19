//go:build linux

package network

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Kernel NAT for published ports, in an nftables table owned by jerboad and
// independent of other firewall rules (Docker's included). Each published
// port is one element of a map consulted by static DNAT rules, so ports are
// added and removed atomically by key, with no rule handles to track.
//
// Only connections made on this host are translated (output hook), 127.0.0.1
// included: the bridge then has route_localnet set, and postrouting
// masquerades loopback sources so the guest's replies come back through the
// bridge. This removes the userspace hop, which cost ~40 % of the request
// rate of a Redis guest.
//
// By default clients on other hosts stay on the userspace proxy: a translated
// connection from outside is forwarded, and Docker and ufw drop forwarded
// traffic by default (filter FORWARD policy), which a rule in this table
// cannot override. With --port-publish nat-lan the prerouting hook translates
// them too, and two rules in the iptables filter table accept the forwarded
// connections, as Docker does for its own ports (+37 % Redis requests from a
// LAN client). Those connections then skip the host's INPUT rules. Ports
// published on a loopback address stay local: a packet from another host
// addressed to 127.0.0.0/8 is never translated.
const natTable = "jerboa"

// natRuleset returns the table, with the prerouting chain for clients on
// other hosts when lan is set. Connections coming from guests (jerboa
// bridges) keep using the proxy: translating them back to a guest on the same
// bridge would need hairpin masquerading.
func natRuleset(lan bool) string {
	var prerouting string
	if lan {
		prerouting = `
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "jerboa-br*" return
		ip daddr 127.0.0.0/8 return
		fib daddr type local dnat ip to meta l4proto . th dport map @pub_any
		dnat ip to ip daddr . meta l4proto . th dport map @pub_addr
	}`
	}
	return `table ip jerboa {
	map pub_any {
		type inet_proto . inet_service : ipv4_addr . inet_service
	}
	map pub_addr {
		type ipv4_addr . inet_proto . inet_service : ipv4_addr . inet_service
	}
	chain output {
		type nat hook output priority -100; policy accept;
		fib daddr type local dnat ip to meta l4proto . th dport map @pub_any
		dnat ip to ip daddr . meta l4proto . th dport map @pub_addr
	}` + prerouting + `
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "jerboa-br*" ip saddr 127.0.0.0/8 masquerade
	}
}
`
}

type nftNAT struct{ lan bool }

func newKernelNAT(lan bool) (portNAT, error) {
	if isWSL() {
		// WSL2 mirrors host ports by watching listening sockets; translated
		// connections would bypass that path.
		return nil, errors.New("WSL2")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return nil, errors.New("nft not found")
	}
	// Replacing the table is the capability probe and clears translations
	// left by a previous daemon in one transaction.
	if err := runNft("table ip " + natTable + "\ndelete table ip " + natTable + "\n" + natRuleset(lan)); err != nil {
		return nil, err
	}
	// Rules a previous daemon left in FORWARD go in every mode.
	closeForward()
	if lan {
		if err := openForward(); err != nil {
			_ = runNft("delete table ip " + natTable + "\n")
			return nil, fmt.Errorf("accept translated connections in FORWARD: %w", err)
		}
	}
	return nftNAT{lan: lan}, nil
}

func runIptables(args ...string) error {
	cmd := hostCommand("iptables", append([]string{"-w"}, args...)...) //nolint:noctx,gosec // short-lived local tool; args are fixed rule specs
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

func isWSL() bool {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil &&
		strings.Contains(strings.ToLower(string(b)), "microsoft") {
		return true
	}
	_, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop")
	return err == nil
}

func runNft(script string) error {
	cmd := hostCommand("nft", "-f", "-") //nolint:noctx // short-lived local tool
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// nftOutput runs an nft script and returns what it printed.
func nftOutput(script string) (string, error) {
	cmd := hostCommand("nft", "-f", "-") //nolint:noctx // short-lived local tool
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// route_localnet lets the kernel route packets from 127.0.0.0/8 to a bridge,
// and so also deliver packets a guest sends to 127.0.0.0/8 to host sockets:
// only the loopback drop of the guard stops those. It is set only while the
// guard is loaded, and WatchGuard turns it off while the guard is gone.
var (
	// Test seams: the guard check and the sysctl write.
	loopbackGuard      = EnsureGuard
	writeRouteLocalnet = func(bridge, value string) error {
		p := "/proc/sys/net/ipv4/conf/" + bridge + "/route_localnet"
		return os.WriteFile(p, []byte(value), 0o644) //nolint:gosec // sysctl file
	}
	// localnet holds the bridges kernel NAT set route_localnet on: true
	// while it is on, false while WatchGuard keeps it off.
	localnet = struct {
		mu      sync.Mutex
		bridges map[string]bool
	}{bridges: map[string]bool{}}
)

func (nftNAT) prepareBridge(bridge string) error {
	if strings.ContainsAny(bridge, "/.") || bridge == "" {
		return fmt.Errorf("invalid bridge name %q", bridge)
	}
	localnet.mu.Lock()
	defer localnet.mu.Unlock()
	// Fail closed: without the guard a guest would reach the host's
	// loopback, so the ports stay on the userspace proxy.
	if err := loopbackGuard(); err != nil {
		_ = writeRouteLocalnet(bridge, "0")
		if _, ok := localnet.bridges[bridge]; ok {
			localnet.bridges[bridge] = false
		}
		return fmt.Errorf("route_localnet on %s needs the loopback guard: %w", bridge, err)
	}
	if err := writeRouteLocalnet(bridge, "1"); err != nil {
		return fmt.Errorf("route_localnet on %s: %w", bridge, err)
	}
	localnet.bridges[bridge] = true
	return nil
}

// disableRouteLocalnet turns route_localnet off on every bridge kernel NAT
// turned it on for, and reports whether it was on for one. A bridge that no
// longer exists is forgotten.
func disableRouteLocalnet() bool {
	localnet.mu.Lock()
	defer localnet.mu.Unlock()
	changed := false
	for b, on := range localnet.bridges {
		err := writeRouteLocalnet(b, "0")
		switch {
		case errors.Is(err, os.ErrNotExist):
			delete(localnet.bridges, b)
		case err != nil:
			slog.Error("guard: route_localnet could not be turned off", "bridge", b, "err", err)
		default:
			changed = changed || on
			localnet.bridges[b] = false
		}
	}
	return changed
}

// restoreRouteLocalnet turns route_localnet back on where disableRouteLocalnet
// turned it off. The guard must be loaded.
func restoreRouteLocalnet() {
	localnet.mu.Lock()
	defer localnet.mu.Unlock()
	for b, on := range localnet.bridges {
		if on {
			continue
		}
		err := writeRouteLocalnet(b, "1")
		switch {
		case errors.Is(err, os.ErrNotExist):
			delete(localnet.bridges, b)
		case err != nil:
			slog.Warn("guard: route_localnet could not be restored", "bridge", b, "err", err)
		default:
			localnet.bridges[b] = true
			slog.Info("guard: route_localnet restored", "bridge", b)
		}
	}
}

func (n nftNAT) add(entries []natEntry) error {
	if err := runNft(natScript("add", entries)); err != nil {
		return err
	}
	// A firewall reload (firewalld recreates its table) drops the accept
	// rules; publishing restores them. Local clients work without them.
	if n.lan {
		if err := ensureForward(); err != nil {
			slog.Warn("port forward: accept LAN connections in the firewall", "err", err)
		}
	}
	return nil
}

func (nftNAT) remove(entries []natEntry) error { return runNft(natScript("delete", entries)) }
func (n nftNAT) shutdown() {
	_ = runNft("table ip " + natTable + "\ndelete table ip " + natTable + "\n")
	if n.lan {
		closeForward()
	}
}

// clearKernelNAT removes translations and FORWARD rules a previous daemon left
// when this one only uses the proxy. Best effort: without nft or iptables
// there is nothing of ours to remove.
func clearKernelNAT() {
	if _, err := exec.LookPath("nft"); err == nil {
		_ = runNft("table ip " + natTable + "\ndelete table ip " + natTable + "\n")
	}
	closeForward()
}

// natScript renders one transaction adding or deleting map elements.
func natScript(op string, entries []natEntry) string {
	var b strings.Builder
	for _, e := range entries {
		if e.bind == nil {
			fmt.Fprintf(&b, "%s element ip %s pub_any { %s . %d", op, natTable, e.proto, e.hostPort)
		} else {
			fmt.Fprintf(&b, "%s element ip %s pub_addr { %s . %s . %d", op, natTable, e.bind, e.proto, e.hostPort)
		}
		if op == "add" {
			fmt.Fprintf(&b, " : %s . %d", e.guest, e.guestPort)
		}
		b.WriteString(" }\n")
	}
	return b.String()
}
