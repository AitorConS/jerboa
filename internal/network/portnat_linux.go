//go:build linux

package network

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
// rate of a Redis guest. Clients on other hosts stay on the userspace proxy:
// a translated connection from outside is forwarded, and Docker and ufw drop
// forwarded traffic by default (filter FORWARD policy), which a rule in this
// table cannot override.
const natTable = "jerboa"

const natRuleset = `table ip jerboa {
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
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "jerboa-br*" ip saddr 127.0.0.0/8 masquerade
	}
}
`

type nftNAT struct{}

func newKernelNAT() (portNAT, error) {
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
	if err := runNft("table ip " + natTable + "\ndelete table ip " + natTable + "\n" + natRuleset); err != nil {
		return nil, err
	}
	return nftNAT{}, nil
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
	cmd := exec.Command("nft", "-f", "-") //nolint:noctx // short-lived local tool
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func (nftNAT) prepareBridge(bridge string) error {
	if strings.ContainsAny(bridge, "/.") || bridge == "" {
		return fmt.Errorf("invalid bridge name %q", bridge)
	}
	p := "/proc/sys/net/ipv4/conf/" + bridge + "/route_localnet"
	if err := os.WriteFile(p, []byte("1"), 0o644); err != nil { //nolint:gosec // sysctl file
		return fmt.Errorf("route_localnet on %s: %w", bridge, err)
	}
	return nil
}

func (nftNAT) add(entries []natEntry) error    { return runNft(natScript("add", entries)) }
func (nftNAT) remove(entries []natEntry) error { return runNft(natScript("delete", entries)) }
func (nftNAT) shutdown()                       { _ = runNft("table ip " + natTable + "\ndelete table ip " + natTable + "\n") }

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
