package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The snapshot store after things go wrong: a fork that died before it wrote
// the artifact, a daemon killed between two steps of a publish or a removal, a
// sidecar or journal record a power cut emptied, a quota reached exactly. A
// damaged entry must cost at most that snapshot, a half-made one must never be
// visible under its name, and the journal must never be silently ignored.

const faultVMID = "0123456789ab"

// reopen simulates a daemon restart: reservations and holds live in memory, so
// a new Store on the same root has forgotten them.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	fresh, err := Open(s.Root(), s.Limits())
	require.NoError(t, err)
	return fresh
}

func stagingEntries(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Root(), stagingDir))
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func listed(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.List()
	require.NoError(t, err)
	var out []string
	for _, e := range list {
		out = append(out, e.Info.Name)
	}
	return out
}

// artifactSize is what one writeArtifact snapshot weighs once published.
func artifactSize(t *testing.T) int64 {
	t.Helper()
	s := openStore(t, Limits{})
	return publish(t, s, "probe").SizeBytes
}

// rewriteManifest replaces the manifest of the artifact in dir with one whose
// decoded form was changed by edit.
func rewriteManifest(t *testing.T, dir string, edit func(m map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, manifestName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	edit(m)
	data, err = json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
}

// --- Reserve -----------------------------------------------------------

func TestFaults_ReserveThatCannotStageHoldsNothing(t *testing.T) {
	skipIfRoot(t)
	s := openStore(t, Limits{MaxCount: 1})
	staging := filepath.Join(s.Root(), stagingDir)
	// The staging directory cannot be written: a read-only or full filesystem.
	require.NoError(t, os.Chmod(staging, 0o500))
	_, err := s.Reserve("snap", 1)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrQuota)

	require.NoError(t, os.Chmod(staging, 0o700))
	// Neither the name nor the single quota slot may stay taken by the
	// reservation that failed.
	res, err := s.Reserve("snap", 1)
	require.NoError(t, err, "a failed reservation kept the name or the quota")
	res.Abort()
}

func TestFaults_ReserveWhenStoreDirectoryIsGone(t *testing.T) {
	s := openStore(t, Limits{})
	require.NoError(t, os.RemoveAll(s.Root()))
	_, err := s.Reserve("snap", 1)
	require.Error(t, err)
	_, _, err = s.Usage()
	require.Error(t, err)
	_, err = s.List()
	require.Error(t, err)
	_, err = s.RecoverStaging()
	require.Error(t, err)

	// Opening the store again rebuilds the layout.
	fresh := reopen(t, s)
	res, err := fresh.Reserve("snap", 1)
	require.NoError(t, err)
	res.Abort()
}

func TestFaults_ConcurrentReserveOfOneNameHasOneWinner(t *testing.T) {
	s := openStore(t, Limits{})
	const callers = 16
	var wg sync.WaitGroup
	var won atomic.Int32
	results := make([]*Reservation, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.Reserve("snap", 1)
			if err == nil {
				won.Add(1)
				results[i] = res
				return
			}
			assert.ErrorIs(t, err, ErrExists)
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(1), won.Load())
	assert.Len(t, stagingEntries(t, s), 1, "a losing caller left a staging directory behind")
	for _, res := range results {
		if res != nil {
			res.Abort()
		}
	}
	assert.Empty(t, stagingEntries(t, s))
}

func TestFaults_ConcurrentReserveNeverExceedsTheCountLimit(t *testing.T) {
	const limit = 3
	s := openStore(t, Limits{MaxCount: limit})
	publish(t, s, "existing")
	const callers = 12
	var wg sync.WaitGroup
	won := make([]*Reservation, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Reserve(fmt.Sprintf("snap%d", i), 1)
			if err != nil {
				assert.ErrorIs(t, err, ErrQuota)
				return
			}
			won[i] = res
		}()
	}
	wg.Wait()
	winners := 0
	for _, res := range won {
		if res == nil {
			continue
		}
		winners++
		writeArtifact(t, res.ArtifactPath(), testMAC)
		_, err := res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, limit-1, winners)
	count, _, err := s.Usage()
	require.NoError(t, err)
	assert.Equal(t, limit, count)
}

// --- Publish -----------------------------------------------------------

// A publish that fails for any reason must leave no entry under the name, no
// staging directory, and neither the name nor the quota reserved.
func requireNothingPublished(t *testing.T, s *Store, name string) {
	t.Helper()
	_, err := s.Get(name)
	require.ErrorIs(t, err, ErrNotFound, "a failed publish is visible under its name")
	assert.Empty(t, listed(t, s))
	assert.Empty(t, stagingEntries(t, s), "a failed publish left its staging directory")
	count, used, err := s.Usage()
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.Zero(t, used)

	res, err := s.Reserve(name, 1)
	require.NoError(t, err, "a failed publish kept the name reserved")
	writeArtifact(t, res.ArtifactPath(), testMAC)
	_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
	require.NoError(t, err, "the store cannot publish after a failed publish")
	_, _, _, err = s.Verify(name)
	require.NoError(t, err)
}

func TestFaults_PublishOfADamagedCaptureLeavesNothing(t *testing.T) {
	damage := map[string]func(t *testing.T, artifact string){
		// The fork died before it wrote anything.
		"no artifact": func(t *testing.T, artifact string) { require.NoError(t, os.RemoveAll(artifact)) },
		"empty artifact directory": func(t *testing.T, artifact string) {
			require.NoError(t, os.RemoveAll(artifact))
			require.NoError(t, os.Mkdir(artifact, 0o700))
		},
		// The fork was killed after the memory dump but before the manifest.
		"no manifest": func(t *testing.T, artifact string) {
			require.NoError(t, os.Remove(filepath.Join(artifact, manifestName)))
		},
		"empty manifest": func(t *testing.T, artifact string) {
			require.NoError(t, os.WriteFile(filepath.Join(artifact, manifestName), nil, 0o600))
		},
		"truncated manifest": func(t *testing.T, artifact string) {
			path := filepath.Join(artifact, manifestName)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data[:len(data)/2], 0o600))
		},
		// The disk filled while the memory dump was being written.
		"truncated component": func(t *testing.T, artifact string) {
			require.NoError(t, os.Truncate(filepath.Join(artifact, "ram.bin"), 100))
		},
		"missing component": func(t *testing.T, artifact string) {
			require.NoError(t, os.Remove(filepath.Join(artifact, "ram.bin")))
		},
		"artifact is a file": func(t *testing.T, artifact string) {
			require.NoError(t, os.RemoveAll(artifact))
			require.NoError(t, os.WriteFile(artifact, []byte("x"), 0o600))
		},
		"temporary file left by the fork": func(t *testing.T, artifact string) {
			require.NoError(t, os.WriteFile(filepath.Join(artifact, "ram.bin.tmp"), []byte("half"), 0o600))
		},
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{})
			res, err := s.Reserve("snap", 1)
			require.NoError(t, err)
			writeArtifact(t, res.ArtifactPath(), testMAC)
			fn(t, res.ArtifactPath())

			_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
			require.ErrorIs(t, err, ErrIntegrity)
			requireNothingPublished(t, s, "snap")
		})
	}
}

func TestFaults_PublishWhoseSidecarCannotBeWrittenLeavesNothing(t *testing.T) {
	obstacles := map[string]func(t *testing.T, staged string){
		// Something already sits where the sidecar's temporary file goes.
		"temporary name taken": func(t *testing.T, staged string) {
			require.NoError(t, os.WriteFile(filepath.Join(staged, SidecarName+".tmp"), []byte("{"), 0o600))
		},
		// The rename over the final name fails.
		"sidecar name is a directory": func(t *testing.T, staged string) {
			require.NoError(t, os.Mkdir(filepath.Join(staged, SidecarName), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(staged, SidecarName, "x"), nil, 0o600))
		},
	}
	for name, fn := range obstacles {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{})
			res, err := s.Reserve("snap", 1)
			require.NoError(t, err)
			writeArtifact(t, res.ArtifactPath(), testMAC)
			fn(t, res.dir)

			_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
			require.Error(t, err)
			requireNothingPublished(t, s, "snap")
		})
	}
}

func TestFaults_PublishWhenStagingVanished(t *testing.T) {
	s := openStore(t, Limits{})
	res, err := s.Reserve("snap", 1)
	require.NoError(t, err)
	writeArtifact(t, res.ArtifactPath(), testMAC)
	// An operator (or a cleanup job) emptied the staging area under a capture.
	removed, err := s.RecoverStaging()
	require.NoError(t, err)
	require.Len(t, removed, 1)

	_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
	require.ErrorIs(t, err, ErrIntegrity)
	requireNothingPublished(t, s, "snap")
}

func TestFaults_PublishRefusesANameThatAppearedMeanwhile(t *testing.T) {
	s := openStore(t, Limits{})
	res, err := s.Reserve("snap", 1)
	require.NoError(t, err)
	writeArtifact(t, res.ArtifactPath(), testMAC)
	// An entry with the reserved name shows up behind the store's back, as
	// when a backup is restored into the directory while a capture runs.
	other := filepath.Join(s.Root(), "snap")
	require.NoError(t, os.Mkdir(other, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(other, "keep"), []byte("precious"), 0o600))

	_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
	require.ErrorIs(t, err, ErrExists)
	data, err := os.ReadFile(filepath.Join(other, "keep"))
	require.NoError(t, err, "publishing over an existing entry destroyed it")
	assert.Equal(t, "precious", string(data))
	assert.Empty(t, stagingEntries(t, s))
}

func TestFaults_ReservationIsSpentExactlyOnce(t *testing.T) {
	t.Run("abort after publish keeps the snapshot", func(t *testing.T) {
		s := openStore(t, Limits{})
		res, err := s.Reserve("snap", 1)
		require.NoError(t, err)
		writeArtifact(t, res.ArtifactPath(), testMAC)
		_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.NoError(t, err)
		// Callers defer Abort; it must not undo a publish that succeeded.
		res.Abort()
		_, _, _, err = s.Verify("snap")
		require.NoError(t, err, "a deferred Abort removed a published snapshot")
	})
	t.Run("second publish fails and keeps the first", func(t *testing.T) {
		s := openStore(t, Limits{})
		res, err := s.Reserve("snap", 1)
		require.NoError(t, err)
		writeArtifact(t, res.ArtifactPath(), testMAC)
		_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.NoError(t, err)
		_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.Error(t, err)
		_, _, _, err = s.Verify("snap")
		require.NoError(t, err)
	})
	t.Run("publish after abort", func(t *testing.T) {
		s := openStore(t, Limits{})
		res, err := s.Reserve("snap", 1)
		require.NoError(t, err)
		writeArtifact(t, res.ArtifactPath(), testMAC)
		res.Abort()
		_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.Error(t, err)
		_, err = s.Get("snap")
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("abort of an old reservation spares its successor", func(t *testing.T) {
		s := openStore(t, Limits{})
		old, err := s.Reserve("snap", 1)
		require.NoError(t, err)
		old.Abort()
		next, err := s.Reserve("snap", 1)
		require.NoError(t, err)
		old.Abort()
		_, err = s.Reserve("snap", 1)
		require.ErrorIs(t, err, ErrExists, "a repeated Abort released a name it no longer owned")
		_, err = os.Stat(next.dir)
		require.NoError(t, err)
	})
}

func TestFaults_PublishRacingAbort(t *testing.T) {
	for range 30 {
		s := openStore(t, Limits{})
		res, err := s.Reserve("snap", 1)
		require.NoError(t, err)
		writeArtifact(t, res.ArtifactPath(), testMAC)

		var wg sync.WaitGroup
		var pubErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, pubErr = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil) }()
		go func() { defer wg.Done(); res.Abort() }()
		wg.Wait()

		// Whoever won, the outcome is whole: a verified snapshot or nothing.
		if pubErr == nil {
			_, _, _, err := s.Verify("snap")
			require.NoError(t, err, "publish reported success but the snapshot is not intact")
		} else {
			_, err := s.Get("snap")
			require.ErrorIs(t, err, ErrNotFound)
		}
		assert.Empty(t, stagingEntries(t, s))
		_, err = s.Reserve("other", 1)
		require.NoError(t, err)
	}
}

// --- Crash between steps, then recovery ----------------------------------

func TestFaults_CrashDuringCreateIsInvisibleAndRecovered(t *testing.T) {
	stages := map[string]func(t *testing.T, res *Reservation){
		"after reserve": func(*testing.T, *Reservation) {},
		"artifact half written": func(t *testing.T, res *Reservation) {
			require.NoError(t, os.Mkdir(res.ArtifactPath(), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(res.ArtifactPath(), "ram.bin"), make([]byte, 1000), 0o600))
		},
		"artifact complete": func(t *testing.T, res *Reservation) {
			writeArtifact(t, res.ArtifactPath(), testMAC)
		},
		"sidecar half written": func(t *testing.T, res *Reservation) {
			writeArtifact(t, res.ArtifactPath(), testMAC)
			require.NoError(t, os.WriteFile(filepath.Join(res.dir, SidecarName+".tmp"), []byte(`{"version":1,"na`), 0o600))
		},
		// Everything is in place but the rename that publishes it.
		"sidecar written": func(t *testing.T, res *Reservation) {
			writeArtifact(t, res.ArtifactPath(), testMAC)
			require.NoError(t, os.WriteFile(filepath.Join(res.dir, SidecarName), []byte(`{"version":1,"name":"snap"}`), 0o600))
		},
	}
	for name, fn := range stages {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{MaxCount: 1, MaxBytes: 1 << 20})
			res, err := s.Reserve("snap", 1<<20)
			require.NoError(t, err)
			fn(t, res)

			// The daemon dies here; a new one opens the same store.
			fresh := reopen(t, s)
			_, err = fresh.Get("snap")
			require.ErrorIs(t, err, ErrNotFound, "a half-made snapshot is visible")
			assert.Empty(t, listed(t, fresh))
			count, used, err := fresh.Usage()
			require.NoError(t, err)
			assert.Zero(t, count)
			assert.Zero(t, used, "staged bytes count against the quota of published snapshots")

			removed, err := fresh.RecoverStaging()
			require.NoError(t, err)
			require.Len(t, removed, 1)
			assert.True(t, strings.HasPrefix(removed[0], "snap-"))
			assert.Empty(t, stagingEntries(t, fresh))

			// The dead daemon's reservation holds neither name nor quota.
			again, err := fresh.Reserve("snap", 1<<20)
			require.NoError(t, err)
			writeArtifact(t, again.ArtifactPath(), testMAC)
			_, err = again.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
			require.NoError(t, err)

			// Recovery is idempotent and never touches published entries.
			removed, err = fresh.RecoverStaging()
			require.NoError(t, err)
			assert.Empty(t, removed)
			_, _, _, err = fresh.Verify("snap")
			require.NoError(t, err)
		})
	}
}

func TestFaults_CrashDuringRemoveNeverExposesAPartialEntry(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	publish(t, s, "other")

	// Reproduce Remove up to the point where the entry has been renamed into
	// staging and deletion has started, then die.
	trash := filepath.Join(s.Root(), stagingDir, "removing-snap-0011aabbccdd")
	require.NoError(t, os.Rename(filepath.Join(s.Root(), "snap"), trash))
	require.NoError(t, os.Remove(filepath.Join(trash, SidecarName)))
	require.NoError(t, os.Remove(filepath.Join(trash, ArtifactName, "ram.bin")))

	fresh := reopen(t, s)
	assert.Equal(t, []string{"other"}, listed(t, fresh))
	_, err := fresh.Get("snap")
	require.ErrorIs(t, err, ErrNotFound)
	removed, err := fresh.RecoverStaging()
	require.NoError(t, err)
	assert.Equal(t, []string{"removing-snap-0011aabbccdd"}, removed)
	assert.Empty(t, stagingEntries(t, fresh))
	_, _, _, err = fresh.Verify("other")
	require.NoError(t, err, "recovering one removal damaged another snapshot")

	// The name is free again.
	publish(t, fresh, "snap")
}

func TestFaults_RecoverStagingRemovesWhateverIsThere(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "keep")
	staging := filepath.Join(s.Root(), stagingDir)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "stray-file"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "deep", "a", "b"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(s.Root(), "keep"), filepath.Join(staging, "link-to-keep")))

	removed, err := s.RecoverStaging()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"stray-file", "deep", "link-to-keep"}, removed)
	assert.Empty(t, stagingEntries(t, s))
	// A link in staging is removed, not followed.
	_, _, _, err = s.Verify("keep")
	require.NoError(t, err, "cleaning a staged link deleted the snapshot it pointed to")
}

func TestFaults_RecoverStagingReportsWhatItCouldNotRemove(t *testing.T) {
	skipIfRoot(t)
	s := openStore(t, Limits{})
	res, err := s.Reserve("stuck", 1)
	require.NoError(t, err)
	writeArtifact(t, res.ArtifactPath(), testMAC)
	// The staged directory cannot be emptied.
	require.NoError(t, os.Chmod(res.dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(res.dir, 0o700) })

	fresh := reopen(t, s)
	_, err = fresh.RecoverStaging()
	require.Error(t, err, "staging that could not be cleaned must not be reported as recovered")

	require.NoError(t, os.Chmod(res.dir, 0o700))
	removed, err := fresh.RecoverStaging()
	require.NoError(t, err)
	assert.Len(t, removed, 1)
}

// A daemon killed at arbitrary points of create and remove must leave a store
// in which every visible snapshot is whole.
func TestFaults_StoreSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills helper processes")
	}
	root := filepath.Join(t.TempDir(), "snapshots")
	s, err := Open(root, Limits{})
	require.NoError(t, err)
	publish(t, s, "base")

	for round := range 12 {
		ready := filepath.Join(t.TempDir(), "ready")
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChurnSnapshots$") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), "SNAPSHOT_HELPER_ROOT="+root, "SNAPSHOT_HELPER_READY="+ready)
		require.NoError(t, cmd.Start())
		require.Eventually(t, func() bool {
			_, err := os.Stat(ready)
			return err == nil
		}, 30*time.Second, 2*time.Millisecond, "helper never started")
		time.Sleep(time.Duration(3+round*5) * time.Millisecond)
		require.NoError(t, cmd.Process.Kill())
		_ = cmd.Wait()

		fresh, err := Open(root, Limits{})
		require.NoError(t, err, "round %d", round)
		_, err = fresh.RecoverStaging()
		require.NoError(t, err, "round %d", round)
		assert.Empty(t, stagingEntries(t, fresh))
		list, err := fresh.List()
		require.NoError(t, err)
		names := map[string]bool{}
		for _, e := range list {
			names[e.Info.Name] = true
			require.NoError(t, e.Err, "round %d: %s is listed but unreadable", round, e.Info.Name)
			_, _, _, err := fresh.Verify(e.Info.Name)
			require.NoError(t, err, "round %d: %s is visible but not intact", round, e.Info.Name)
		}
		assert.True(t, names["base"], "round %d: the snapshot that was never touched is gone", round)
		ops, err := fresh.Operations()
		require.NoError(t, err, "round %d: the journal is unreadable after a kill", round)
		for _, op := range ops {
			require.NoError(t, fresh.RemoveOperation(op.VMID))
		}
	}
}

func TestHelperChurnSnapshots(t *testing.T) {
	root := os.Getenv("SNAPSHOT_HELPER_ROOT")
	if root == "" {
		t.Skip("helper process for TestFaults_StoreSurvivesKill")
	}
	s, err := Open(root, Limits{})
	if err != nil {
		os.Exit(3)
	}
	_ = os.WriteFile(os.Getenv("SNAPSHOT_HELPER_READY"), nil, 0o600)
	for i := 0; ; i++ {
		name := fmt.Sprintf("churn%d", i%3)
		vmID := fmt.Sprintf("%012x", i%3)
		_ = s.WriteOperation(Operation{Kind: OperationCreate, VMID: vmID, Snapshot: name, Phase: "capturing"})
		if res, err := s.Reserve(name, 1); err == nil {
			writeArtifact(t, res.ArtifactPath(), testMAC)
			_, _ = res.Publish(Info{VMID: vmID, Backend: "test"}, nil)
		}
		_ = s.RemoveOperation(vmID)
		if i%2 == 0 {
			_ = s.Remove(name)
		}
	}
}

// --- Damaged sidecar -----------------------------------------------------

func TestFaults_DamagedSidecarCostsOnlyThatSnapshot(t *testing.T) {
	damage := map[string]func(t *testing.T, sidecar string){
		"deleted": func(t *testing.T, p string) { require.NoError(t, os.Remove(p)) },
		"empty":   func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, nil, 0o600)) },
		"truncated": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"version":1,"name":"bro`), 0o600))
		},
		"garbage": func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, []byte{0x00, 0xff, 0x10}, 0o600)) },
		"zeroes":  func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, make([]byte, 4096), 0o600)) },
		"null":    func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, []byte("null"), 0o600)) },
		"wrong type": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"version":"one","name":7}`), 0o600))
		},
		"future version": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"version":2,"name":"broken"}`), 0o600))
		},
		// The sidecar of another snapshot, as after a botched manual copy.
		"other name": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"version":1,"name":"fine"}`), 0o600))
		},
		"oversized": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte(" "), maxSidecar+1), 0o600))
		},
		"is a directory": func(t *testing.T, p string) {
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Mkdir(p, 0o700))
		},
		"readable by others": func(t *testing.T, p string) { require.NoError(t, os.Chmod(p, 0o644)) },
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{MaxCount: 2})
			publish(t, s, "broken")
			publish(t, s, "fine")
			fn(t, filepath.Join(s.Root(), "broken", SidecarName))

			_, err := s.Get("broken")
			require.ErrorIs(t, err, ErrIntegrity)
			_, _, _, err = s.Verify("broken")
			require.ErrorIs(t, err, ErrIntegrity, "a snapshot without a valid sidecar must never be handed to a restore")

			// The damaged entry is still listed, flagged, and the other one
			// is unaffected.
			list, err := s.List()
			require.NoError(t, err)
			require.Len(t, list, 2)
			assert.Equal(t, "broken", list[0].Info.Name)
			require.ErrorIs(t, list[0].Err, ErrIntegrity)
			assert.Equal(t, "fine", list[1].Info.Name)
			require.NoError(t, list[1].Err)
			_, _, _, err = s.Verify("fine")
			require.NoError(t, err)

			// It still occupies its name and its quota slot until removed...
			_, err = s.Reserve("broken", 1)
			require.ErrorIs(t, err, ErrExists)
			_, err = s.Reserve("third", 1)
			require.ErrorIs(t, err, ErrQuota)
			// ...and removing it is how the operator gets out of the state.
			require.NoError(t, s.Remove("broken"), "a damaged snapshot cannot be removed")
			assert.Equal(t, []string{"fine"}, listed(t, s))
			res, err := s.Reserve("third", 1)
			require.NoError(t, err)
			res.Abort()
		})
	}
}

func TestFaults_SidecarThatDisagreesWithTheArtifact(t *testing.T) {
	edits := map[string]func(info *Info){
		"manifest digest": func(i *Info) { i.ManifestSHA256 = strings.Repeat("0", 64) },
		"size":            func(i *Info) { i.SizeBytes++ },
		"missing component": func(i *Info) {
			delete(i.Components, "ram.bin")
		},
		"extra component": func(i *Info) {
			i.Components["extra.bin"] = Component{Size: 1, SHA256: strings.Repeat("a", 64)}
		},
		"component digest": func(i *Info) {
			c := i.Components["ram.bin"]
			c.SHA256 = strings.Repeat("b", 64)
			i.Components["ram.bin"] = c
		},
		"component swapped": func(i *Info) {
			i.Components["renamed.bin"] = i.Components["ram.bin"]
			delete(i.Components, "ram.bin")
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{})
			info := publish(t, s, "snap")
			edit(&info)
			data, err := json.Marshal(info)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(s.Root(), "snap", SidecarName), data, 0o600))

			// The sidecar alone still parses, so the cheap read succeeds...
			_, err = s.Get("snap")
			require.NoError(t, err)
			// ...but a restore, which verifies, is refused.
			_, _, _, err = s.Verify("snap")
			require.ErrorIs(t, err, ErrIntegrity)
		})
	}
}

func TestFaults_EntryThatIsNotAPrivateDirectory(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "fine")
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "victim"), []byte("precious"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(s.Root(), "link")))
	require.NoError(t, os.WriteFile(filepath.Join(s.Root(), "file"), []byte("x"), 0o600))

	for _, name := range []string{"link", "file"} {
		_, err := s.Get(name)
		require.ErrorIs(t, err, ErrIntegrity, name)
		_, _, _, err = s.Verify(name)
		require.ErrorIs(t, err, ErrIntegrity, name)
		require.ErrorIs(t, s.Remove(name), ErrIntegrity, "Remove must not act on %s", name)
	}
	// The link was not followed by the refused removal.
	data, err := os.ReadFile(filepath.Join(outside, "victim"))
	require.NoError(t, err)
	assert.Equal(t, "precious", string(data))
	assert.Equal(t, []string{"file", "fine", "link"}, listed(t, s))
}

// --- Damaged artifact ----------------------------------------------------

func TestFaults_DamagedArtifactIsNeverVerified(t *testing.T) {
	damage := map[string]func(t *testing.T, artifact string){
		"artifact directory deleted": func(t *testing.T, a string) { require.NoError(t, os.RemoveAll(a)) },
		"manifest deleted": func(t *testing.T, a string) {
			require.NoError(t, os.Remove(filepath.Join(a, manifestName)))
		},
		"manifest empty": func(t *testing.T, a string) {
			require.NoError(t, os.WriteFile(filepath.Join(a, manifestName), nil, 0o600))
		},
		"manifest zeroes": func(t *testing.T, a string) {
			require.NoError(t, os.WriteFile(filepath.Join(a, manifestName), make([]byte, 512), 0o600))
		},
		"manifest oversized": func(t *testing.T, a string) {
			require.NoError(t, os.WriteFile(filepath.Join(a, manifestName), bytes.Repeat([]byte(" "), maxManifestBytes+1), 0o600))
		},
		"manifest format from the future": func(t *testing.T, a string) {
			rewriteManifest(t, a, func(m map[string]any) { m["format_version"] = 2 })
		},
		"manifest without components": func(t *testing.T, a string) {
			rewriteManifest(t, a, func(m map[string]any) { m["components"] = map[string]any{} })
		},
		"manifest config is not an object": func(t *testing.T, a string) {
			rewriteManifest(t, a, func(m map[string]any) { m["config"] = "nope" })
		},
		"component truncated": func(t *testing.T, a string) {
			require.NoError(t, os.Truncate(filepath.Join(a, "ram.bin"), 4095))
		},
		"component grown": func(t *testing.T, a string) {
			require.NoError(t, os.Truncate(filepath.Join(a, "ram.bin"), 4097))
		},
		"component emptied": func(t *testing.T, a string) {
			require.NoError(t, os.Truncate(filepath.Join(a, "ram.bin"), 0))
		},
		// Same size, one bit flipped at the very end.
		"component last byte": func(t *testing.T, a string) {
			f, err := os.OpenFile(filepath.Join(a, "ram.bin"), os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{6}, 4095)
			require.NoError(t, err)
			require.NoError(t, f.Close())
		},
		"component deleted": func(t *testing.T, a string) {
			require.NoError(t, os.Remove(filepath.Join(a, "disk-0.bin")))
		},
		"component is a directory": func(t *testing.T, a string) {
			require.NoError(t, os.Remove(filepath.Join(a, "disk-0.bin")))
			require.NoError(t, os.Mkdir(filepath.Join(a, "disk-0.bin"), 0o700))
		},
		"zero-length component gained content": func(t *testing.T, a string) {
			require.NoError(t, os.WriteFile(filepath.Join(a, "firmware-0"), []byte("x"), 0o600))
		},
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{})
			publish(t, s, "snap")
			publish(t, s, "fine")
			fn(t, filepath.Join(s.Root(), "snap", ArtifactName))

			_, _, _, err := s.Verify("snap")
			require.ErrorIs(t, err, ErrIntegrity)
			// The sidecar is intact, so the entry lists normally; only a
			// restore discovers the damage.
			_, err = s.Get("snap")
			require.NoError(t, err)
			_, _, _, err = s.Verify("fine")
			require.NoError(t, err)
			require.NoError(t, s.Remove("snap"))
			assert.Equal(t, []string{"fine"}, listed(t, s))
		})
	}
}

func TestFaults_ManifestCannotNameFilesOutsideTheArtifact(t *testing.T) {
	sum := sha256.Sum256([]byte("secret"))
	digest := hex.EncodeToString(sum[:])
	for _, name := range []string{"../outside", "sub/file", ".", "..", manifestName, "/etc/passwd"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "artifact")
			writeArtifact(t, dir, testMAC)
			require.NoError(t, os.WriteFile(filepath.Join(base, "outside"), []byte("secret"), 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "file"), []byte("secret"), 0o600))
			rewriteManifest(t, dir, func(m map[string]any) {
				m["components"].(map[string]any)[name] = map[string]any{"size": 6, "sha256": digest}
			})
			_, err := VerifyArtifact(dir)
			require.ErrorIs(t, err, ErrIntegrity)
		})
	}
}

func TestFaults_ManifestComponentFieldsAreValidated(t *testing.T) {
	for name, comp := range map[string]map[string]any{
		"negative size": {"size": -1, "sha256": strings.Repeat("a", 64)},
		"short digest":  {"size": 0, "sha256": "abc"},
		"empty digest":  {"size": 0, "sha256": ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "artifact")
			writeArtifact(t, dir, testMAC)
			rewriteManifest(t, dir, func(m map[string]any) {
				m["components"].(map[string]any)["firmware-0"] = comp
			})
			_, err := VerifyArtifact(dir)
			require.ErrorIs(t, err, ErrIntegrity)
		})
	}
}

// --- Journal -------------------------------------------------------------

func TestFaults_DamagedJournalIsNeverIgnored(t *testing.T) {
	record := faultVMID + ".json"
	damage := map[string]func(t *testing.T, dir string){
		"empty": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), nil, 0o600))
		},
		"truncated": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), []byte(`{"version":1,"kind":"rest`), 0o600))
		},
		"garbage": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), []byte{0xff, 0x00, 0x7f}, 0o600))
		},
		"zeroes": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), make([]byte, 512), 0o600))
		},
		"null": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), []byte("null"), 0o600))
		},
		"version from the future": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), []byte(`{"version":2,"kind":"restore","vm_id":"`+faultVMID+`"}`), 0o600))
		},
		"unknown field": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), []byte(`{"version":1,"kind":"restore","vm_id":"`+faultVMID+`","extra":1}`), 0o600))
		},
		"readable by others": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, record), []byte(`{"version":1,"kind":"restore","vm_id":"`+faultVMID+`"}`), 0o600))
			require.NoError(t, os.Chmod(filepath.Join(dir, record), 0o644))
		},
		"is a directory": func(t *testing.T, dir string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, record), 0o700))
		},
		"stray file": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600))
		},
		"record named for no VM": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "not-a-vm-id.json"), []byte(`{"version":1}`), 0o600))
		},
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, Limits{})
			publish(t, s, "snap")
			dir := filepath.Join(s.Root(), operationsDir)
			fn(t, dir)

			_, err := s.Operations()
			require.ErrorIs(t, err, ErrIntegrity, "an unreadable record may describe an interrupted restore")
			// With the journal unreadable nobody knows whether the snapshot is
			// still needed by a rollback, so it must not be deleted.
			require.ErrorIs(t, s.Remove("snap"), ErrIntegrity)
			_, _, _, err = s.Verify("snap")
			require.NoError(t, err, "a refused removal damaged the snapshot")

			// Once the operator clears the damage the store works again.
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			for _, e := range entries {
				require.NoError(t, os.RemoveAll(filepath.Join(dir, e.Name())))
			}
			ops, err := s.Operations()
			require.NoError(t, err)
			assert.Empty(t, ops)
			require.NoError(t, s.Remove("snap"))
		})
	}
}

func TestFaults_JournalWriteReplacesADamagedRecord(t *testing.T) {
	s := openStore(t, Limits{})
	path := filepath.Join(s.Root(), operationsDir, faultVMID+".json")
	require.NoError(t, os.WriteFile(path, []byte("{half"), 0o600))
	require.NoError(t, s.WriteOperation(Operation{Kind: OperationCreate, VMID: faultVMID, Snapshot: "snap", Phase: "capturing"}))
	ops, err := s.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, "capturing", ops[0].Phase)
}

func TestFaults_CrashDuringJournalWriteKeepsThePreviousRecord(t *testing.T) {
	s := openStore(t, Limits{})
	require.NoError(t, s.WriteOperation(Operation{Kind: OperationRestore, VMID: faultVMID, Snapshot: "snap", Phase: "prepared", PID: 7}))
	dir := filepath.Join(s.Root(), operationsDir)
	// The daemon died while writing the next phase: its temporary file holds
	// half a record and was never renamed over the old one.
	tmp := filepath.Join(dir, faultVMID+".json.tmp")
	require.NoError(t, os.WriteFile(tmp, []byte(`{"version":1,"kind":"restore","vm_id":"0123`), 0o600))
	// Another VM's first record died the same way.
	orphan := filepath.Join(dir, "ffffffffffff.json.tmp")
	require.NoError(t, os.WriteFile(orphan, nil, 0o600))

	fresh := reopen(t, s)
	ops, err := fresh.Operations()
	require.NoError(t, err, "a leftover temporary file made the journal unreadable")
	require.Len(t, ops, 1)
	assert.Equal(t, "prepared", ops[0].Phase)
	assert.Equal(t, 7, ops[0].PID)
	assert.NoFileExists(t, tmp)
	assert.NoFileExists(t, orphan)

	// The leftover does not block the next write for that VM either.
	require.NoError(t, os.WriteFile(tmp, []byte("{"), 0o600))
	require.NoError(t, fresh.WriteOperation(Operation{Kind: OperationRestore, VMID: faultVMID, Snapshot: "snap", Phase: "loading"}))
	ops, err = fresh.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, "loading", ops[0].Phase)
}

func TestFaults_JournalWriteFailureKeepsThePreviousRecord(t *testing.T) {
	skipIfRoot(t)
	s := openStore(t, Limits{})
	require.NoError(t, s.WriteOperation(Operation{Kind: OperationRestore, VMID: faultVMID, Snapshot: "snap", Phase: "prepared"}))
	dir := filepath.Join(s.Root(), operationsDir)
	// The journal directory cannot be written: read-only or full filesystem.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	require.Error(t, s.WriteOperation(Operation{Kind: OperationRestore, VMID: faultVMID, Snapshot: "snap", Phase: "loading"}))
	require.Error(t, s.WriteOperation(Operation{Kind: OperationCreate, VMID: "ffffffffffff", Snapshot: "snap", Phase: "capturing"}))
	require.Error(t, s.RemoveOperation(faultVMID), "a record that could not be deleted must be reported")

	require.NoError(t, os.Chmod(dir, 0o700))
	ops, err := s.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 1, "a failed write left a record for a VM that had none")
	assert.Equal(t, "prepared", ops[0].Phase)
}

func TestFaults_JournalIsPerVMAndSurvivesRestart(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	require.NoError(t, s.WriteOperation(Operation{Kind: OperationRestore, VMID: faultVMID, Snapshot: "snap", Phase: "prepared"}))
	require.NoError(t, s.WriteOperation(Operation{Kind: OperationCreate, VMID: "ffffffffffff", Snapshot: "snap", Phase: "capturing"}))
	require.NoError(t, s.WriteOperation(Operation{Kind: OperationRestore, VMID: faultVMID, Snapshot: "snap", Phase: "loading"}))

	fresh := reopen(t, s)
	ops, err := fresh.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 2, "one record per VM")
	byVM := map[string]Operation{}
	for _, op := range ops {
		byVM[op.VMID] = op
		assert.Equal(t, 1, op.Version)
		assert.False(t, op.UpdatedAt.IsZero())
	}
	assert.Equal(t, "loading", byVM[faultVMID].Phase)

	// An interrupted restore pins its snapshot across the restart; an
	// interrupted create of the same name does not.
	require.ErrorIs(t, fresh.Remove("snap"), ErrInUse)
	require.NoError(t, fresh.RemoveOperation(faultVMID))
	require.NoError(t, fresh.Remove("snap"))
}

func TestFaults_JournalWritesAndReadsDoNotDisturbEachOther(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	const writers = 4
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writeErr, readErr atomic.Value
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("%012x", w)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := s.WriteOperation(Operation{Kind: OperationCreate, VMID: id, Snapshot: "other", Phase: fmt.Sprint(i)}); err != nil {
					writeErr.Store(err)
					return
				}
			}
		}()
	}
	// Readers: listing the journal, and removals that consult it, run while
	// other VMs record their progress.
	for range 300 {
		if _, err := s.Operations(); err != nil {
			readErr.Store(err)
			break
		}
		if err := s.Remove("absent"); !errors.Is(err, ErrNotFound) {
			readErr.Store(err)
			break
		}
	}
	close(stop)
	wg.Wait()
	if err, _ := writeErr.Load().(error); err != nil {
		t.Fatalf("a journal write failed because the journal was read at the same time: %v", err)
	}
	if err, _ := readErr.Load().(error); err != nil {
		t.Fatalf("reading the journal failed during concurrent writes: %v", err)
	}
	ops, err := s.Operations()
	require.NoError(t, err)
	assert.Len(t, ops, writers)
}

// --- Limits at the boundary ------------------------------------------------

func TestFaults_CountLimitAtTheBoundary(t *testing.T) {
	s := openStore(t, Limits{MaxCount: 3})
	publish(t, s, "a")
	publish(t, s, "b")
	pending, err := s.Reserve("c", 1)
	require.NoError(t, err, "the last free slot must be usable")
	_, err = s.Reserve("d", 1)
	require.ErrorIs(t, err, ErrQuota, "published plus pending already reach the limit")

	pending.Abort()
	res, err := s.Reserve("d", 1)
	require.NoError(t, err, "an aborted reservation gives its slot back")
	writeArtifact(t, res.ArtifactPath(), testMAC)
	_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
	require.NoError(t, err)
	_, err = s.Reserve("e", 1)
	require.ErrorIs(t, err, ErrQuota)

	// Removing one frees exactly one slot.
	require.NoError(t, s.Remove("a"))
	res, err = s.Reserve("e", 1)
	require.NoError(t, err)
	_, err = s.Reserve("f", 1)
	require.ErrorIs(t, err, ErrQuota)
	res.Abort()

	// A quota failure names no reservation: the name stays free.
	_, err = reopen(t, s).Reserve("f", 1)
	require.NoError(t, err)
}

func TestFaults_ByteLimitAtTheBoundary(t *testing.T) {
	// size is what a capture weighs at publish; onDisk is what a published
	// entry occupies afterwards, sidecar included.
	size := artifactSize(t)
	probe := openStore(t, Limits{})
	publish(t, probe, "a")
	_, onDisk, err := probe.Usage()
	require.NoError(t, err)
	require.Greater(t, onDisk, size)

	t.Run("estimate", func(t *testing.T) {
		s := openStore(t, Limits{MaxBytes: onDisk + size})
		publish(t, s, "a")
		_, err := s.Reserve("b", size+1)
		require.ErrorIs(t, err, ErrQuota)
		first, err := s.Reserve("b", size)
		require.NoError(t, err, "an estimate that exactly fills the store must be accepted")
		_, err = s.Reserve("c", 1)
		require.ErrorIs(t, err, ErrQuota, "bytes promised to a running capture are not free")
		first.Abort()
		res, err := s.Reserve("c", 1)
		require.NoError(t, err)
		res.Abort()
	})

	t.Run("captured size exactly at the limit", func(t *testing.T) {
		s := openStore(t, Limits{MaxBytes: onDisk + size})
		publish(t, s, "a")
		res, err := s.Reserve("b", 1)
		require.NoError(t, err)
		writeArtifact(t, res.ArtifactPath(), testMAC)
		_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.NoError(t, err)
	})

	t.Run("captured size one byte over", func(t *testing.T) {
		s := openStore(t, Limits{MaxBytes: onDisk + size - 1})
		publish(t, s, "a")
		res, err := s.Reserve("b", 1)
		require.NoError(t, err)
		writeArtifact(t, res.ArtifactPath(), testMAC)
		_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.ErrorIs(t, err, ErrQuota)
		assert.Equal(t, []string{"a"}, listed(t, s))
		assert.Empty(t, stagingEntries(t, s))
		_, _, _, err = s.Verify("a")
		require.NoError(t, err)
	})

	t.Run("another capture's reservation is honored at publish", func(t *testing.T) {
		s := openStore(t, Limits{MaxBytes: 2 * size})
		other, err := s.Reserve("other", size+1)
		require.NoError(t, err)
		res, err := s.Reserve("mine", 1)
		require.NoError(t, err)
		writeArtifact(t, res.ArtifactPath(), testMAC)
		_, err = res.Publish(Info{VMID: faultVMID, Backend: "test"}, nil)
		require.ErrorIs(t, err, ErrQuota, "publishing took bytes another capture had reserved")
		// The other capture keeps its reservation.
		_, err = s.Reserve("other", 1)
		require.ErrorIs(t, err, ErrExists)
		other.Abort()
	})

	t.Run("usage is measured, not trusted", func(t *testing.T) {
		s := openStore(t, Limits{MaxBytes: 2 * size})
		publish(t, s, "a")
		// A sidecar that understates its size does not create room.
		path := filepath.Join(s.Root(), "a", SidecarName)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var info Info
		require.NoError(t, json.Unmarshal(data, &info))
		info.SizeBytes = 1
		data, err = json.Marshal(info)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		_, used, err := s.Usage()
		require.NoError(t, err)
		assert.Greater(t, used, size)
		_, err = s.Reserve("b", 2*size-used+1)
		require.ErrorIs(t, err, ErrQuota)
		res, err := s.Reserve("b", 2*size-used)
		require.NoError(t, err)
		res.Abort()
	})
}

func TestFaults_ZeroLimitsAreUnbounded(t *testing.T) {
	s := openStore(t, Limits{})
	for i := range 5 {
		publish(t, s, fmt.Sprintf("snap%d", i))
	}
	res, err := s.Reserve("huge", 1<<50)
	require.NoError(t, err)
	res.Abort()
}

// --- Acquire against Remove ------------------------------------------------

func TestFaults_AcquireAndRemoveNeverBothWin(t *testing.T) {
	for range 40 {
		s := openStore(t, Limits{})
		publish(t, s, "snap")
		var wg sync.WaitGroup
		var release func()
		var acquireErr, removeErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; release, acquireErr = s.Acquire("snap") }()
		go func() { defer wg.Done(); <-start; removeErr = s.Remove("snap") }()
		close(start)
		wg.Wait()

		if acquireErr == nil {
			// A restore holds the snapshot: it must still be there, whole.
			require.ErrorIs(t, removeErr, ErrInUse)
			_, _, _, err := s.Verify("snap")
			require.NoError(t, err, "a snapshot was removed while an operation held it")
			release()
			require.NoError(t, s.Remove("snap"))
		} else {
			require.ErrorIs(t, acquireErr, ErrNotFound)
			require.NoError(t, removeErr)
		}
		_, err := s.Get("snap")
		require.ErrorIs(t, err, ErrNotFound)
	}
}

func TestFaults_SnapshotStaysUntilTheLastHolderLetsGo(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	const holders = 8
	releases := make([]func(), holders)
	var wg sync.WaitGroup
	for i := range holders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.Acquire("snap")
			assert.NoError(t, err)
			releases[i] = release
		}()
	}
	wg.Wait()
	for i, release := range releases {
		require.NotNil(t, release)
		require.ErrorIs(t, s.Remove("snap"), ErrInUse, "removed with %d holders left", holders-i)
		release()
		release() // releasing twice must not drop someone else's hold
	}
	require.NoError(t, s.Remove("snap"))

	_, err := s.Acquire("snap")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Acquire("../snap")
	require.ErrorIs(t, err, ErrInvalidName)
}

func TestFaults_HoldDoesNotOutliveTheDaemon(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	_, err := s.Acquire("snap")
	require.NoError(t, err)
	// The daemon died holding the snapshot; only the journal pins a snapshot
	// across a restart.
	require.NoError(t, reopen(t, s).Remove("snap"))
}

func TestFaults_RemoveIsAllOrNothing(t *testing.T) {
	skipIfRoot(t)
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	staging := filepath.Join(s.Root(), stagingDir)
	// The entry cannot be moved into staging.
	require.NoError(t, os.Chmod(staging, 0o500))
	t.Cleanup(func() { _ = os.Chmod(staging, 0o700) })
	require.Error(t, s.Remove("snap"))
	_, _, _, err := s.Verify("snap")
	require.NoError(t, err, "a removal that failed left the snapshot damaged")

	require.NoError(t, os.Chmod(staging, 0o700))
	require.NoError(t, s.Remove("snap"))
	require.ErrorIs(t, s.Remove("snap"), ErrNotFound)
	assert.Empty(t, stagingEntries(t, s))
}

func TestFaults_NamesNeverEscapeTheStore(t *testing.T) {
	s := openStore(t, Limits{})
	outside := filepath.Join(filepath.Dir(s.Root()), "victim")
	require.NoError(t, os.Mkdir(outside, 0o700))
	for _, name := range []string{"../victim", "..", ".", stagingDir, operationsDir, "a/b", "", "UPPER", "sp ace", "nul\x00"} {
		_, err := s.Reserve(name, 1)
		require.ErrorIs(t, err, ErrInvalidName, "Reserve(%q)", name)
		_, err = s.Get(name)
		require.ErrorIs(t, err, ErrInvalidName, "Get(%q)", name)
		_, _, _, err = s.Verify(name)
		require.ErrorIs(t, err, ErrInvalidName, "Verify(%q)", name)
		require.ErrorIs(t, s.Remove(name), ErrInvalidName, "Remove(%q)", name)
	}
	assert.DirExists(t, outside)
	assert.DirExists(t, filepath.Join(s.Root(), stagingDir))
	assert.DirExists(t, filepath.Join(s.Root(), operationsDir))
	for _, id := range []string{"", "../x", "0123456789AB", "0123456789abc", "0123456789a"} {
		require.Error(t, s.WriteOperation(Operation{Kind: OperationCreate, VMID: id}), "WriteOperation(%q)", id)
		require.Error(t, s.RemoveOperation(id), "RemoveOperation(%q)", id)
	}
}

// --- Open ------------------------------------------------------------------

func TestFaults_OpenRefusesALayoutItCannotTrust(t *testing.T) {
	for _, sub := range []string{stagingDir, operationsDir} {
		t.Run(sub+" is a link", func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "snapshots")
			elsewhere := filepath.Join(base, "elsewhere")
			require.NoError(t, os.Mkdir(root, 0o700))
			require.NoError(t, os.Mkdir(elsewhere, 0o700))
			require.NoError(t, os.Symlink(elsewhere, filepath.Join(root, sub)))
			_, err := Open(root, Limits{})
			require.ErrorIs(t, err, ErrIntegrity)
		})
		t.Run(sub+" is a file", func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "snapshots")
			require.NoError(t, os.Mkdir(root, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, sub), []byte("x"), 0o600))
			_, err := Open(root, Limits{})
			require.Error(t, err)
		})
	}
	t.Run("root is a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "snapshots")
		require.NoError(t, os.WriteFile(root, []byte("x"), 0o600))
		_, err := Open(root, Limits{})
		require.Error(t, err)
	})
}

func TestFaults_OpenRepairsLoosePermissions(t *testing.T) {
	s := openStore(t, Limits{})
	publish(t, s, "snap")
	// A restore from backup or a careless chmod opened the layout up.
	for _, dir := range []string{s.Root(), filepath.Join(s.Root(), stagingDir), filepath.Join(s.Root(), operationsDir)} {
		require.NoError(t, os.Chmod(dir, 0o755))
	}
	fresh := reopen(t, s)
	for _, dir := range []string{fresh.Root(), filepath.Join(fresh.Root(), stagingDir), filepath.Join(fresh.Root(), operationsDir)} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), dir)
	}
	_, _, _, err := fresh.Verify("snap")
	require.NoError(t, err)
}
