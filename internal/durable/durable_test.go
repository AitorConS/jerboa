package durable_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	oldContent = `{"generation":"old","padding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	newContent = `{"generation":"new","padding":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func temps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if durable.IsTemp(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestWriteFile_CreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	require.NoError(t, durable.WriteFile(path, []byte(oldContent), 0o600))
	assert.Equal(t, oldContent, read(t, path))

	require.NoError(t, durable.WriteFile(path, []byte(newContent), 0o600))
	assert.Equal(t, newContent, read(t, path))
	assert.Empty(t, temps(t, filepath.Dir(path)), "a finished write leaves no temporary file")
}

func TestWriteFile_ShorterContentLeavesNoTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, durable.WriteFile(path, []byte(newContent), 0o600))
	require.NoError(t, durable.WriteFile(path, []byte("{}"), 0o600))
	assert.Equal(t, "{}", read(t, path))
}

func TestWriteFile_EmptyContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, durable.WriteFile(path, nil, 0o600))
	assert.Empty(t, read(t, path))
}

func TestWriteFile_AppliesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	for _, perm := range []os.FileMode{0o600, 0o644, 0o400} {
		path := filepath.Join(dir, fmt.Sprintf("f-%o", perm))
		require.NoError(t, durable.WriteFile(path, []byte("x"), perm))
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, perm, info.Mode().Perm())
	}
}

func TestWriteFile_TightensModeOfExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
	require.NoError(t, durable.WriteFile(path, []byte("token"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a secret rewritten over a world-readable file must not stay readable")
}

func TestWriteFile_RelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, durable.WriteFile("state.json", []byte(oldContent), 0o600))
	assert.Equal(t, oldContent, read(t, "state.json"))
}

func TestWriteFile_MissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "state.json")
	err := durable.WriteFile(path, []byte(oldContent), 0o600)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Contains(t, err.Error(), "create")
}

func TestWriteFile_ReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	require.NoError(t, durable.WriteFile(path, []byte(oldContent), 0o600))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := durable.WriteFile(path, []byte(newContent), 0o600)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.Equal(t, oldContent, read(t, path), "a write that cannot start must not touch the file")
}

func TestWriteFile_TargetIsDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	require.NoError(t, os.Mkdir(path, 0o700))
	require.Error(t, durable.WriteFile(path, []byte(newContent), 0o600))
	assert.Empty(t, temps(t, dir), "a failed rename removes its temporary file")
}

// A write is safe against a power cut only if the content reaches stable
// storage before the rename exposes it, and the rename is flushed after.
func TestWriteFile_StepOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var got []durable.Step
	restore := durable.SetFault(func(step durable.Step, p string) error {
		assert.Equal(t, path, p)
		got = append(got, step)
		return nil
	})
	require.NoError(t, durable.WriteFile(path, []byte(oldContent), 0o600))
	restore()
	assert.Equal(t, durable.Steps, got)
}

func TestWriteFileNoSync_SkipsFlushesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var got []durable.Step
	restore := durable.SetFault(func(step durable.Step, _ string) error {
		got = append(got, step)
		return nil
	})
	require.NoError(t, durable.WriteFileNoSync(path, []byte(oldContent), 0o600))
	restore()
	assert.Equal(t, []durable.Step{durable.StepCreate, durable.StepWrite, durable.StepClose, durable.StepRename}, got)
	assert.Equal(t, oldContent, read(t, path))
}

// The invariant everything else rests on: whichever step fails, and however
// it fails, the file holds all of the old content or all of the new.
func TestWriteFile_EveryFailureLeavesOldOrNew(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			require.NoError(t, durable.WriteFile(path, []byte(oldContent), 0o600))

			inj := durabletest.Inject(t, f, 0, nil)
			err := durable.WriteFile(path, []byte(newContent), 0o600)
			require.True(t, inj.Fired())

			got := read(t, path)
			switch {
			case f.Step == durable.StepSyncDir && !f.Crash():
				require.NoError(t, err, "the rename already happened: a directory that cannot be flushed does not fail the write")
				assert.Equal(t, newContent, got)
			case f.Step == durable.StepSyncDir:
				require.ErrorIs(t, err, durable.ErrCrash)
				assert.Equal(t, newContent, got)
			default:
				require.ErrorIs(t, err, f.Err)
				assert.Equal(t, oldContent, got, "a write that failed before the rename must leave the previous file")
			}

			if !f.Crash() {
				assert.Empty(t, temps(t, dir), "a failed write cleans its temporary file up")
			}
			// Whatever is left behind, the next write and a sweep recover.
			require.NoError(t, durable.WriteFile(path, []byte(newContent), 0o600))
			assert.Equal(t, newContent, read(t, path))
			durable.RemoveTemps(dir)
			assert.Empty(t, temps(t, dir))
		})
	}
}

func TestWriteFile_EveryFailureOnFirstWriteLeavesNothingOrAll(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			durabletest.Inject(t, f, 0, nil)
			_ = durable.WriteFile(path, []byte(newContent), 0o600)

			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return
			}
			require.NoError(t, err)
			assert.Equal(t, newContent, string(data), "a file that appears must be complete")
		})
	}
}

func TestWriteFile_ShortWriteNeverReachesTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	require.NoError(t, durable.WriteFile(path, []byte(oldContent), 0o600))

	durabletest.Inject(t, durabletest.Failure{Step: durable.StepWrite, Err: durable.ErrCrash}, 0, nil)
	require.ErrorIs(t, durable.WriteFile(path, []byte(newContent), 0o600), durable.ErrCrash)

	left := temps(t, dir)
	require.Len(t, left, 1, "the crash leaves its half-written temporary file")
	partial := read(t, filepath.Join(dir, left[0]))
	assert.Equal(t, newContent[:len(newContent)/2], partial)
	assert.Equal(t, oldContent, read(t, path))
}

func TestInject_NthWriteAndMatch(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	onlyB := func(p string) bool { return strings.HasSuffix(p, "b.json") }
	inj := durabletest.Inject(t, durabletest.Failure{Step: durable.StepRename, Err: syscall.ENOSPC}, 1, onlyB)

	require.NoError(t, durable.WriteFile(a, []byte("1"), 0o600), "a.json is not matched")
	require.NoError(t, durable.WriteFile(b, []byte("1"), 0o600), "the first b.json write is not the nth")
	require.ErrorIs(t, durable.WriteFile(b, []byte("2"), 0o600), syscall.ENOSPC)
	require.NoError(t, durable.WriteFile(b, []byte("3"), 0o600), "a failure fires once")
	assert.True(t, inj.Fired())
	assert.Equal(t, 3, inj.Writes())
	assert.Equal(t, "3", read(t, b))
}

func TestCount(t *testing.T) {
	dir := t.TempDir()
	n := durabletest.Count(t, nil, func() {
		for i := range 4 {
			require.NoError(t, durable.WriteFile(filepath.Join(dir, fmt.Sprint(i)), []byte("x"), 0o600))
		}
	})
	assert.Equal(t, 4, n)
}

func TestSetFault_RestoreAndNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	boom := errors.New("boom")
	restoreOuter := durable.SetFault(func(durable.Step, string) error { return boom })
	require.ErrorIs(t, durable.WriteFile(path, []byte("x"), 0o600), boom)

	restoreInner := durable.SetFault(nil)
	require.NoError(t, durable.WriteFile(path, []byte("x"), 0o600), "a nil fault clears the one installed")
	restoreInner()
	require.ErrorIs(t, durable.WriteFile(path, []byte("x"), 0o600), boom, "restoring puts the outer fault back")
	restoreOuter()
	require.NoError(t, durable.WriteFile(path, []byte("x"), 0o600))
}

func TestStepString(t *testing.T) {
	want := []string{"create", "write", "sync", "close", "rename", "syncdir"}
	for i, s := range durable.Steps {
		assert.Equal(t, want[i], s.String())
	}
	assert.Equal(t, "step(42)", durable.Step(42).String())
}

func TestIsTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refs.json")
	durabletest.Inject(t, durabletest.Failure{Step: durable.StepRename, Err: durable.ErrCrash}, 0, nil)
	require.Error(t, durable.WriteFile(path, []byte("x"), 0o600))
	left := temps(t, filepath.Dir(path))
	require.Len(t, left, 1)
	assert.True(t, strings.HasPrefix(left[0], ".refs.json.tmp-"))

	for _, name := range []string{"refs.json", "state.json.tmp", ".hidden", "tmp-1", "a.tmp-1"} {
		assert.False(t, durable.IsTemp(name), name)
	}
}

func TestRemoveTemps(t *testing.T) {
	dir := t.TempDir()
	keep := []string{"refs.json", ".verified", "state.json.tmp"}
	for _, name := range keep {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".refs.json.tmp-1"), []byte("12345"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".meta.json.tmp-2"), []byte("123"), 0o600))
	// A directory is never a temporary file of ours, whatever it is called.
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".dir.tmp-3"), 0o700))

	count, bytes := durable.RemoveTemps(dir)
	assert.Equal(t, 2, count)
	assert.Equal(t, int64(8), bytes)
	for _, name := range append(keep, ".dir.tmp-3") {
		_, err := os.Stat(filepath.Join(dir, name))
		assert.NoError(t, err, name)
	}

	count, bytes = durable.RemoveTemps(filepath.Join(dir, "absent"))
	assert.Zero(t, count)
	assert.Zero(t, bytes)
}

func TestSyncDir_MissingDirectoryIsHarmless(t *testing.T) {
	assert.NotPanics(t, func() { durable.SyncDir(filepath.Join(t.TempDir(), "absent")) })
}

// Many writers racing on one file: the survivor is one writer's content,
// whole, and no temporary file is left.
func TestWriteFile_ConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	const writers = 16
	valid := make(map[string]bool, writers)
	var wg sync.WaitGroup
	for i := range writers {
		content := strings.Repeat(fmt.Sprintf("%02d", i), 4096)
		valid[content] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				assert.NoError(t, durable.WriteFileNoSync(path, []byte(content), 0o600))
			}
		}()
	}
	wg.Wait()
	assert.True(t, valid[read(t, path)], "the file is not any writer's complete content")
	assert.Empty(t, temps(t, dir))
}

// Readers never observe a partial file while it is being replaced.
func TestWriteFile_ReadersSeeWholeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to replace a file another handle has open")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	a, b := strings.Repeat("a", 1<<16), strings.Repeat("b", 1<<17)
	require.NoError(t, durable.WriteFile(path, []byte(a), 0o600))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			content := a
			if i%2 == 0 {
				content = b
			}
			assert.NoError(t, durable.WriteFileNoSync(path, []byte(content), 0o600))
		}
	}()
	for range 500 {
		got := read(t, path)
		if got != a && got != b {
			t.Fatalf("read a partial file of %d bytes", len(got))
		}
	}
	close(stop)
	wg.Wait()
}

// The real thing rather than a simulation: a process is killed outright while
// it rewrites a file in a loop, again and again, and the file must always be
// one of the two complete payloads.
func TestWriteFile_SurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills processes")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	a, b := strings.Repeat("a", 1<<18), strings.Repeat("b", 1<<19)
	require.NoError(t, durable.WriteFile(path, []byte(a), 0o600))

	for round := range 25 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWriteForever$") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), "DURABLE_HELPER_PATH="+path)
		require.NoError(t, cmd.Start())
		time.Sleep(time.Duration(5+round*3) * time.Millisecond)
		require.NoError(t, cmd.Process.Kill())
		_ = cmd.Wait()

		got := read(t, path)
		if got != a && got != b {
			t.Fatalf("round %d: the kill left a file of %d bytes that is neither payload", round, len(got))
		}
	}
	// Killed writers leave temporary files; a sweep reclaims every one.
	durable.RemoveTemps(dir)
	assert.Empty(t, temps(t, dir))
}

func TestHelperWriteForever(t *testing.T) {
	path := os.Getenv("DURABLE_HELPER_PATH")
	if path == "" {
		t.Skip("helper process for TestWriteFile_SurvivesKill")
	}
	a, b := []byte(strings.Repeat("a", 1<<18)), []byte(strings.Repeat("b", 1<<19))
	for i := 0; ; i++ {
		content := a
		if i%2 == 0 {
			content = b
		}
		if err := durable.WriteFileNoSync(path, content, 0o600); err != nil {
			os.Exit(3)
		}
	}
}
