//go:build linux || (darwin && arm64)

package upgradetest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/AitorConS/jerboa/internal/upgradetest"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests open the state a released version left on disk with the
// current stores, one store at a time, and compare what they see with what
// that version reported. cmd/jerboad starts the whole daemon over the same
// state.

// TestFixturesCoverTheLatestRelease keeps the fixtures from going stale: the
// newest one must be of the version this source was last released as, or of
// the one before it while the next release is being prepared.
func TestFixturesCoverTheLatestRelease(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("fixtures are recorded on Linux and macOS")
	}
	version, err := os.ReadFile(filepath.Join("..", "..", "VERSION.md"))
	require.NoError(t, err)
	current := "v" + string(trimSpace(version))
	var newest string
	for _, f := range upgradetest.All(t) {
		if f.OS == "linux" {
			newest = f.Version // All is sorted, oldest first
		}
	}
	require.NotEmpty(t, newest, "no Linux fixture is committed")
	major, minor, patch := parseVersion(t, current)
	nMajor, nMinor, nPatch := parseVersion(t, newest)
	// The fixture of X.Y.Z is recorded once X.Y.Z is published, so while
	// VERSION.md already names the next release the newest fixture is one
	// release behind: the same minor, or the minor before it.
	behind := major == nMajor && (minor == nMinor && patch >= nPatch || minor == nMinor+1)
	assert.True(t, behind,
		"VERSION.md is %s but the newest Linux upgrade fixture is %s: record the released version with "+
			"scripts/upgrade/make-fixture-docker.sh <version> and commit internal/upgradetest/testdata", current, newest)
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func parseVersion(t *testing.T, v string) (major, minor, patch int) {
	t.Helper()
	var parts [3]int
	n := 0
	for i := 1; i < len(v) && n < 3; i++ {
		switch {
		case v[i] >= '0' && v[i] <= '9':
			parts[n] = parts[n]*10 + int(v[i]-'0')
		case v[i] == '.':
			n++
		default:
			t.Fatalf("version %q", v)
		}
	}
	require.Equal(t, 2, n, "version %q", v)
	return parts[0], parts[1], parts[2]
}

func TestRestoreGivesBackTheRecordedState(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		require.NotEmpty(t, f.Files)
		for _, want := range f.Files {
			info, err := os.Lstat(filepath.Join(f.Root, filepath.FromSlash(want.Path)))
			require.NoError(t, err, want.Path)
			assert.Equal(t, want.Perm(), info.Mode().Perm(), "permissions of %s", want.Path)
			assert.Equal(t, want.Dir, info.IsDir(), want.Path)
		}
		// The scenario left something of every kind.
		e := f.Expected
		assert.GreaterOrEqual(t, len(e.Images), 3)
		assert.GreaterOrEqual(t, len(e.VMs), 5)
		assert.GreaterOrEqual(t, len(e.Volumes), 3)
		assert.GreaterOrEqual(t, len(e.Networks), 3)
		// (Releases before the registry had no login.)
		_, err := os.Stat(filepath.Join(f.Data(), "credentials.json"))
		assert.Equal(t, err == nil, len(e.Creds) > 0, "credentials are recorded when the release stored any")
		assert.NotEmpty(t, e.Compose.State.Services)
		assert.Len(t, e.SQLite.VMs, 2)
	})
}

func TestImagesAndNamesSurvive(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		store, err := image.NewStore(filepath.Join(f.Data(), "images"))
		require.NoError(t, err)
		manifests, err := store.List()
		require.NoError(t, err)

		// Every name the released version listed, on the same disk, with the
		// same recorded properties.
		type row struct {
			Name, Tag, Digest, Platform, Architecture, Recipe string
			Size                                              int64
			Created                                           string
		}
		var got, want []row
		for _, m := range manifests {
			got = append(got, row{m.Name, m.Tag, m.DiskDigest, m.Platform, m.Architecture, m.Recipe, m.DiskSize, m.Created.UTC().Format(time.RFC3339)})
		}
		for _, m := range f.Expected.Images {
			created, err := time.Parse(time.RFC3339, str(m, "created"))
			require.NoError(t, err)
			want = append(want, row{str(m, "name"), str(m, "tag"), str(m, "disk_digest"), str(m, "platform"), str(m, "architecture"),
				str(m, "recipe"), int64(m["disk_size"].(float64)), created.UTC().Format(time.RFC3339)})
		}
		assert.ElementsMatch(t, want, got)

		refs, err := store.Refs()
		require.NoError(t, err)
		assert.Len(t, refs, len(want))
		for _, w := range want {
			assert.Equal(t, w.Digest, refs[w.Name+":"+w.Tag])
			m, disk, err := store.Get(w.Name + ":" + w.Tag)
			require.NoError(t, err)
			assert.Equal(t, w.Digest, m.DiskDigest)
			assert.FileExists(t, disk)
			// The stored disk still is the image, bit for bit.
			require.NoError(t, store.VerifyDisk(w.Digest))
			// And the released version's verification record is honored,
			// so the first start after an upgrade does not hash every disk.
			assert.True(t, store.Trusted(disk, w.Digest), "%s:%s was verified by the released version", w.Name, w.Tag)
		}

		// A disk a rebuild left without a name stays while a VM boots from
		// it, and the first prune after the upgrade does not take it.
		files := vm.NewFileStore(filepath.Join(f.Data(), "vms"))
		require.NoError(t, files.Restore())
		inUse := vm.ImageDigestsInUse(files.List())
		kept := f.Expected.ReplacedImages.InUse
		require.True(t, inUse[kept], "a recorded VM boots from the replaced image")
		// What the first prune may take is exactly what no name and no VM
		// refers to: nothing in a store of a release that cleaned up after
		// a rebuild itself, the replaced disks in one of a release that did
		// not.
		var orphans []string
		for _, dir := range f.Expected.ImageDirs {
			digest := "sha256:" + dir
			named := false
			for _, w := range want {
				named = named || w.Digest == digest
			}
			if !named && !inUse[digest] {
				orphans = append(orphans, digest)
			}
		}
		removed, err := store.PruneUnreferenced(func(d string) bool { return inUse[d] })
		require.NoError(t, err)
		assert.ElementsMatch(t, orphans, removed)
		require.NoError(t, store.VerifyDisk(kept))
		after, err := store.List()
		require.NoError(t, err)
		assert.Len(t, after, len(manifests))
	})
}

// TestImageNamesAreRebuiltFromAReleasedStore damages refs.json: the names
// must come back from what the released version wrote in each image
// directory, including a tag it moved to another disk.
func TestImageNamesAreRebuiltFromAReleasedStore(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		root := filepath.Join(f.Data(), "images")
		for name, damage := range map[string][]byte{"truncated": {}, "garbage": []byte("{\"probe:latest\": "), "null": []byte("null")} {
			t.Run(name, func(t *testing.T) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "refs.json"), damage, 0o600))
				store, err := image.NewStore(root)
				require.NoError(t, err)
				refs, err := store.Refs()
				require.NoError(t, err)
				want := map[string]string{}
				for _, m := range f.Expected.Images {
					want[str(m, "name")+":"+str(m, "tag")] = str(m, "disk_digest")
				}
				assert.Equal(t, want, refs)
			})
		}
	})
}

func TestVMRecordsSurvive(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		store := vm.NewFileStore(filepath.Join(f.Data(), "vms"))
		require.NoError(t, store.Restore())
		vms := store.List()
		require.Len(t, vms, len(f.Expected.VMs))
		for _, v := range vms {
			want, ok := f.Expected.VMs[v.Cfg.Name]
			require.True(t, ok, "VM %q was not recorded", v.Cfg.Name)
			assertSameVM(t, want, v)
			// A VM that was up when the released daemon stopped is recorded
			// as running, by a process that no longer exists: it comes back
			// stopped and marked as such, never as running.
			assert.Equal(t, vm.StateStopped, v.State, v.Cfg.Name)
			assert.Equal(t, want.State == "running", v.DaemonRecovered, v.Cfg.Name)
			info, err := os.Stat(filepath.Join(f.Data(), "vms", v.ID, "state.json"))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a VM record holds its environment")
		}
	})
}

// assertSameVM compares a VM record with what the released CLI printed.
func assertSameVM(t *testing.T, want upgradetest.VM, v *vm.VM) {
	t.Helper()
	name := want.Name
	assert.Equal(t, want.ID, v.ID, name)
	assert.Equal(t, want.Image, v.Cfg.ImageRef, name)
	assert.Equal(t, want.Memory, v.Cfg.Memory, name)
	assert.Equal(t, want.CPUs, v.Cfg.CPUs, name)
	assert.Equal(t, want.Env, v.Cfg.Env, name)
	assert.Equal(t, want.IPAddress, v.Cfg.IPAddress, name)
	assert.Equal(t, want.GatewayIP, v.Cfg.GatewayIP, name)
	assert.Equal(t, want.RestartPolicy, string(v.Cfg.Restart.Policy), name)
	// The CLI prints times in the zone of the machine it runs on.
	created, err := time.Parse(time.RFC3339, want.CreatedAt)
	require.NoError(t, err, name)
	assert.True(t, created.Equal(v.CreatedAt.Truncate(time.Second)), "created_at of %s: %s, recorded %s", name, v.CreatedAt, want.CreatedAt)
	assert.Equal(t, want.Health != "", v.Cfg.HealthCheck != nil, name)
	require.Len(t, v.Cfg.Volumes, len(want.Volumes), name)
	for i, raw := range want.Volumes {
		var mount struct {
			DiskPath  string `json:"disk_path"`
			GuestPath string `json:"guest_path"`
			Label     string `json:"label"`
		}
		require.NoError(t, json.Unmarshal(raw, &mount))
		assert.Equal(t, mount.DiskPath, v.Cfg.Volumes[i].DiskPath, name)
		assert.Equal(t, mount.GuestPath, v.Cfg.Volumes[i].GuestPath, name)
		assert.Equal(t, mount.Label, v.Cfg.Volumes[i].Label, name)
		assert.FileExists(t, mount.DiskPath)
	}
	require.Len(t, v.Cfg.PortMaps, len(want.Ports), name)
	for i, raw := range want.Ports {
		var port struct {
			BindAddr  string `json:"bind_addr"`
			HostPort  int    `json:"host_port"`
			GuestPort int    `json:"guest_port"`
			Protocol  string `json:"protocol"`
		}
		require.NoError(t, json.Unmarshal(raw, &port))
		got := v.Cfg.PortMaps[i]
		assert.Equal(t, port.BindAddr, got.BindAddr, name)
		assert.Equal(t, port.HostPort, int(got.HostPort), name)
		assert.Equal(t, port.GuestPort, int(got.GuestPort), name)
		assert.Equal(t, port.Protocol, string(got.Protocol), name)
	}
}

func TestVolumesSurvive(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		store, err := volume.NewStore(filepath.Join(f.Data(), "volumes"))
		require.NoError(t, err)
		vols, err := store.List()
		require.NoError(t, err)
		require.Len(t, vols, len(f.Expected.Volumes))
		recorded := map[string]upgradetest.File{}
		for _, file := range f.Files {
			recorded[filepath.Join(f.Root, filepath.FromSlash(file.Path))] = file
		}
		for _, want := range f.Expected.Volumes {
			v, err := store.Get(str(want, "id"))
			require.NoError(t, err)
			assert.Equal(t, str(want, "disk_path"), v.DiskPath)
			assert.Equal(t, str(want, "label"), v.Label)
			assert.Equal(t, int64(want["size_bytes"].(float64)), v.SizeBytes)
			created, err := time.Parse(time.RFC3339Nano, str(want, "created_at"))
			require.NoError(t, err)
			assert.True(t, created.Equal(v.CreatedAt), "created_at of %s", v.ID)
			// Opening and listing the store leaves the data alone.
			assert.Equal(t, recorded[v.DiskPath].SHA256, fileSHA256(t, v.DiskPath), "content of volume %s", v.ID)
			formatted, err := volume.IsFormatted(v.DiskPath)
			require.NoError(t, err)
			assert.Equal(t, volumeWasAttached(f, v.DiskPath), formatted, "volume %s", v.ID)
		}
	})
}

// volumeWasAttached reports whether a recorded VM mounted the volume, which
// is when the released daemon formatted it.
func volumeWasAttached(f *upgradetest.Fixture, disk string) bool {
	for _, v := range f.Expected.VMs {
		for _, raw := range v.Volumes {
			var mount struct {
				DiskPath string `json:"disk_path"`
			}
			if json.Unmarshal(raw, &mount) == nil && mount.DiskPath == disk {
				return true
			}
		}
	}
	return false
}

func TestNetworksAndLeasesSurvive(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		store, err := network.NewStore(filepath.Join(f.Data(), "networks"))
		require.NoError(t, err)
		nets, err := store.List()
		require.NoError(t, err)
		require.Len(t, nets, len(f.Expected.Networks))
		for _, want := range f.Expected.Networks {
			n, err := store.Get(str(want, "name"))
			require.NoError(t, err)
			assert.Equal(t, str(want, "subnet"), n.Subnet)
			assert.Equal(t, str(want, "gateway"), n.Gateway)
			assert.Equal(t, str(want, "bridge"), n.Bridge)
			// (The driver is printed per platform; cmd/jerboad compares it
			// as the CLI prints it.)
			created, err := time.Parse(time.RFC3339, str(want, "created_at"))
			require.NoError(t, err)
			assert.True(t, created.Equal(n.CreatedAt.Truncate(time.Second)), "created_at of network %s", n.Name)
		}
		// The addresses the recorded VMs hold are still leased: none can be
		// reserved again, and none is handed to a new VM.
		held := map[string]string{}
		store2 := vm.NewFileStore(filepath.Join(f.Data(), "vms"))
		require.NoError(t, store2.Restore())
		for _, v := range store2.List() {
			if v.Cfg.NetworkName == "" || v.Cfg.IPAddress == "" || f.Expected.VMs[v.Cfg.Name].State != "running" {
				continue
			}
			held[v.Cfg.IPAddress] = v.Cfg.NetworkName
			err := store.ReserveIP(v.Cfg.NetworkName, v.Cfg.IPAddress)
			require.ErrorIs(t, err, network.ErrIPAlreadyAllocated, "%s of %s", v.Cfg.IPAddress, v.Cfg.Name)
		}
		require.NotEmpty(t, held, "the scenario leaves VMs running on networks")
		for _, want := range f.Expected.Networks {
			ip, err := store.AllocateIP(str(want, "name"))
			require.NoError(t, err)
			assert.NotEqual(t, str(want, "gateway"), ip.String())
			assert.NotContains(t, held, ip.String())
		}
	})
}

func TestCredentialsAndSettingsSurvive(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		t.Setenv("HOME", f.Home())
		for _, name := range []string{"JERBOA_HOST", "JERBOA_AUTH_TOKEN", credentials.EnvToken, credentials.EnvHost} {
			t.Setenv(name, "")
		}
		creds, err := credentials.Load()
		require.NoError(t, err)
		require.Len(t, creds.Registries, len(f.Expected.Creds))
		for registry, token := range f.Expected.Creds {
			assert.Equal(t, token, creds.Registries[registry].Token)
			got, err := credentials.For(registry)
			require.NoError(t, err)
			assert.Equal(t, token, got)
		}
		// Saving through the current code keeps what was there.
		require.NoError(t, creds.Save())
		again, err := credentials.Load()
		require.NoError(t, err)
		assert.Equal(t, creds, again)

		require.Equal(t, filepath.Join(f.Data(), "config.toml"), config.DefaultPath())
		cfg, err := config.Load(config.DefaultPath())
		require.NoError(t, err)
		want := f.Expected.Config
		assert.Equal(t, want.Hypervisor, cfg.Hypervisor)
		assert.Equal(t, want.Endpoint, cfg.Daemon.Endpoint)
		assert.Equal(t, want.Token, cfg.Daemon.Token)
		if want.Endpoint != "" {
			assert.Equal(t, want.Endpoint, config.ResolveEndpoint(""))
			assert.Equal(t, want.Token, config.ResolveToken())
		}
		// A setting changed after the upgrade does not drop the others.
		cfg.Hypervisor = ""
		require.NoError(t, config.Save(config.DefaultPath(), cfg))
		cfg, err = config.Load(config.DefaultPath())
		require.NoError(t, err)
		assert.Empty(t, cfg.Hypervisor)
		assert.Equal(t, want.Endpoint, cfg.Daemon.Endpoint)
		assert.Equal(t, want.Token, cfg.Daemon.Token)
	})
}

func TestDamagedRecordsDoNotHideTheRest(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		web := f.Expected.VMs["web"]
		require.NotEmpty(t, web.ID)
		// What a power cut leaves of files that were being rewritten.
		require.NoError(t, os.WriteFile(filepath.Join(f.Data(), "vms", web.ID, "state.json"), nil, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(f.Data(), "networks", "appnet", "state.json"), []byte("{"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(f.Data(), "volumes", "spare", "meta.json"), []byte("\x00\x00"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(f.Data(), "credentials.json"), nil, 0o600))

		vms := vm.NewFileStore(filepath.Join(f.Data(), "vms"))
		require.NoError(t, vms.Restore())
		assert.Len(t, vms.List(), len(f.Expected.VMs)-1, "every VM but the damaged one")

		nets, err := network.NewStore(filepath.Join(f.Data(), "networks"))
		require.NoError(t, err)
		list, err := nets.List()
		require.NoError(t, err)
		assert.Len(t, list, len(f.Expected.Networks))
		// The lease file is rebuilt; the gateway is never handed out.
		ip, err := nets.AllocateIP("appnet")
		require.NoError(t, err)
		assert.NotEqual(t, "10.77.0.1", ip.String())

		vols, err := volume.NewStore(filepath.Join(f.Data(), "volumes"))
		require.NoError(t, err)
		_, err = vols.Get("data")
		require.NoError(t, err)
		// The record of a volume is rebuilt from its disk, which is the data.
		spare, err := vols.Get("spare")
		require.NoError(t, err)
		assert.Equal(t, int64(8<<20), spare.SizeBytes)
		all, err := vols.List()
		require.NoError(t, err)
		assert.Len(t, all, len(f.Expected.Volumes))

		t.Setenv("HOME", f.Home())
		creds, err := credentials.Load()
		require.NoError(t, err, "an emptied credentials file is no logins, not a broken CLI")
		assert.Empty(t, creds.Registries)
	})
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
