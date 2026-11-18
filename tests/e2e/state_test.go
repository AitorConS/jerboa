//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// damage replaces a state file with what a crash can leave of it: nothing
// ("empty": the file was created and never written) or its first half
// ("truncated").
func damage(t *testing.T, path, mode string) {
	t.Helper()
	old, err := os.ReadFile(path)
	require.NoErrorf(t, err, "the state file to damage")
	require.NotEmpty(t, old)
	require.NoError(t, os.Chmod(path, 0o600))
	switch mode {
	case "empty":
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	case "truncated":
		require.NoError(t, os.WriteFile(path, old[:len(old)/2], 0o600))
	default:
		t.Fatalf("unknown damage %q", mode)
	}
}

// TestDaemonSurvivesDamagedStateFiles damages one of the daemon's state
// files while it is down, as a power cut or a full disk would. Whatever the
// file, the daemon must start again, everything the file did not describe
// must keep working, the damage must not go unreported when it cost the
// user something, and the user must be able to get back to a clean state
// with the CLI.
func TestDaemonSurvivesDamagedStateFiles(t *testing.T) {
	modes := []string{"empty"}
	if os.Getenv("JERBOA_E2E_SLOW") != "" {
		modes = append(modes, "truncated")
	}

	// setup leaves, with the daemon stopped: image probe, network netName
	// with the stopped VM "a" (volume "data" holding a note) and a second
	// network other with the stopped VM "b".
	type fixture struct {
		e                *env
		digest           string
		netName, other   string
		aID, bID         string
		dataDir, volNote string
	}
	setup := func(t *testing.T) *fixture {
		e := newEnv(t)
		f := &fixture{e: e, digest: e.buildProbe(), dataDir: filepath.Join(e.home, ".jerboa"), volNote: "kept through the damage"}
		f.netName, _ = e.network()
		f.other, _ = e.network()
		e.ok("volume", "create", "data", "--size", "32M")
		a := e.startGuest(guestOpts{name: "a", network: f.netName, volume: "data"})
		a.write("note", f.volNote)
		b := e.startGuest(guestOpts{name: "b", network: f.other})
		for _, g := range []*guest{a, b} {
			e.ok("stop", g.id)
			e.waitState(g.id, "stopped", 60*time.Second)
		}
		f.aID, f.bID = a.id, b.id
		e.stopDaemon()
		return f
	}
	// restart starts the daemon on the damaged state: it must come up.
	restart := func(t *testing.T, f *fixture) {
		require.True(t, f.e.tryStartDaemon(60*time.Second), "the daemon did not start on a damaged state file; log:\n%s", f.e.daemonLog())
	}
	// untouched checks what no damaged file described.
	untouched := func(t *testing.T, f *fixture) {
		e := f.e
		require.Equal(t, "stopped", e.vmState(f.bID), "a VM whose record is intact")
		require.Contains(t, e.networks(), f.other, "a network whose record is intact")
		g := e.startGuest(guestOpts{name: "fresh", network: f.other, env: []string{"PROBE_MARKER=fresh"}})
		require.Equal(t, "fresh", g.status().Marker)
		e.stopAndRemove(g.id)
	}

	for _, mode := range modes {
		t.Run(mode+"/vm record", func(t *testing.T) {
			f := setup(t)
			e := f.e
			record := filepath.Join(f.dataDir, "vms", f.aID, "state.json")
			damage(t, record, mode)
			restart(t, f)
			untouched(t, f)

			// The VM is lost, and the doctor says a record is unreadable.
			require.Empty(t, e.vmState(f.aID))
			rep, _ := e.doctor()
			probs := rep.problems()
			require.Len(t, probs, 1)
			require.Equal(t, "vms", probs[0].Check)
			require.Contains(t, probs[0].Message, "1 VM record could not be read back")
			// Its volume is not lost with it.
			again := e.startGuest(guestOpts{name: "a2", network: f.other, volume: "data"})
			require.Equal(t, f.volNote, again.read("note"))
			e.stopAndRemove(again.id)

			// prune leaves a record alone while it may belong to a VM being
			// created (one hour); after that it removes it, as the doctor
			// says.
			require.Empty(t, e.prune("-y").kinds()["leftover"])
			require.FileExists(t, record)
			old := time.Now().Add(-2 * time.Hour)
			require.NoError(t, os.Chtimes(record, old, old))
			require.NoError(t, os.Chtimes(filepath.Dir(record), old, old))
			require.Equal(t, []string{"vms/" + f.aID}, e.prune("-y").kinds()["leftover"])
			require.NoDirExists(t, filepath.Dir(record))
			e.requireHealthy()
		})

		t.Run(mode+"/image index", func(t *testing.T) {
			f := setup(t)
			e := f.e
			damage(t, filepath.Join(f.dataDir, "images", "refs.json"), mode)
			restart(t, f)
			// The index is rebuilt from the images themselves: nothing lost.
			require.Equal(t, f.digest, e.imageDigest("probe"))
			require.Equal(t, "stopped", e.vmState(f.aID))
			untouched(t, f)
			e.requireHealthy()
		})

		t.Run(mode+"/image manifest", func(t *testing.T) {
			f := setup(t)
			e := f.e
			damage(t, filepath.Join(f.dataDir, "images", strings.TrimPrefix(f.digest, "sha256:"), "manifest.json"), mode)
			restart(t, f)
			// The image's disk is intact, so either the image still runs or
			// the doctor reports it: it must not be listed as fine and fail.
			res := e.run("run", "probe", "--name", "probe-after", "--network", f.other, "-p", "127.0.0.1:"+itoa(freePort(t))+":8080")
			rep, _ := e.doctor()
			if res.code == 0 {
				id := strings.TrimSpace(res.stdout)
				e.vmIDs[id] = true
				e.waitLog(id, "PROBE_READY", 60*time.Second)
				e.stopAndRemove(id)
			} else {
				require.NotEmptyf(t, rep.problems(), "the image cannot run (%s) and the doctor reports nothing", strings.TrimSpace(res.stderr))
			}
			require.Equal(t, "stopped", e.vmState(f.bID))
		})

		t.Run(mode+"/network record", func(t *testing.T) {
			f := setup(t)
			e := f.e
			damage(t, filepath.Join(f.dataDir, "networks", f.netName, "meta.json"), mode)
			restart(t, f)
			untouched(t, f)

			// The network is gone for the daemon, its VM is still listed,
			// and the doctor says which VM lost its network.
			require.NotContains(t, e.networks(), f.netName)
			require.Equal(t, "stopped", e.vmState(f.aID))
			rep, _ := e.doctor()
			probs := rep.problems()
			require.Len(t, probs, 1)
			require.Equal(t, "networks", probs[0].Check)
			require.Contains(t, probs[0].Message, f.netName)
			require.Contains(t, probs[0].Message, "a")

			// The way out: remove the network's remains, then the VM.
			e.ok("network", "rm", f.netName)
			e.ok("rm", f.aID)
			e.requireHealthy()
			// And the name is free for a new network.
			e.ok("network", "create", f.netName, "--subnet", composeSubnet())
			require.Contains(t, e.networks(), f.netName)
		})

		t.Run(mode+"/volume record", func(t *testing.T) {
			f := setup(t)
			e := f.e
			damage(t, filepath.Join(f.dataDir, "volumes", "data", "meta.json"), mode)
			restart(t, f)
			untouched(t, f)
			// The record describes the disk; the data is on the disk.
			require.Contains(t, e.volumes(), "data")
			e.ok("rm", f.aID)
			again := e.startGuest(guestOpts{name: "a2", network: f.other, volume: "data"})
			require.Equal(t, f.volNote, again.read("note"), "the volume's data must survive the loss of its record")
			e.stopAndRemove(again.id)
			e.requireHealthy()
		})
	}
}
