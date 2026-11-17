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

type usageRow struct {
	Kind        string `json:"kind"`
	Count       int    `json:"count"`
	Active      int    `json:"active"`
	Bytes       int64  `json:"bytes"`
	Reclaimable int64  `json:"reclaimable"`
}

type dfReport struct {
	Usage   []usageRow `json:"usage"`
	DataDir string     `json:"data_dir"`
	Total   int64      `json:"total"`
	Free    int64      `json:"free"`
}

func (e *env) df() map[string]usageRow {
	e.t.Helper()
	var rep dfReport
	e.jsonOut(&rep, "system", "df")
	require.Equal(e.t, filepath.Join(e.home, ".jerboa"), rep.DataDir)
	require.Positive(e.t, rep.Total)
	out := map[string]usageRow{}
	for _, u := range rep.Usage {
		out[u.Kind] = u
	}
	return out
}

type pruneItem struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

type pruneReport struct {
	Removed   []pruneItem `json:"removed"`
	Reclaimed int64       `json:"reclaimed"`
	Errors    []string    `json:"errors"`
	DryRun    bool        `json:"dry_run"`
}

// kinds groups what was removed by kind. A VM is listed as "name (id)":
// only its name is kept.
func (r pruneReport) kinds() map[string][]string {
	out := map[string][]string{}
	for _, it := range r.Removed {
		name := it.Name
		if it.Kind == "vm" {
			name, _, _ = strings.Cut(name, " (")
		}
		out[it.Kind] = append(out[it.Kind], name)
	}
	return out
}

func (e *env) prune(args ...string) pruneReport {
	e.t.Helper()
	var rep pruneReport
	e.jsonOut(&rep, append([]string{"system", "prune"}, args...)...)
	require.Empty(e.t, rep.Errors, "system prune reported removals that failed")
	return rep
}

// rebuildProbe builds the probe again with different content, so the name
// moves to a new disk and the old one is left without a name.
func (e *env) rebuildProbe(image, salt string) string {
	e.t.Helper()
	dir := filepath.Join(e.root, "probe-"+salt)
	writeProject(e.t, dir, "", "")
	main := filepath.Join(dir, "main.go")
	src, err := os.ReadFile(main)
	require.NoError(e.t, err)
	require.NoError(e.t, os.WriteFile(main, append(src, []byte("\nconst salt = \""+salt+"\"\n\nfunc init() { _ = salt }\n")...), 0o600))
	e.ok(buildArgs(dir, image)...)
	return e.imageDigest(image)
}

// imageDisks returns the digests of the image disks on the daemon's disk.
func (e *env) imageDisks() []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.home, ".jerboa", "images"))
	require.NoError(e.t, err)
	var out []string
	for _, ent := range entries {
		if ent.IsDir() {
			out = append(out, "sha256:"+ent.Name())
		}
	}
	return out
}

// TestSystemDFAndPrune fills a daemon with things of every kind, in use and
// not, and checks that `system df` counts them and that `system prune`
// removes exactly what each of its options promises, never what is in use
// and never without being told to.
func TestSystemDFAndPrune(t *testing.T) {
	e := newEnv(t)
	oldDigest := e.buildProbe()
	netName, _ := e.network()
	e.ok("volume", "create", "inuse", "--size", "32M")
	e.ok("volume", "create", "parked", "--size", "32M")
	e.ok("volume", "create", "spare", "--size", "32M")

	// A stopped VM on the first build, with a volume.
	dead := e.startGuest(guestOpts{name: "dead", network: netName, volume: "parked"})
	dead.write("note", "parked data")
	e.ok("stop", dead.id)
	e.waitState(dead.id, "stopped", 60*time.Second)

	// The name moves to a second build; a running VM uses it. A third image
	// is used by nothing.
	newDigest := e.rebuildProbe("probe", "second")
	require.NotEqual(t, oldDigest, newDigest)
	extraDigest := e.rebuildProbe("extra", "third")
	live := e.startGuest(guestOpts{name: "live", network: netName, volume: "inuse", env: []string{"PROBE_MARKER=live"}})
	live.write("note", "live data")

	df := e.df()
	require.Equal(t, 3, df["images"].Count, "df must count the image disks: two names and the one a rebuild left without a name")
	require.Equal(t, 2, df["vms"].Count)
	require.Equal(t, 1, df["vms"].Active)
	require.Equal(t, 3, df["volumes"].Count)
	require.Positive(t, df["images"].Bytes)
	require.Positive(t, df["images"].Reclaimable)
	require.Less(t, df["images"].Reclaimable, df["images"].Bytes, "the image of a running VM is not reclaimable")

	// Without -y and without anyone to ask, prune removes nothing.
	res := e.run("system", "prune", "--vms", "--all", "--volumes")
	require.Equalf(t, 1, res.code, "system prune with nobody to confirm:\n%s", res.all())
	require.Contains(t, res.stderr, "Remove these?")
	require.Len(t, e.ps(), 2)
	require.Len(t, e.volumes(), 3)
	require.Len(t, e.imageDisks(), 3)

	// --dry-run names what a prune would remove and removes nothing.
	plan := e.prune("--dry-run", "--vms", "--all", "--volumes")
	require.True(t, plan.DryRun)
	require.Contains(t, plan.kinds()["vm"], "dead")
	require.NotContains(t, plan.kinds()["vm"], "live")
	require.ElementsMatch(t, []string{"spare", "parked"}, plan.kinds()["volume"], "with --vms, the stopped VM's volume becomes unmounted too")
	require.Len(t, e.ps(), 2)
	require.Len(t, e.volumes(), 3)
	require.Len(t, e.imageDisks(), 3)

	// A plain prune: only what nothing can name any more. The stopped VM
	// still boots from the disk the rebuild left without a name, so that
	// disk stays as long as the VM does.
	plain := e.prune("-y")
	require.Empty(t, plain.kinds()["vm"], "a plain prune must not remove VMs")
	require.Empty(t, plain.kinds()["volume"], "a plain prune must not remove volumes")
	require.Len(t, e.ps(), 2)
	require.Len(t, e.volumes(), 3)
	require.Contains(t, e.imageDisks(), extraDigest, "a plain prune must not remove an image that has a name")
	require.Contains(t, e.imageDisks(), oldDigest, "the image a stopped VM was created from must stay while the VM does")

	// --vms removes the stopped VM, and with it the last use of the old disk.
	vms := e.prune("-y", "--vms")
	require.Equal(t, []string{"dead"}, vms.kinds()["vm"])
	require.Positive(t, vms.Reclaimed)
	rows := e.ps()
	require.Len(t, rows, 1)
	require.Equal(t, "live", rows[0].Name)
	require.NotContains(t, e.imageDisks(), oldDigest, "the image no name and no VM refers to must be removed")
	require.Len(t, e.volumes(), 3, "--vms must not remove volumes")

	// --volumes removes the volumes no VM mounts, not the mounted one.
	vols := e.prune("-y", "--volumes")
	require.ElementsMatch(t, []string{"spare", "parked"}, vols.kinds()["volume"])
	require.Equal(t, []string{"inuse"}, e.volumes())

	// --all removes the image nothing runs, not the running VM's.
	all := e.prune("-y", "--all")
	require.Len(t, all.kinds()["image"], 1)
	require.Contains(t, all.kinds()["image"][0], "extra")
	require.Equal(t, []string{newDigest}, e.imageDisks())
	require.False(t, e.hasImage("extra"))

	// Through all of it the running guest was not touched.
	require.Equal(t, "live", live.status().Marker)
	require.Equal(t, 1, live.status().Boots)
	require.Equal(t, "live data", live.read("note"))

	// Nothing left to do, and the daemon agrees with itself.
	require.Empty(t, e.prune("-y").Removed)
	df = e.df()
	require.Equal(t, 1, df["images"].Count)
	require.Zero(t, df["images"].Reclaimable)
	require.Equal(t, 1, df["volumes"].Count)
	e.requireHealthy()

	// And what survived still works after the VM is replaced.
	e.stopAndRemove(live.id)
	again := e.startGuest(guestOpts{name: "again", network: netName, volume: "inuse"})
	require.Equal(t, "live data", again.read("note"))
}

// TestDoctorFindsDamage damages what the daemon stores, one thing at a
// time, behind its back. `jerboa doctor` must report each, name what is
// damaged and exit non-zero, and must go back to a clean report once the
// damaged thing is removed the way its hint says.
func TestDoctorFindsDamage(t *testing.T) {
	e := newEnv(t)
	digest := e.buildProbe()
	e.ok("volume", "create", "lost", "--size", "32M")
	e.requireHealthy()
	_, code := e.doctor("--deep")
	require.Zero(t, code, "doctor --deep on an untouched store")
	data := filepath.Join(e.home, ".jerboa")

	// A volume whose disk is gone.
	require.NoError(t, os.Remove(filepath.Join(data, "volumes", "lost", "disk.img")))
	rep, code := e.doctor()
	require.Equal(t, 1, code, "doctor must exit non-zero for a volume without its disk")
	require.False(t, rep.OK)
	errs := rep.errorsOf("volumes")
	require.Len(t, errs, 1)
	require.Contains(t, errs[0].Message, "lost")
	require.Contains(t, errs[0].Hint, "jerboa volume rm lost")
	e.ok("volume", "rm", "lost")
	e.requireHealthy()

	// An image disk with a flipped byte: only --deep reads the disks.
	disk := filepath.Join(data, "images", strings.TrimPrefix(digest, "sha256:"), "disk.img")
	content, err := os.ReadFile(disk)
	require.NoError(t, err)
	content[len(content)/2] ^= 0xff
	fi, err := os.Stat(disk)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(disk, 0o600)) // the store keeps its disks read-only
	require.NoError(t, os.WriteFile(disk, content, 0o600))
	require.NoError(t, os.Chmod(disk, fi.Mode().Perm()))
	rep, code = e.doctor("--deep")
	require.Equal(t, 1, code, "doctor --deep must exit non-zero for an image disk that does not match its digest")
	errs = rep.errorsOf("images")
	require.Len(t, errs, 1)
	require.Contains(t, errs[0].Message, "probe")
	// The damaged image must not boot either.
	res := e.run("run", "probe", "--name", "tainted")
	if res.code == 0 {
		e.vmIDs[strings.TrimSpace(res.stdout)] = true
	}
	require.Equalf(t, 1, res.code, "running an image whose disk does not match its digest must be refused:\n%s", res.all())
	e.ok("rmi", "probe")
	_, code = e.doctor("--deep")
	require.Zero(t, code, "doctor --deep after removing the damaged image")

	// With the daemon gone the doctor says so instead of failing.
	e.stopDaemon()
	rep, code = e.doctor()
	require.Equal(t, 1, code)
	errs = rep.errorsOf("daemon")
	require.Len(t, errs, 1)
	require.Contains(t, errs[0].Message, "not reachable")
	e.startDaemon()
}
