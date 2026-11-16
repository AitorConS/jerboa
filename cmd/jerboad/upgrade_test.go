//go:build linux || (darwin && arm64)

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/upgradetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests start this daemon over the home directory a released version
// left behind (internal/upgradetest) and drive it with this CLI, as the
// first minutes after an upgrade do. No VM is started: QEMU is a binary that
// does not exist, so what is checked is everything the daemon and the CLI
// read, migrate and rewrite.

// upgraded is this daemon over a released version's home directory.
type upgraded struct {
	t        *testing.T
	f        *upgradetest.Fixture
	endpoint string
	token    string
}

// newUpgraded points the process at the fixture's home and reads the daemon
// endpoint and token from the settings the released version's installer
// wrote there, which is how the service and the CLI find each other.
func newUpgraded(t *testing.T, f *upgradetest.Fixture) *upgraded {
	t.Helper()
	t.Setenv("HOME", f.Home())
	for _, name := range []string{"JERBOA_HOST", "JERBOA_AUTH_TOKEN", "JERBOA_CLUSTER_TOKEN", "JERBOA_OBSERVABILITY_TOKEN"} {
		t.Setenv(name, "")
	}
	cfg, err := config.Load(config.DefaultPath())
	require.NoError(t, err)
	u := &upgraded{t: t, f: f, endpoint: cfg.Daemon.Endpoint, token: cfg.Daemon.Token}
	if u.endpoint == "" {
		u.endpoint = config.DefaultEndpoint() // macOS: the socket in ~/.jerboa
	}
	return u
}

func (u *upgraded) serve(ctx context.Context) error {
	data := u.f.Data()
	return serve(ctx, u.endpoint, u.token, "", "", "qemu-that-does-not-exist", filepath.Join(data, "images"), "file",
		"", "", "text", "", "", "", "qemu", "firecracker-that-does-not-exist", "", filepath.Join(data, "tools"), filepath.Join(data, "vm-disks"))
}

// start serves until the test ends (or stop is called) and returns a client.
func (u *upgraded) start() (client *api.Client, stop func()) {
	u.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.serve(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			if client != nil {
				_ = client.Close()
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					u.t.Errorf("the daemon must shut down cleanly: %v", err)
				}
			case <-time.After(30 * time.Second):
				u.t.Error("daemon did not stop")
			}
		})
	}
	u.t.Cleanup(stop)
	require.Eventually(u.t, func() bool {
		select {
		case err := <-done:
			done <- err
			u.t.Logf("daemon exited during startup: %v", err)
			return false
		default:
		}
		c, err := api.DialWithToken(u.endpoint, u.token)
		if err != nil {
			return false
		}
		client = c
		return true
	}, 30*time.Second, 20*time.Millisecond, "the daemon did not start over the released state")
	return client, stop
}

var (
	cliOnce sync.Once
	cliPath string
	cliErr  error
)

// cli runs this checkout's jerboa in dir, with the fixture's home and nothing
// else: it must find the daemon through the settings file.
func (u *upgraded) cli(dir string, args ...string) (stdout string, err error) {
	u.t.Helper()
	cliOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "jerboa-cli")
		if err != nil {
			cliErr = err
			return
		}
		cliPath = filepath.Join(tmp, "jerboa")
		out, err := exec.Command("go", "build", "-o", cliPath, "../jerboa").CombinedOutput() //nolint:gosec,noctx // builds this repository's CLI
		if err != nil {
			cliErr = &exec.Error{Name: "go build ../jerboa: " + string(out), Err: err}
		}
	})
	require.NoError(u.t, cliErr)
	cmd := exec.Command(cliPath, args...) //nolint:gosec,noctx // the CLI built above
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + u.f.Home(), "PATH=" + os.Getenv("PATH")}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if err != nil {
		return out.String() + errOut.String(), err
	}
	return out.String(), nil
}

func (u *upgraded) cliJSON(dir string, into any, args ...string) {
	u.t.Helper()
	out, err := u.cli(dir, args...)
	require.NoError(u.t, err, "jerboa %s: %s", strings.Join(args, " "), out)
	require.NoError(u.t, json.Unmarshal([]byte(out), into), "jerboa %s printed %s", strings.Join(args, " "), out)
}

// assertStillPrints checks that every field the released CLI printed for an
// object is printed with the same value now; new fields are fine.
func assertStillPrints(t *testing.T, want, got map[string]any, what string, except ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, k := range except {
		skip[k] = true
	}
	for k, v := range want {
		if skip[k] {
			continue
		}
		// Times are printed in the zone of the machine the CLI runs on.
		if before, ok := v.(string); ok {
			after, _ := got[k].(string)
			t1, err1 := time.Parse(time.RFC3339Nano, before)
			t2, err2 := time.Parse(time.RFC3339Nano, after)
			if err1 == nil && err2 == nil {
				assert.True(t, t1.Equal(t2), "%s: field %q was %s and is %s", what, k, before, after)
				continue
			}
		}
		assert.Equal(t, v, got[k], "%s: field %q", what, k)
	}
}

func byKey(list []map[string]any, keys ...string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range list {
		var id []string
		for _, k := range keys {
			s, _ := m[k].(string)
			id = append(id, s)
		}
		out[strings.Join(id, ":")] = m
	}
	return out
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// assertRecordedState checks, through the API and the CLI, that the daemon
// serves everything the released version reported.
func (u *upgraded) assertRecordedState(client *api.Client) {
	t, f := u.t, u.f
	t.Helper()
	ctx := context.Background()
	want := f.Expected

	vms, err := client.List(ctx)
	require.NoError(t, err)
	require.Len(t, vms, len(want.VMs))
	for _, v := range vms {
		rec, ok := want.VMs[v.Name]
		require.True(t, ok, "VM %q was not there before the upgrade", v.Name)
		assert.Equal(t, rec.ID, v.ID)
		assert.Equal(t, rec.Image, v.Image)
		// Nothing runs any more: a VM that was up is stopped, and says why.
		assert.Equal(t, "stopped", v.State, v.Name)
		detail, err := client.Inspect(ctx, v.ID)
		require.NoError(t, err)
		assert.Equal(t, rec.State == "running", detail.DaemonRecovered, v.Name)
		assert.Equal(t, rec.Env, detail.Env, v.Name)
		assert.Equal(t, rec.Memory, detail.Memory, v.Name)
		assert.Equal(t, rec.IPAddress, detail.IPAddress, v.Name)
	}

	var images, volumes, networks []map[string]any
	u.cliJSON("", &images, "images", "--output", "json")
	require.Len(t, images, len(want.Images))
	got := byKey(images, "name", "tag")
	for key, img := range byKey(want.Images, "name", "tag") {
		require.Contains(t, got, key)
		assertStillPrints(t, img, got[key], "image "+key)
	}
	u.cliJSON("", &volumes, "volume", "ls", "--output", "json")
	require.Len(t, volumes, len(want.Volumes))
	got = byKey(volumes, "id")
	for key, vol := range byKey(want.Volumes, "id") {
		require.Contains(t, got, key)
		assertStillPrints(t, vol, got[key], "volume "+key)
	}
	u.cliJSON("", &networks, "network", "ls", "--output", "json")
	require.Len(t, networks, len(want.Networks))
	got = byKey(networks, "name")
	for key, n := range byKey(want.Networks, "name") {
		require.Contains(t, got, key)
		assertStillPrints(t, n, got[key], "network "+key)
	}
}

func TestUpgrade_DaemonServesAReleasedHome(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		u := newUpgraded(t, f)
		volumes := map[string]string{}
		for _, v := range f.Expected.Volumes {
			path, _ := v["disk_path"].(string)
			volumes[path] = hashFile(t, path)
		}
		client, stop := u.start()
		u.assertRecordedState(client)

		// `jerboa inspect` still prints what it printed, except what the
		// restart changed.
		for name, rec := range f.Expected.VMs {
			var before, after map[string]any
			out, err := u.cli("", "inspect", rec.ID)
			require.NoError(t, err, out)
			require.NoError(t, json.Unmarshal([]byte(out), &after))
			raw, err := json.Marshal(rec)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &before))
			for _, k := range []string{"ports", "volumes", "env"} {
				if before[k] == nil {
					delete(before, k)
				}
			}
			for k, v := range before {
				if v == "" || v == float64(0) {
					delete(before, k)
				}
			}
			assertStillPrints(t, before, after, "inspect "+name, "state", "health")
		}

		// The image a rebuild left without a name is still there for the VM
		// that boots from it: the startup clean-up must not take it.
		kept := strings.TrimPrefix(f.Expected.ReplacedImages.InUse, "sha256:")
		assert.FileExists(t, filepath.Join(f.Data(), "images", kept, "disk.img"))

		// The signature made with the released version verifies. Releases
		// before 0.56.0 signed in a format that is no longer accepted: the
		// CLI says so, and the key they generated signs again.
		require.True(t, f.Expected.Signed)
		out, err := u.cli("", "verify", "probe:latest")
		if f.Before(scopedComposeSince) {
			require.Error(t, err, out)
			assert.Contains(t, out, "legacy format; sign the image again")
			out, err = u.cli("", "sign", "probe:latest")
			require.NoError(t, err, out)
			out, err = u.cli("", "verify", "probe:latest")
		}
		require.NoError(t, err, out)

		// The daemon's own consistency check finds nothing wrong with what
		// it inherited, reading every image disk.
		check, err := client.SystemCheck(context.Background(), api.SystemCheckParams{Deep: true})
		require.NoError(t, err)
		for _, finding := range check.Findings {
			assert.NotEqual(t, api.SeverityError, finding.Severity, "%s: %s", finding.Check, finding.Message)
			if finding.Severity != api.SeverityOK {
				t.Logf("doctor: [%s] %s: %s", finding.Severity, finding.Check, finding.Message)
			}
		}

		// A second start finds the same, and no volume was written to.
		stop()
		client, _ = u.start()
		u.assertRecordedState(client)
		for path, sum := range volumes {
			assert.Equal(t, sum, hashFile(t, path), "volume data of %s", path)
		}
	})
}

// scopedComposeSince is the release whose `compose up` first gave a
// project's networks and volumes names of its own, and whose `jerboa sign`
// first wrote the current signature format.
const scopedComposeSince = "v0.56.0"

// TestUpgrade_ComposeProjectFromBeforeScopedNames pins what happens to a
// project brought up before 0.56.0, whose state names its network and volume
// by their bare names: this version cannot tell such a state from one written
// to make it remove somebody else's resources, so it refuses it, names the
// file, and touches nothing. The project's VMs, volume and network remain and
// are removed with jerboa stop/rm, volume rm and network rm.
func TestUpgrade_ComposeProjectFromBeforeScopedNames(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		if !f.Before(scopedComposeSince) {
			t.Skipf("%s scopes compose names", f.Version)
		}
		u := newUpgraded(t, f)
		client, _ := u.start()
		want := f.Expected.Compose
		project := filepath.Dir(want.File)
		before, err := os.ReadFile(want.StateFile)
		require.NoError(t, err)
		for _, args := range [][]string{{"ps"}, {"down", "--volumes"}, {"up"}} {
			out, err := u.cli(project, append([]string{"compose", args[0], "compose.yaml"}, args[1:]...)...)
			require.Error(t, err, "compose %s: %s", args[0], out)
			assert.Contains(t, out, "is not one of this project's", "compose %s", args[0])
			assert.Contains(t, out, want.StateFile, "compose %s names the file", args[0])
		}
		ctx := context.Background()
		for _, id := range want.State.Services {
			_, err := client.Get(ctx, id)
			require.NoError(t, err, "the service's VM is left alone")
		}
		vols, err := client.VolumeList(ctx)
		require.NoError(t, err)
		assert.Len(t, vols, len(f.Expected.Volumes))
		nets, err := client.NetworkList(ctx)
		require.NoError(t, err)
		assert.Len(t, nets, len(f.Expected.Networks))
		after, err := os.ReadFile(want.StateFile)
		require.NoError(t, err)
		assert.Equal(t, before, after)
		assert.Empty(t, newComposeStateFiles(t, f))

		// The way out: remove what the project created by name, and its
		// state file; the project can then be brought up again.
		for _, id := range want.State.Services {
			require.NoError(t, client.Remove(ctx, id))
		}
		for _, name := range want.State.CreatedVolumes {
			out, err := u.cli("", "volume", "rm", name)
			require.NoError(t, err, out)
		}
		for _, name := range want.State.CreatedNetworks {
			out, err := u.cli("", "network", "rm", name)
			require.NoError(t, err, out)
		}
		require.NoError(t, os.Remove(want.StateFile))
		out, err := u.cli(project, "compose", "ps", "compose.yaml")
		require.Error(t, err)
		assert.Contains(t, out, "run 'jerboa compose up' first")
	})
}

// composeState is the compose state file, old or new.
func readComposeState(t *testing.T, path string) upgradetest.ComposeState {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var s upgradetest.ComposeState
	require.NoError(t, json.Unmarshal(data, &s))
	return s
}

// newComposeStateFiles lists the state files in ~/.jerboa/compose.
func newComposeStateFiles(t *testing.T, f *upgradetest.Fixture) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(f.Data(), "compose", "*.json"))
	require.NoError(t, err)
	return files
}

func TestUpgrade_ComposeProjectIsTakenOver(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		if f.Before(scopedComposeSince) {
			t.Skipf("%s: see TestUpgrade_ComposeProjectFromBeforeScopedNames", f.Version)
		}
		u := newUpgraded(t, f)
		client, _ := u.start()
		want := f.Expected.Compose
		project := filepath.Dir(want.File)
		require.FileExists(t, want.StateFile, "the released version keeps the state next to the compose file")
		require.Empty(t, newComposeStateFiles(t, f))

		var ps []map[string]any
		u.cliJSON(project, &ps, "compose", "ps", "compose.yaml", "--output", "json")
		require.Len(t, ps, len(want.PS))
		for i, row := range want.PS {
			assertStillPrints(t, row, ps[i], "compose ps", "state")
			assert.Equal(t, "stopped", ps[i]["state"])
		}

		// The state moved into the user's own directory, whole, and private.
		assert.NoFileExists(t, want.StateFile)
		moved := newComposeStateFiles(t, f)
		require.Len(t, moved, 1)
		assert.Equal(t, want.State, readComposeState(t, moved[0]))
		info, err := os.Stat(moved[0])
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o077)

		// The project is still managed: asked again it answers the same, a
		// second `up` is refused, and `down` removes what `up` created with
		// the released version and nothing else.
		u.cliJSON(project, &ps, "compose", "ps", "compose.yaml", "--output", "json")
		require.Len(t, ps, len(want.PS))
		out, err := u.cli(project, "compose", "up", "compose.yaml")
		require.Error(t, err)
		assert.Contains(t, out, "already has state")

		out, err = u.cli(project, "compose", "down", "compose.yaml", "--volumes")
		require.NoError(t, err, out)
		ctx := context.Background()
		vms, err := client.List(ctx)
		require.NoError(t, err)
		assert.Len(t, vms, len(f.Expected.VMs)-len(want.State.Services))
		for _, id := range want.State.Services {
			_, err := client.Get(ctx, id)
			require.Error(t, err, "the service's VM is removed")
		}
		nets, err := client.NetworkList(ctx)
		require.NoError(t, err)
		assert.Len(t, nets, len(f.Expected.Networks)-len(want.State.CreatedNetworks))
		vols, err := client.VolumeList(ctx)
		require.NoError(t, err)
		assert.Len(t, vols, len(f.Expected.Volumes)-len(want.State.CreatedVolumes))
		assert.Empty(t, newComposeStateFiles(t, f))
		_, err = u.cli(project, "compose", "ps", "compose.yaml")
		require.Error(t, err, "a project that was taken down has no state, old or new")
	})
}

// TestUpgrade_ComposeTakeoverInterrupted leaves the move of the compose
// state half done, as a CLI killed in the middle does, and expects the
// project to stay manageable with nothing lost.
func TestUpgrade_ComposeTakeoverInterrupted(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		if f.Before(scopedComposeSince) {
			t.Skipf("%s: see TestUpgrade_ComposeProjectFromBeforeScopedNames", f.Version)
		}
		u := newUpgraded(t, f)
		client, _ := u.start()
		want := f.Expected.Compose
		project := filepath.Dir(want.File)
		legacy, err := os.ReadFile(want.StateFile)
		require.NoError(t, err)

		// Take the state over once to learn where it goes, then put the old
		// file back: the new one is written, the old one not yet removed.
		_, err = u.cli(project, "compose", "ps", "compose.yaml")
		require.NoError(t, err)
		moved := newComposeStateFiles(t, f)
		require.Len(t, moved, 1)
		require.NoError(t, os.WriteFile(want.StateFile, legacy, 0o600))
		// And the temporary file of a write that never finished.
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(moved[0]), ".compose-state-123456"), legacy[:len(legacy)/2], 0o600))

		var ps []map[string]any
		u.cliJSON(project, &ps, "compose", "ps", "compose.yaml", "--output", "json")
		require.Len(t, ps, len(want.PS))
		assert.Equal(t, want.State, readComposeState(t, moved[0]))

		out, err := u.cli(project, "compose", "down", "compose.yaml", "--volumes")
		require.NoError(t, err, out)
		for _, id := range want.State.Services {
			_, err := client.Get(context.Background(), id)
			require.Error(t, err)
		}
		// Down is final: the copy the interrupted move left next to the
		// compose file must not bring the project back.
		out, err = u.cli(project, "compose", "ps", "compose.yaml")
		require.Error(t, err, "the project was taken down, but compose ps answers: %s", out)
		assert.NoFileExists(t, want.StateFile)
		assert.Empty(t, newComposeStateFiles(t, f))
	})
}

// TestUpgrade_ComposeStateThatIsNotOurs covers the state files this version
// refuses where the released one obeyed them: nothing they name is touched,
// the error says which file and why, and the file stays for the user to
// inspect.
func TestUpgrade_ComposeStateThatIsNotOurs(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		if f.Before(scopedComposeSince) {
			t.Skipf("%s: see TestUpgrade_ComposeProjectFromBeforeScopedNames", f.Version)
		}
		want := f.Expected.Compose
		project := filepath.Dir(want.File)
		var state map[string]any
		original, err := os.ReadFile(want.StateFile)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(original, &state))
		rewrite := func(change func(map[string]any)) []byte {
			var s map[string]any
			require.NoError(t, json.Unmarshal(original, &s))
			change(s)
			out, err := json.Marshal(s)
			require.NoError(t, err)
			return out
		}
		other := f.Expected.VMs["web"]
		cases := map[string]struct {
			content []byte
			mode    os.FileMode
			message string
		}{
			"writable by others": {original, 0o666, "writable by other users"},
			"names another VM": {rewrite(func(s map[string]any) {
				s["services"] = map[string]any{"api": other.ID}
			}), 0o600, "is not the ID of the VM of service"},
			"names a volume of nobody's": {rewrite(func(s map[string]any) {
				s["created_volumes"] = []string{"data"}
			}), 0o600, "is not one of this project's"},
			"another project's": {rewrite(func(s map[string]any) {
				s["project"] = "/somewhere/else/.jerboa-compose-00.json"
			}), 0o600, "records project"},
			"cut short": {original[:len(original)/2], 0o600, "parse state"},
			"empty":     {nil, 0o600, "parse state"},
		}
		u := newUpgraded(t, f)
		client, _ := u.start()
		for name, c := range cases {
			t.Run(name, func(t *testing.T) {
				require.NoError(t, os.WriteFile(want.StateFile, c.content, 0o600))
				require.NoError(t, os.Chmod(want.StateFile, c.mode)) //nolint:gosec // the case under test
				for _, command := range [][]string{{"ps"}, {"down", "--volumes"}, {"up"}} {
					args := append([]string{"compose", command[0], "compose.yaml"}, command[1:]...)
					out, err := u.cli(project, args...)
					require.Error(t, err, "compose %s: %s", command[0], out)
					assert.Contains(t, out, c.message, "compose %s", command[0])
					assert.Contains(t, out, filepath.Base(want.StateFile), "the error names the file")
				}
				vms, err := client.List(context.Background())
				require.NoError(t, err)
				assert.Len(t, vms, len(f.Expected.VMs), "no VM was stopped or removed")
				vols, err := client.VolumeList(context.Background())
				require.NoError(t, err)
				assert.Len(t, vols, len(f.Expected.Volumes))
				after, err := os.ReadFile(want.StateFile)
				require.NoError(t, err)
				assert.Equal(t, c.content, append([]byte(nil), after...)[:len(c.content)])
				assert.Len(t, after, len(c.content))
				assert.Empty(t, newComposeStateFiles(t, f), "a refused state is not taken over")
			})
		}
	})
}

// TestUpgrade_FirstStartInterrupted kills the first start after the upgrade
// at every step of every file it rewrites, and fails each of them with a
// full disk and an I/O error. Whatever happened, the next start must serve
// everything the released version left.
func TestUpgrade_FirstStartInterrupted(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		u := newUpgraded(t, f)
		// How many files does a first start rewrite?
		inHome := func(path string) bool { return strings.HasPrefix(path, f.Root) }
		writes := durabletest.Count(t, inHome, func() {
			client, stop := u.start()
			u.assertRecordedState(client)
			stop()
		})
		require.Positive(t, writes, "the first start records the VMs it found dead")

		reached := 0
		defer func() { assert.Greater(t, reached, writes, "failures were injected") }()
		for n := 0; n < writes; n++ {
			for _, failure := range durabletest.Failures() {
				// A failed directory flush is not reported by design.
				if failure.Step == durable.StepSyncDir && !failure.Crash() {
					continue
				}
				f.Reset(t)
				inj := durabletest.Inject(t, failure, n, inHome)
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- u.serve(ctx) }()
				// Let the start get as far as it gets: serving, or failing.
				deadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					if c, err := api.DialWithToken(u.endpoint, u.token); err == nil {
						_ = c.Close()
						break
					}
					if len(done) == 1 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Fatalf("write %d %s: daemon did not stop", n, failure.Name)
				}
				durable.SetFault(nil)
				if !inj.Fired() {
					// Records rewritten on every state change are not
					// flushed, so those steps do not exist for them.
					require.Contains(t, []durable.Step{durable.StepSync, durable.StepSyncDir}, failure.Step,
						"write %d %s was not reached", n, failure.Name)
					continue
				}
				reached++

				client, stop := u.start()
				u.assertRecordedState(client)
				stop()
				if t.Failed() {
					t.Fatalf("the state did not survive write %d failing with %s", n, failure.Name)
				}
			}
		}
	})
}
