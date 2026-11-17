//go:build e2e

package e2e

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDaemonKilledMidOperation kills the daemon with SIGKILL at a random
// moment while the CLI creates and removes volumes, networks, images and
// VMs as fast as it can, then starts it again; several times over. A crash
// may lose the operation it interrupted. It must not cost anything else:
// the daemon starts, every listing works, the doctor finds no error in the
// state, no VMM process is left without a VM, and everything that is listed
// can be used and removed.
func TestDaemonKilledMidOperation(t *testing.T) {
	rounds := 4
	if os.Getenv("JERBOA_E2E_SLOW") != "" {
		rounds = 25
	}
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewSource(seed))

	e := newEnv(t)
	e.buildProbe()
	// What must survive every crash untouched.
	keepNet, _ := e.network()
	e.ok("volume", "create", "keep", "--size", "32M")
	g := e.startGuest(guestOpts{name: "keeper", network: keepNet, volume: "keep"})
	g.write("note", "must survive every crash")
	e.stopAndRemove(g.id)

	for round := 0; round < rounds; round++ {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		// Three clients at once, each over its own names.
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					name := fmt.Sprintf("churn-%d-%d-%d", round, w, i)
					switch w {
					case 0:
						e.run("volume", "create", name, "--size", "16M")
						e.run("volume", "rm", name)
					case 1:
						e.run("tag", "probe", name+":latest")
						res := e.run("run", "probe", "--name", name, "--rm", "-e", "PROBE_EXIT=0")
						if res.code == 0 {
							e.vmIDs[strings.TrimSpace(res.stdout)] = true
						}
						e.run("rmi", name+":latest")
					case 2:
						subnet := fmt.Sprintf("10.%d.%d.0/24", 230+round%20, i%250)
						e.run("network", "create", fmt.Sprintf("c%d-%d", round, i%1000), "--subnet", subnet)
						e.run("network", "rm", fmt.Sprintf("c%d-%d", round, i%1000))
					}
				}
			}(w)
		}
		time.Sleep(time.Duration(100+rng.Intn(1500)) * time.Millisecond)
		e.killDaemon()
		close(stop)
		wg.Wait()

		require.Truef(t, e.tryStartDaemon(90*time.Second), "round %d: the daemon did not start after being killed mid-operation; log:\n%s", round, e.daemonLog())

		// Everything can be listed.
		vms := e.ps()
		imgs := e.images()
		vols := e.volumes()
		nets := e.networks()
		e.df()
		rep, _ := e.doctor()
		for _, f := range rep.Findings {
			if stateChecks[f.Check] {
				assert.NotEqualf(t, "error", f.Severity, "round %d: doctor after the crash: %s: %s", round, f.Check, f.Message)
			}
		}
		// What existed before the churn is intact.
		require.Containsf(t, vols, "keep", "round %d", round)
		require.Containsf(t, nets, keepNet, "round %d", round)
		require.Truef(t, e.hasImage("probe"), "round %d: the image is gone", round)

		// Everything listed can be removed; no VMM outlives its VM.
		for _, v := range vms {
			e.vmIDs[v.ID] = true
			// A VM the crash caught between being recorded and being
			// started is "created": it has nothing to stop.
			if v.State != "stopped" && v.State != "created" {
				res := e.run("stop", "--force", v.ID)
				require.Zerof(t, res.code, "round %d: stop %s (%s): %s", round, v.Name, v.State, res.all())
			}
		}
		for _, v := range vms {
			require.Eventuallyf(t, func() bool { st := e.vmState(v.ID); return st == "stopped" || st == "created" || st == "" }, 60*time.Second, 200*time.Millisecond,
				"round %d: VM %s does not stop", round, v.Name)
			if e.vmState(v.ID) != "" {
				res := e.run("rm", v.ID)
				// --rm may remove it first.
				require.Truef(t, res.code == 0 || e.vmState(v.ID) == "", "round %d: rm %s: %s", round, v.Name, res.all())
			}
		}
		for id := range e.vmIDs {
			assert.Emptyf(t, vmmPIDs(id), "round %d: a VMM process of VM %s outlived it", round, id)
		}
		for _, img := range imgs {
			if strings.HasPrefix(img.Name, "churn-") {
				e.ok("rmi", img.Name+":"+img.Tag)
			}
		}
		for _, v := range vols {
			if strings.HasPrefix(v, "churn-") {
				e.ok("volume", "rm", v)
			}
		}
		for _, n := range nets {
			if n != keepNet {
				e.ok("network", "rm", n)
			}
		}
		require.Emptyf(t, e.ps(), "round %d", round)
		require.Equalf(t, []string{"keep"}, e.volumes(), "round %d", round)
		require.Equalf(t, []string{keepNet}, e.networks(), "round %d", round)
	}

	// After all of it the daemon does what it did before.
	g = e.startGuest(guestOpts{name: "keeper", network: keepNet, volume: "keep"})
	require.Equal(t, "must survive every crash", g.read("note"))
	e.stopAndRemove(g.id)
	rep, code := e.doctor("--deep")
	require.Zero(t, code, "doctor --deep after the crashes: %+v", rep.Findings)
}

// smallDisk mounts a filesystem of the given size and returns its mount
// point: a RAM-backed tmpfs on Linux (needs root), a disk image on macOS.
func smallDisk(t *testing.T, megabytes int) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "jb-e2e-disk-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	mnt := filepath.Join(dir, "mnt")
	require.NoError(t, os.Mkdir(mnt, 0o700))
	switch runtime.GOOS {
	case "linux":
		out, err := exec.Command("mount", "-t", "tmpfs", "-o", fmt.Sprintf("size=%dm,mode=0700", megabytes), "jb-e2e-disk", mnt).CombinedOutput()
		if err != nil {
			missing(t, "mounting a small filesystem needs root: %v: %s", err, out)
		}
		t.Cleanup(func() {
			if out, err := exec.Command("umount", "-l", mnt).CombinedOutput(); err != nil {
				t.Logf("umount %s: %v: %s", mnt, err, out)
			}
		})
	case "darwin":
		img := filepath.Join(dir, "disk.dmg")
		out, err := exec.Command("hdiutil", "create", "-size", fmt.Sprintf("%dm", megabytes), "-fs", "HFS+", "-volname", "jbe2e", img).CombinedOutput()
		require.NoErrorf(t, err, "hdiutil create: %s", out)
		out, err = exec.Command("hdiutil", "attach", "-nobrowse", "-mountpoint", mnt, img).CombinedOutput()
		require.NoErrorf(t, err, "hdiutil attach: %s", out)
		t.Cleanup(func() {
			if out, err := exec.Command("hdiutil", "detach", "-force", mnt).CombinedOutput(); err != nil {
				t.Logf("hdiutil detach %s: %v: %s", mnt, err, out)
			}
		})
	default:
		t.Skipf("no small filesystem on %s", runtime.GOOS)
	}
	return mnt
}

// fillDisk writes a file under dir until the filesystem is full and returns
// its path.
func fillDisk(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ballast")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	for _, size := range []int{1 << 20, 4 << 10, 1} {
		chunk := make([]byte, size)
		for {
			if _, err := f.Write(chunk); err != nil {
				break
			}
		}
	}
	_ = f.Sync()
	return path
}

// TestHostDiskFull runs the daemon on a small filesystem and fills it up.
// With no space left a build must fail with an error, not hang or leave a
// half-written image that is then listed; the daemon must keep answering,
// keep serving the guest that is running, and start again if it is killed.
// Once space is freed the same build must succeed, and what existed before
// must be intact.
func TestHostDiskFull(t *testing.T) {
	slow(t)
	mnt := smallDisk(t, 700)
	home := filepath.Join(mnt, "home")
	e := newEnv(t, withHome(home))
	digest := e.buildProbe()
	netName, _ := e.network()
	e.ok("volume", "create", "data", "--size", "32M")
	g := e.startGuest(guestOpts{name: "survivor", network: netName, volume: "data", env: []string{"PROBE_MARKER=survivor"}})
	g.write("note", "written before the disk filled up")

	ballast := fillDisk(t, mnt)
	t.Cleanup(func() { _ = os.Remove(ballast) })

	// A build has nowhere to put its image.
	second := filepath.Join(e.root, "second")
	writeProject(t, second, "", "")
	main := filepath.Join(second, "main.go")
	src, err := os.ReadFile(main)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(main, append(src, []byte("\nconst salt = \"second\"\n\nfunc init() { _ = salt }\n")...), 0o600))
	res := e.runFor(3*time.Minute, buildArgs(second, "second")...)
	require.Equalf(t, 1, res.code, "a build on a full disk must fail with an error:\n%s", res.all())
	require.False(t, e.hasImage("second"), "a build that failed for lack of space left an image")

	// The daemon still answers, and the running guest still serves.
	require.Equal(t, "running", e.vmState(g.id))
	require.Equal(t, "survivor", g.status().Marker)
	require.Equal(t, digest, e.imageDigest("probe"))
	e.df()

	// Killed while the disk is full, it starts again on the full disk.
	e.killDaemon()
	require.True(t, e.tryStartDaemon(90*time.Second), "the daemon did not start on a full disk; log:\n%s", e.daemonLog())
	require.Equal(t, digest, e.imageDigest("probe"))
	require.Contains(t, e.volumes(), "data")
	requireNoOrphans(e, g.id)

	// Space is back: the build that failed now succeeds, nothing is damaged.
	require.NoError(t, os.Remove(ballast))
	res = e.runFor(3*time.Minute, buildArgs(second, "second")...)
	require.Zerof(t, res.code, "the build after space was freed:\n%s", res.all())
	require.True(t, e.hasImage("second"))
	if e.vmState(g.id) == "running" {
		require.Equal(t, "written before the disk filled up", g.read("note"))
		e.ok("stop", g.id)
		e.waitState(g.id, "stopped", 60*time.Second)
	}
	e.ok("rm", g.id)
	again := e.startGuest(guestOpts{name: "after", image: "second", network: netName, volume: "data"})
	require.Equal(t, "written before the disk filled up", again.read("note"))
	e.stopAndRemove(again.id)
	// Not the exit status: on a disk this small the doctor rightly reports
	// how little space is left.
	rep, _ := e.doctor("--deep")
	require.Empty(t, rep.problems(), "doctor --deep after the disk was full")
}

// TestGuestVolumeFull fills a guest's volume from inside. The guest must be
// told there is no space left, must keep running, and what it had written
// before must still be on the volume at the next boot.
func TestGuestVolumeFull(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()
	netName, _ := e.network()
	e.ok("volume", "create", "tiny", "--size", "24M")
	g := e.startGuest(guestOpts{name: "filler", network: netName, volume: "tiny"})
	g.write("note", "written before the volume filled up")

	var fill struct {
		Written int64  `json:"written"`
		Error   string `json:"error"`
	}
	code, body, err := httpGet(g.url("/fill"))
	require.NoError(t, err, "the guest stopped answering while it filled its volume")
	require.Equal(t, 200, code, body)
	require.NoError(t, jsonUnmarshal(body, &fill))
	require.NotEmptyf(t, fill.Error, "the guest wrote %d bytes to a 24M volume without an error", fill.Written)
	require.Contains(t, fill.Error, "no space left")
	require.Less(t, fill.Written, int64(24<<20))
	require.Greater(t, fill.Written, int64(8<<20), "the volume took far less than its size")

	// The guest lives on and still reads what it wrote before.
	require.Equal(t, "running", e.vmState(g.id))
	require.Equal(t, "written before the volume filled up", g.read("note"))
	e.stopAndRemove(g.id)

	// The host's volume file did not grow past the volume's size.
	fi, err := os.Stat(filepath.Join(e.home, ".jerboa", "volumes", "tiny", "disk.img"))
	require.NoError(t, err)
	require.LessOrEqual(t, fi.Size(), int64(25<<20))

	again := e.startGuest(guestOpts{name: "reader", network: netName, volume: "tiny"})
	require.Equal(t, "written before the volume filled up", again.read("note"))
	require.Equal(t, 2, again.status().Boots)
	e.stopAndRemove(again.id)
	e.requireHealthy()
}
