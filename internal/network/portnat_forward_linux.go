//go:build linux

package network

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode"
)

// With --port-publish nat-lan, connections from other hosts are translated in
// prerouting and then forwarded to a guest bridge. Firewalls drop forwarded
// traffic by default, so jerboad accepts exactly those connections (DNAT
// conntrack state, to or from jerboa bridges) wherever forwarded packets are
// filtered:
//   - in the iptables filter table (Docker, ufw, iptables-nft or legacy):
//     DOCKER-USER when Docker set it up, FORWARD otherwise;
//   - in every forward base chain of other nftables tables (nftables.service,
//     firewalld): an accept in jerboa's own table could not override a drop
//     there, since every base chain on a hook must accept a packet.
//
// Every rule carries forwardComment, so jerboad finds and removes its own
// rules, including those left by a daemon that did not exit cleanly.
// ensureForward runs again whenever ports are published, which restores the
// rules after a firewall reload (firewalld recreates its table).

// forwardComment marks the rules jerboad owns.
const forwardComment = "jerboa published ports"

// forwardRules are the iptables rule specs, one per direction.
func forwardRules() [][]string {
	var rules [][]string
	for _, dir := range []string{"-o", "-i"} {
		rules = append(rules, []string{dir, "jerboa-br+", "-m", "conntrack", "--ctstate", "DNAT",
			"-m", "comment", "--comment", forwardComment, "-j", "ACCEPT"})
	}
	return rules
}

// nftForwardRules are the same rules in nft syntax.
func nftForwardRules() []string {
	return []string{
		`oifname "jerboa-br*" ct status dnat accept comment "` + forwardComment + `"`,
		`iifname "jerboa-br*" ct status dnat accept comment "` + forwardComment + `"`,
	}
}

// iptablesChain is DOCKER-USER when Docker set it up (evaluated first and
// kept across Docker restarts), FORWARD otherwise. If Docker starts later, a
// rule in FORWARD still runs: Docker's chains do not drop traffic to other
// bridges, only its policy does, and that comes after.
func iptablesChain() string {
	if runIptables("-S", "DOCKER-USER") == nil {
		return "DOCKER-USER"
	}
	return "FORWARD"
}

// openForward clears stale rules and installs fresh ones.
func openForward() error {
	if !hasTool("iptables") && !hasTool("nft") {
		return errors.New("neither iptables nor nft found")
	}
	closeForward()
	if err := ensureForward(); err != nil {
		closeForward()
		return err
	}
	return nil
}

// ensureForward installs the rules that are missing. It is idempotent.
func ensureForward() error {
	if hasTool("iptables") && runIptables("-S", "FORWARD") == nil {
		chain := iptablesChain()
		for _, r := range forwardRules() {
			if runIptables(append([]string{"-C", chain}, r...)...) == nil {
				continue
			}
			if err := runIptables(append([]string{"-I", chain, "1"}, r...)...); err != nil {
				return err
			}
		}
	}
	if !hasTool("nft") {
		return nil
	}
	ruleset, err := nftRuleset()
	if err != nil {
		return err
	}
	owned := map[nftChain]int{}
	for _, r := range ownedNftRules(ruleset) {
		owned[r.chain]++
	}
	for _, c := range foreignForwardChains(ruleset) {
		if owned[c] == len(nftForwardRules()) {
			continue
		}
		var script bytes.Buffer
		for _, r := range nftForwardRules() {
			fmt.Fprintf(&script, "insert rule %s %s %s %s\n", c.Family, c.Table, c.Name, r)
		}
		if err := runNft(script.String()); err != nil {
			return err
		}
	}
	return nil
}

// closeForward removes every rule jerboad owns, in both firewalls.
func closeForward() {
	if hasTool("iptables") {
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
	if !hasTool("nft") {
		return
	}
	ruleset, err := nftRuleset()
	if err != nil {
		return
	}
	var script bytes.Buffer
	for _, r := range ownedNftRules(ruleset) {
		if !nftIdentifier(r.chain.Table) || !nftIdentifier(r.chain.Name) {
			continue
		}
		fmt.Fprintf(&script, "delete rule %s %s %s handle %d\n", r.chain.Family, r.chain.Table, r.chain.Name, r.handle)
	}
	if script.Len() > 0 {
		_ = runNft(script.String())
	}
}

func hasTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// nftChain names a chain of the nftables ruleset.
type nftChain struct{ Family, Table, Name string }

type nftOwnedRule struct {
	chain  nftChain
	handle int
}

// nftObject is one element of `nft -j list ruleset`.
type nftObject struct {
	Chain *struct {
		Family string `json:"family"`
		Table  string `json:"table"`
		Name   string `json:"name"`
		Hook   string `json:"hook"`
	} `json:"chain"`
	Rule *struct {
		Family  string `json:"family"`
		Table   string `json:"table"`
		Chain   string `json:"chain"`
		Handle  int    `json:"handle"`
		Comment string `json:"comment"`
	} `json:"rule"`
}

func nftRuleset() ([]nftObject, error) {
	cmd := exec.Command("nft", "-j", "list", "ruleset") //nolint:noctx // short-lived local tool
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("nft list ruleset: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return parseNftRuleset(out.Bytes())
}

func parseNftRuleset(b []byte) ([]nftObject, error) {
	var doc struct {
		Nftables []nftObject `json:"nftables"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse nft ruleset: %w", err)
	}
	return doc.Nftables, nil
}

// foreignForwardChains lists the IPv4 forward base chains jerboad must open:
// not its own table, and not the iptables filter table, which takes the
// iptables rules (iptables-nft shows it to nft as table ip filter with
// upper-case chains).
func foreignForwardChains(objs []nftObject) []nftChain {
	var out []nftChain
	for _, o := range objs {
		c := o.Chain
		if c == nil || c.Hook != "forward" || (c.Family != "ip" && c.Family != "inet") {
			continue
		}
		if c.Table == natTable || (c.Family == "ip" && c.Table == "filter" && c.Name == "FORWARD") {
			continue
		}
		// nft scripts take these names unquoted; skip any it could misparse.
		if !nftIdentifier(c.Table) || !nftIdentifier(c.Name) {
			continue
		}
		out = append(out, nftChain{c.Family, c.Table, c.Name})
	}
	return out
}

// ownedNftRules lists the rules carrying forwardComment, wherever they are.
func ownedNftRules(objs []nftObject) []nftOwnedRule {
	var out []nftOwnedRule
	for _, o := range objs {
		if r := o.Rule; r != nil && r.Comment == forwardComment {
			out = append(out, nftOwnedRule{nftChain{r.Family, r.Table, r.Chain}, r.Handle})
		}
	}
	return out
}

// nftIdentifier reports whether name can appear unquoted in an nft script
// (nft rejects quoted table and chain names in rule commands).
func nftIdentifier(name string) bool {
	if name == "" || !unicode.IsLetter(rune(name[0])) {
		return false
	}
	for _, r := range name {
		allowed := unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.-/", r)
		if r > unicode.MaxASCII || !allowed {
			return false
		}
	}
	return true
}
