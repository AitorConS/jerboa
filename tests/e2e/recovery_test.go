//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireNoOrphans fails when a VM the daemon does not report as running
// still has a VMM process: a guest nobody can stop, that keeps its volume
// open and its address taken.
func requireNoOrphans(e *env, ids ...string) {
	e.t.Helper()
	for _, id := range ids {
		if e.vmState(id) == "running" {
			continue
		}
		assert.Emptyf(e.t, vmmPIDs(id), "VM %s is %q for the daemon but its VMM process is still running", id, e.vmState(id))
	}
}

// TestDaemonKilledWhileVMsRun kills the daemon with SIGKILL under two
// running guests, one with a volume, and starts a new one. Afterwards the
// daemon and the host must agree: a guest it reports as running still
// serves on its published port with its data, and can be stopped; a guest
// it reports as stopped has no VMM process left.
func TestDaemonKilledWhileVMsRun(t *testing.T) {
	eachHypervisor(t, func(t *testing.T, hv string) {
		e := newEnv(t, withHypervisor(hv))
		e.buildProbe()
		netName, _ := e.network()
		e.ok("volume", "create", "data", "--size", "64M")
		db := e.startGuest(guestOpts{name: "db", network: netName, volume: "data", env: []string{"PROBE_MARKER=db"}})
		web := e.startGuest(guestOpts{name: "web", network: netName, env: []string{"PROBE_MARKER=web"}})
		db.write("note", "written-before-the-crash")

		e.killDaemon()
		res := e.run("ps")
		require.NotZero(t, res.code, "the CLI must fail without a daemon")
		require.Contains(t, res.stderr, "connect", "the CLI must say it could not reach the daemon")
		e.startDaemon()

		requireNoOrphans(e, db.id, web.id)
		for _, g := range []*guest{db, web} {
			state := e.vmState(g.id)
			require.Containsf(t, []string{"running", "stopped"}, state, "VM %s after the daemon restart", g.name)
			if state == "stopped" {
				continue
			}
			// Still the same boot: the guest was adopted, not restarted.
			require.Equal(t, g.name, g.status().Marker)
		}
		if e.vmState(db.id) == "running" {
			require.Equal(t, 1, db.status().Boots, "the guest must not have been rebooted")
			require.Equal(t, "written-before-the-crash", db.read("note"))
			// The new daemon still resolves names for guests it adopted.
			if e.vmState(web.id) == "running" {
				require.Equal(t, []string{e.inspect(db.id).IPAddress}, web.resolve("db").Addrs)
			}
		}

		// Whatever state they are in, the new daemon can stop and remove
		// them, and no VMM process outlives that.
		for _, g := range []*guest{db, web} {
			if e.vmState(g.id) == "running" {
				e.ok("stop", g.id)
				e.waitState(g.id, "stopped", 60*time.Second)
			}
			e.waitNoVMM(g.id, 15*time.Second)
			e.ok("rm", g.id)
		}

		// The volume survived both the crash and the guest's stop.
		again := e.startGuest(guestOpts{name: "db2", network: netName, volume: "data"})
		require.Equal(t, "written-before-the-crash", again.read("note"))
		require.Equal(t, 2, again.status().Boots)
		e.stopAndRemove(again.id)
		e.requireHealthy()
	})
}

// TestDaemonRestartKeepsStoppedVMsAndData restarts the daemon cleanly and
// checks that what the user created is all still there and usable: the
// image, the volume with its data, the network and a stopped VM.
func TestDaemonRestartKeepsStoppedVMsAndData(t *testing.T) {
	e := newEnv(t)
	digest := e.buildProbe()
	netName, subnet := e.network()
	e.ok("volume", "create", "keep", "--size", "64M")
	g := e.startGuest(guestOpts{name: "first", network: netName, volume: "keep", env: []string{"PROBE_MARKER=first"}})
	g.write("note", "kept across the restart")
	e.ok("stop", g.id)
	e.waitState(g.id, "stopped", 60*time.Second)

	e.restartDaemon()

	require.Equal(t, digest, e.imageDigest("probe"))
	require.Equal(t, "stopped", e.vmState("first"))
	// (Its console does not: the daemon keeps consoles in memory.)
	var nets []struct {
		Name   string `json:"name"`
		Subnet string `json:"subnet"`
	}
	e.jsonOut(&nets, "network", "ls")
	require.Len(t, nets, 1)
	require.Equal(t, netName, nets[0].Name)
	require.Equal(t, subnet, nets[0].Subnet)

	e.ok("rm", "first")
	again := e.startGuest(guestOpts{name: "second", network: netName, volume: "keep"})
	require.Equal(t, "kept across the restart", again.read("note"))
	e.stopAndRemove(again.id)
	e.requireHealthy()
}

var ackRE = regexp.MustCompile(`PROBE_ACK (\d+)`)

// lastAck returns the highest journal record the guest acknowledged on its
// console, or -1.
func lastAck(console string) int {
	last := -1
	for _, m := range ackRE.FindAllStringSubmatch(console, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > last {
			last = n
		}
	}
	return last
}

// TestVMMKilledMidWrite is the power-cut test: a guest appends records to a
// file on its volume, syncing each before acknowledging it on the console,
// and the VMM is killed with SIGKILL while it does. Every record the guest
// acknowledged must be on the volume at the next boot, the file must hold
// nothing but whole records, and the daemon must report the VM as stopped,
// not as running, and let it be removed.
func TestVMMKilledMidWrite(t *testing.T) {
	eachHypervisor(t, func(t *testing.T, hv string) {
		e := newEnv(t, withHypervisor(hv))
		e.buildProbe()
		netName, _ := e.network()
		e.ok("volume", "create", "journal", "--size", "64M")

		rounds := 3
		if os.Getenv("JERBOA_E2E_SLOW") != "" {
			rounds = 12
		}
		acked := -1
		for round := 0; round < rounds; round++ {
			name := fmt.Sprintf("writer%d", round)
			g := e.startGuest(guestOpts{name: name, network: netName, volume: "journal", env: []string{"PROBE_JOURNAL=1"}})
			st := g.status()
			require.GreaterOrEqualf(t, st.Journal, acked+1,
				"round %d: the guest acknowledged record %d before the kill, but only %d records survived", round, acked, st.Journal)
			require.Zerof(t, st.Torn, "round %d: the journal ends in %d bytes that are not a whole record", round, st.Torn)

			// Let it write past where it had got to, then cut the power at a
			// moment of no particular significance.
			target := st.Journal + 50
			require.Eventuallyf(t, func() bool { return lastAck(e.logs(g.id)) >= target }, 60*time.Second, 50*time.Millisecond,
				"round %d: the guest did not acknowledge record %d", round, target)
			time.Sleep(time.Duration(round*37%200) * time.Millisecond)
			pid := e.vmmPID(g.id)
			require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
			// Read the console only after the kill: an acknowledgment the
			// guest printed is one it was entitled to make.
			e.waitState(g.id, "stopped", 30*time.Second)
			acked = lastAck(e.logs(g.id))
			require.GreaterOrEqual(t, acked, target)
			e.ok("rm", g.id)
		}

		g := e.startGuest(guestOpts{name: "reader", network: netName, volume: "journal"})
		st := g.status()
		require.GreaterOrEqualf(t, st.Journal, acked+1, "the guest acknowledged record %d before the last kill, but only %d records survived", acked, st.Journal)
		require.Zero(t, st.Torn)
		require.Equal(t, rounds+1, st.Boots, "the boot counter on the volume must have survived every kill")
		e.stopAndRemove(g.id)
		e.requireHealthy()
	})
}

// TestGuestExitStatusAndAutoRemove checks what a user sees of a guest that
// ends by itself: its state, its exit status and its console, and that --rm
// removes it together with its boot disk.
func TestGuestExitStatusAndAutoRemove(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()

	id := strings.TrimSpace(e.ok("run", "probe", "--name", "quitter", "-e", "PROBE_EXIT=0"))
	e.vmIDs[id] = true
	e.waitState(id, "stopped", 60*time.Second)
	require.Contains(t, e.logs(id), "PROBE_EXIT 0")
	e.waitNoVMM(id, 10*time.Second)
	e.ok("rm", id)

	id = strings.TrimSpace(e.ok("run", "probe", "--rm", "-e", "PROBE_EXIT=0"))
	e.vmIDs[id] = true
	e.waitGone(id, 60*time.Second)
	e.waitNoVMM(id, 10*time.Second)
	disks, err := filepath.Glob(filepath.Join(e.home, ".jerboa", "vm-disks", "*"+id+"*"))
	require.NoError(t, err)
	require.Empty(t, disks, "--rm must remove the VM's private boot disk")
}
