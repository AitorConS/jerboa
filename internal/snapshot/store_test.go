package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const testMAC = "02:aa:bb:cc:dd:ee"

func writeArtifact(t *testing.T, dir, mac string) {
	t.Helper()
	require.NoError(t, os.Mkdir(dir, 0o700))
	files := map[string][]byte{
		"ram.bin":    bytes.Repeat([]byte{7}, 4096),
		"disk-0.bin": make([]byte, 512),
		"firmware-0": {},
	}
	components := map[string]Component{}
	for name, data := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
		sum := sha256.Sum256(data)
		components[name] = Component{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	}
	manifest := map[string]any{
		"format_version": 1,
		"compatibility":  map[string]any{"host": "test"},
		"config": map[string]any{
			"machine-config":     map[string]any{"vcpu_count": 1, "mem_size_mib": 256},
			"drives":             []any{map[string]any{"drive_id": "rootfs"}},
			"network-interfaces": []any{map[string]any{"iface_id": "eth0", "guest_mac": mac, "backend": "unix-stream"}},
		},
		"components": components,
	}
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600))
}

func openStore(t *testing.T, limits Limits) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "snapshots"), limits)
	require.NoError(t, err)
	return s
}

func publish(t *testing.T, s *Store, name string) Info {
	t.Helper()
	res, err := s.Reserve(name, 1)
	require.NoError(t, err)
	writeArtifact(t, res.ArtifactPath(), testMAC)
	info, err := res.Publish(Info{VMID: "0123456789ab", Backend: "test"}, nil)
	require.NoError(t, err)
	return info
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "snap-1", "db.v2_final", "0abc"} {
		require.NoError(t, ValidateName(ok), ok)
	}
	for _, bad := range []string{"", ".hidden", "../x", "a/b", "A", "a..b", "-x", string(make([]byte, 64))} {
		require.ErrorIs(t, ValidateName(bad), ErrInvalidName, bad)
	}
}

func TestOpenCreatesPrivateLayout(t *testing.T) {
	s := openStore(t, Limits{})
	for _, dir := range []string{s.Root(), filepath.Join(s.Root(), stagingDir), filepath.Join(s.Root(), operationsDir)} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), dir)
	}
}

func TestOpenRejectsSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(base, "real"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")))
	_, err := Open(filepath.Join(base, "link"), Limits{})
	require.ErrorIs(t, err, ErrIntegrity)
}

func TestPublishVerifyListRemove(t *testing.T) {
	s := openStore(t, Limits{})
	info := publish(t, s, "snap1")
	require.Equal(t, 1, info.Version)
	require.Len(t, info.Components, 3)
	require.Positive(t, info.SizeBytes)

	got, artifact, path, err := s.Verify("snap1")
	require.NoError(t, err)
	require.Equal(t, info.ManifestSHA256, got.ManifestSHA256)
	require.Equal(t, testMAC, artifact.Config.Networks[0].GuestMAC)
	require.Equal(t, filepath.Join(s.Root(), "snap1", ArtifactName), path)

	sidecar, err := os.Stat(filepath.Join(s.Root(), "snap1", SidecarName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), sidecar.Mode().Perm())

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, list[0].Err)

	_, err = s.Reserve("snap1", 1)
	require.ErrorIs(t, err, ErrExists)

	require.NoError(t, s.Remove("snap1"))
	_, err = s.Get("snap1")
	require.ErrorIs(t, err, ErrNotFound)
	entries, err := os.ReadDir(filepath.Join(s.Root(), stagingDir))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestReserveQuotas(t *testing.T) {
	s := openStore(t, Limits{MaxCount: 1})
	r, err := s.Reserve("a", 1)
	require.NoError(t, err)
	_, err = s.Reserve("b", 1)
	require.ErrorIs(t, err, ErrQuota, "in-flight reservation counts")
	_, err = s.Reserve("a", 1)
	require.ErrorIs(t, err, ErrExists, "a reserved name cannot be reused concurrently")
	r.Abort()
	r.Abort()
	_, err = os.Stat(r.dir)
	require.ErrorIs(t, err, os.ErrNotExist)

	b := openStore(t, Limits{MaxBytes: 1000})
	_, err = b.Reserve("big", 1001)
	require.ErrorIs(t, err, ErrQuota)
	res, err := b.Reserve("small", 10)
	require.NoError(t, err)
	writeArtifact(t, res.ArtifactPath(), testMAC)
	_, err = res.Publish(Info{}, nil)
	require.ErrorIs(t, err, ErrQuota, "real size is enforced after capture")
	_, err = os.Stat(res.dir)
	require.ErrorIs(t, err, os.ErrNotExist, "rejected artifact is removed")
	_, err = b.Get("small")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPublishCheckRejects(t *testing.T) {
	s := openStore(t, Limits{})
	res, err := s.Reserve("snap", 1)
	require.NoError(t, err)
	writeArtifact(t, res.ArtifactPath(), testMAC)
	_, err = res.Publish(Info{}, func(Artifact) error { return errors.New("wrong vm") })
	require.EqualError(t, err, "wrong vm")
	_, err = s.Get("snap")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Reserve("snap", 1)
	require.NoError(t, err, "name released")
}

func TestVerifyRejectsTampering(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, artifact string){
		"hash": func(t *testing.T, _, artifact string) {
			f, err := os.OpenFile(filepath.Join(artifact, "ram.bin"), os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{9}, 100)
			require.NoError(t, err)
			require.NoError(t, f.Close())
		},
		"symlink component": func(t *testing.T, dir, artifact string) {
			target := filepath.Join(dir, "..", "outside")
			require.NoError(t, os.WriteFile(target, make([]byte, 512), 0o600))
			require.NoError(t, os.Remove(filepath.Join(artifact, "disk-0.bin")))
			require.NoError(t, os.Symlink(target, filepath.Join(artifact, "disk-0.bin")))
		},
		"hard link": func(t *testing.T, dir, artifact string) {
			require.NoError(t, os.Link(filepath.Join(artifact, "ram.bin"), filepath.Join(dir, "..", "alias")))
		},
		"extra entry": func(t *testing.T, _, artifact string) {
			require.NoError(t, os.WriteFile(filepath.Join(artifact, "extra"), nil, 0o600))
		},
		"group readable dir": func(t *testing.T, dir, _ string) {
			require.NoError(t, os.Chmod(dir, 0o750))
		},
		"world readable component": func(t *testing.T, _, artifact string) {
			require.NoError(t, os.Chmod(filepath.Join(artifact, "ram.bin"), 0o644))
		},
		"sidecar unknown field": func(t *testing.T, dir, _ string) {
			path := filepath.Join(dir, SidecarName)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			data = bytes.Replace(data, []byte(`"version"`), []byte(`"socket_path":"/x","version"`), 1)
			require.NoError(t, os.WriteFile(path, data, 0o600))
		},
		"sidecar symlink": func(t *testing.T, dir, _ string) {
			path := filepath.Join(dir, SidecarName)
			require.NoError(t, os.Rename(path, filepath.Join(dir, "..", "sidecar")))
			require.NoError(t, os.Symlink(filepath.Join(dir, "..", "sidecar"), path))
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{})
			publish(t, s, "snap")
			dir := filepath.Join(s.Root(), "snap")
			tamper(t, dir, filepath.Join(dir, ArtifactName))
			_, _, _, err := s.Verify("snap")
			require.ErrorIs(t, err, ErrIntegrity)
		})
	}
}

func TestRemoveRespectsUseAndJournal(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	release, err := s.Acquire("snap")
	require.NoError(t, err)
	require.ErrorIs(t, s.Remove("snap"), ErrInUse)
	release()
	release()

	require.NoError(t, s.WriteOperation(Operation{Kind: OperationRestore, VMID: "0123456789ab", Snapshot: "snap", Phase: "loading", PID: 42}))
	require.ErrorIs(t, s.Remove("snap"), ErrInUse)
	ops, err := s.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 1)
	require.Equal(t, 42, ops[0].PID)
	require.NoError(t, s.RemoveOperation("0123456789ab"))
	require.NoError(t, s.RemoveOperation("0123456789ab"))
	require.NoError(t, s.Remove("snap"))
}

func TestJournalRejectsInvalidRecords(t *testing.T) {
	s := openStore(t, Limits{})
	require.Error(t, s.WriteOperation(Operation{Kind: OperationCreate, VMID: "../../etc"}))
	require.Error(t, s.WriteOperation(Operation{Kind: "clone", VMID: "0123456789ab"}))
	require.NoError(t, os.WriteFile(filepath.Join(s.Root(), operationsDir, "0123456789ab.json"), []byte(`{"version":1,"vm_id":"ffffffffffff"}`), 0o600))
	_, err := s.Operations()
	require.ErrorIs(t, err, ErrIntegrity, "a record naming another VM is not silently ignored")
}

func TestRecoverStaging(t *testing.T) {
	s := openStore(t, Limits{})
	res, err := s.Reserve("partial", 1)
	require.NoError(t, err)
	writeArtifact(t, res.ArtifactPath(), testMAC)
	fresh, err := Open(s.Root(), Limits{})
	require.NoError(t, err)
	removed, err := fresh.RecoverStaging()
	require.NoError(t, err)
	require.Len(t, removed, 1)
	list, err := fresh.List()
	require.NoError(t, err)
	require.Empty(t, list)
}
