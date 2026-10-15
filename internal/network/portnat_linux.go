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
// rate of a Redis guest.
//
// By default clients on other hosts stay on the userspace proxy: a translated
// connection from outside is forwarded, and Docker and ufw drop forwarded
// traffic by default (filter FORWARD policy), which a rule in this table
// cannot override. With --port-publish nat-lan the prerouting hook translates
// them too, and two rules in the iptables filter table accept the forwarded
// connections, as Docker does for its own ports (+37 % Redis requests from a
// LAN client). Those connections then skip the host's INPUT rules.
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

// forwardComment marks the FORWARD rules jerboad owns.
const forwardComment = "jerboa published ports"

// forwardRules accept connections translated to a guest, in both directions.
// The DNAT conntrack state limits them to published ports.
func forwardRules() [][]string {
	var rules [][]string
	for _, dir := range []string{"-o", "-i"} {
		rules = append(rules, []string{dir, "jerboa-br+", "-m", "conntrack", "--ctstate", "DNAT",
			"-m", "comment", "--comment", forwardComment, "-j", "ACCEPT"})
	}
	return rules
}

// forwardChain is DOCKER-USER when Docker is set up (it is evaluated first
// and Docker keeps it across restarts), FORWARD otherwise. If Docker starts
// later, a rule in FORWARD still runs: Docker's chains do not drop traffic to
// other bridges, only its policy does, and that comes after.
func forwardChain() string {
	if runIptables("-S", "DOCKER-USER") == nil {
		return "DOCKER-USER"
	}
	return "FORWARD"
}

func openForward() error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return errors.New("iptables not found")
	}
	chain := forwardChain()
	for _, r := range forwardRules() {
		if err := runIptables(append([]string{"-I", chain, "1"}, r...)...); err != nil {
			closeForward()
			return err
		}
	}
	return nil
}

// closeForward removes every jerboad rule from both chains it may use,
// including duplicates left by daemons that did not exit cleanly.
func closeForward() {
	if _, err := exec.LookPath("iptables"); err != nil {
		return
	}
	for _, chain := range []string{"DOCKER-USER", "FORWARD"} {
		for _, r := range forwardRules() {
			for range 16 {
				if runIptables(append([]string{"-D", chain}, r...)...) != nil {
					break
				}
			}
		}
	}
}

func runIptables(args ...string) error {
	cmd := exec.Command("iptables", append([]string{"-w"}, args...)...) //nolint:noctx // short-lived local tool
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
