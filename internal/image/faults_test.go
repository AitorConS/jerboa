package image

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about what the store does when things go wrong: the disk
// fills up, the device errors, the daemon is killed or the machine loses
// power in the middle of an operation, or a file is found damaged afterwards.
// The rule they hold the store to: a failure may lose the operation that was
// in flight, never an image that was already stored, and the store always
// opens and lists again.

func diskWith(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func digestOf(content string) string { return DigestSHA256([]byte(content)) }

// requireIntact reopens the store the way a restarted daemon does and checks
// that it is whole: it lists, and every image it lists resolves to a disk
// whose bytes hash to the digest the manifest claims.
func requireIntact(t *testing.T, root string) (*Store, map[string]string) {
	t.Helper()
	s, err := NewStore(root)
	require.NoError(t, err)
	list, err := s.List()
	require.NoError(t, err, "a store must list after any failure")
	got := make(map[string]string, len(list))
	for _, m := range list {
		ref := m.Name + ":" + m.Tag
		gm, disk, err := s.Get(ref)
		require.NoError(t, err, "listed image %s does not resolve", ref)
		sum, err := fileSHA256(disk)
		require.NoError(t, err)
		require.Equal(t, gm.DiskDigest, sum, "image %s serves a disk that is not the one it names", ref)
		got[ref] = gm.DiskDigest
	}
	return s, got
}

func noPartials(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		require.NoError(t, err)
		assert.False(t, strings.HasSuffix(path, partialSuffix), "half-copied disk left behind: %s", path)
		return nil
	})
}

// seedStore returns a store holding base:latest.
func seedStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "images")
	s, err := NewStore(root)
	require.NoError(t, err)
	require.NoError(t, s.Put("base", "latest", validManifest(), diskWith(t, "base disk")))
	return s, root
}

// eachFailure runs body once for every durable write op performs, failing
// that write in every way a write can fail.
func eachFailure(t *testing.T, op func(s *Store) error, body func(t *testing.T, root string, f durabletest.Failure, opErr error)) {
	t.Helper()
	s, _ := seedStore(t)
	writes := durabletest.Count(t, nil, func() { require.NoError(t, op(s)) })
	require.Positive(t, writes, "the operation persists nothing")

	for nth := range writes {
		for _, f := range durabletest.Failures() {
			t.Run(fmt.Sprintf("write%d/%s", nth, f.Name), func(t *testing.T) {
				s, root := seedStore(t)
				inj := durabletest.Inject(t, f, nth, nil)
				err := op(s)
				require.True(t, inj.Fired(), "the failure was never reached")
				durable.SetFault(nil)
				body(t, root, f, err)
			})
		}
	}
}

func TestFaults_PutNewImage(t *testing.T) {
	disk := "app disk v1"
	put := func(s *Store) error { return s.Put("app", "v1", validManifest(), diskWith(t, disk)) }

	eachFailure(t, put, func(t *testing.T, root string, f durabletest.Failure, opErr error) {
		s, got := requireIntact(t, root)
		assert.Equal(t, digestOf("base disk"), got["base:latest"], "an image stored before the failure was lost")
		if d, ok := got["app:v1"]; ok {
			assert.Equal(t, digestOf(disk), d)
		} else if opErr == nil {
			t.Fatal("Put reported success but the image is not in the store")
		}

		// The operation can simply be tried again, and a restart's prune
		// keeps everything that is named.
		require.NoError(t, put(s))
		_, err := s.PruneUnreferenced(nil)
		require.NoError(t, err)
		_, got = requireIntact(t, root)
		assert.Equal(t, digestOf(disk), got["app:v1"])
		assert.Equal(t, digestOf("base disk"), got["base:latest"])
		if !f.Crash() {
			noPartials(t, root)
		}
	})
}

// Rebuilding a tag is the case that used to lose images: the name must point
// at the old disk or at the new one, whole, at every instant.
func TestFaults_PutMovesExistingTag(t *testing.T) {
	rebuilt := "base disk, rebuilt"
	put := func(s *Store) error { return s.Put("base", "latest", validManifest(), diskWith(t, rebuilt)) }

	eachFailure(t, put, func(t *testing.T, root string, _ durabletest.Failure, opErr error) {
		_, got := requireIntact(t, root)
		d, ok := got["base:latest"]
		require.True(t, ok, "the tag vanished while it was being moved")
		if opErr == nil {
			assert.Equal(t, digestOf(rebuilt), d)
		} else {
			assert.Contains(t, []string{digestOf("base disk"), digestOf(rebuilt)}, d)
		}
	})
}

func TestFaults_PutMove(t *testing.T) {
	content := "moved disk"
	put := func(s *Store) error {
		tmp, err := s.TempFile("pull-*.img")
		if err != nil {
			return err
		}
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return err
		}
		_, err = s.PutMove("moved", "latest", validManifest(), tmp)
		return err
	}
	eachFailure(t, put, func(t *testing.T, root string, _ durabletest.Failure, opErr error) {
		s, got := requireIntact(t, root)
		assert.Equal(t, digestOf("base disk"), got["base:latest"])
		if opErr == nil {
			assert.Equal(t, digestOf(content), got["moved:latest"])
		}
		require.NoError(t, put(s))
		_, got = requireIntact(t, root)
		assert.Equal(t, digestOf(content), got["moved:latest"])
	})
}

func TestFaults_Tag(t *testing.T) {
	tag := func(s *Store) error {
		_, err := s.Tag("base:latest", "base", "stable")
		return err
	}
	eachFailure(t, tag, func(t *testing.T, root string, _ durabletest.Failure, opErr error) {
		s, got := requireIntact(t, root)
		assert.Equal(t, digestOf("base disk"), got["base:latest"], "tagging must never cost the source its name")
		if d, ok := got["base:stable"]; ok {
			assert.Equal(t, digestOf("base disk"), d)
		} else if opErr == nil {
			t.Fatal("Tag reported success but the tag is not in the store")
		}
		require.NoError(t, tag(s))
		_, got = requireIntact(t, root)
		assert.Equal(t, digestOf("base disk"), got["base:stable"])
	})
}

func TestFaults_RemoveOneOfTwoTags(t *testing.T) {
	setup := func(s *Store) error {
		_, err := s.Tag("base:latest", "base", "stable")
		return err
	}
	s, _ := seedStore(t)
	require.NoError(t, setup(s))
	writes := durabletest.Count(t, nil, func() { require.NoError(t, s.Remove("base:stable")) })
	require.Positive(t, writes)

	for nth := range writes {
		for _, f := range durabletest.Failures() {
			t.Run(fmt.Sprintf("write%d/%s", nth, f.Name), func(t *testing.T) {
				s, root := seedStore(t)
				require.NoError(t, setup(s))
				inj := durabletest.Inject(t, f, nth, nil)
				err := s.Remove("base:stable")
				require.True(t, inj.Fired())
				durable.SetFault(nil)

				s, got := requireIntact(t, root)
				assert.Equal(t, digestOf("base disk"), got["base:latest"], "removing one tag damaged the other")
				if err == nil {
					assert.NotContains(t, got, "base:stable")
				}
				// A removal that failed can be repeated until it goes through.
				if _, still := got["base:stable"]; still {
					require.NoError(t, s.Remove("base:stable"))
				}
				_, got = requireIntact(t, root)
				assert.NotContains(t, got, "base:stable")
				assert.Contains(t, got, "base:latest")
			})
		}
	}
}

func TestFaults_RemoveLastTag(t *testing.T) {
	remove := func(s *Store) error { return s.Remove("base:latest") }
	eachFailure(t, remove, func(t *testing.T, root string, _ durabletest.Failure, opErr error) {
		s, got := requireIntact(t, root)
		if opErr == nil {
			assert.Empty(t, got)
			return
		}
		// Either the removal did not happen, or it did; never half.
		if _, still := got["base:latest"]; still {
			require.NoError(t, s.Remove("base:latest"))
		}
		_, err := s.PruneUnreferenced(nil)
		require.NoError(t, err)
		_, got = requireIntact(t, root)
		assert.Empty(t, got)
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		for _, e := range entries {
			assert.False(t, e.IsDir(), "an image directory outlived its last tag: %s", e.Name())
		}
	})
}

func TestFaults_PruneWithDanglingRef(t *testing.T) {
	// A ref whose image was deleted behind the store makes prune rewrite the
	// index; that rewrite failing must not cost the images that remain.
	setup := func(t *testing.T) (*Store, string) {
		s, root := seedStore(t)
		require.NoError(t, s.Put("gone", "latest", validManifest(), diskWith(t, "gone disk")))
		require.NoError(t, os.RemoveAll(filepath.Join(root, stripPrefix(digestOf("gone disk")))))
		return s, root
	}
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, root := setup(t)
			inj := durabletest.Inject(t, f, 0, nil)
			_, _ = s.PruneUnreferenced(nil)
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			s, got := requireIntact(t, root)
			assert.Equal(t, map[string]string{"base:latest": digestOf("base disk")}, got)
			_, err := s.PruneUnreferenced(nil)
			require.NoError(t, err)
			refs, err := s.readRefs()
			require.NoError(t, err)
			assert.NotContains(t, refs, "gone:latest")
		})
	}
}

// failingCopy makes the disk copy stop after n bytes with err, the way a
// volume that fills up half-way through a pull or a build does.
func failingCopy(t *testing.T, n int64, err error) {
	t.Helper()
	orig := copyDisk
	copyDisk = func(dst io.Writer, src io.Reader) (int64, error) {
		written, _ := io.CopyN(dst, src, n)
		return written, err
	}
	t.Cleanup(func() { copyDisk = orig })
}

func TestFaults_DiskFullWhileCopyingDisk(t *testing.T) {
	for _, cause := range []error{syscall.ENOSPC, syscall.EIO, syscall.EDQUOT} {
		t.Run(cause.Error(), func(t *testing.T) {
			s, root := seedStore(t)
			content := strings.Repeat("large disk ", 4096)
			failingCopy(t, 1000, cause)

			err := s.Put("big", "latest", validManifest(), diskWith(t, content))
			require.ErrorIs(t, err, cause)
			noPartials(t, root)
			_, statErr := os.Stat(filepath.Join(root, stripPrefix(digestOf(content)), "disk.img"))
			assert.True(t, os.IsNotExist(statErr), "a truncated disk was left under the image's name")
			_, got := requireIntact(t, root)
			assert.Equal(t, map[string]string{"base:latest": digestOf("base disk")}, got)

			// Once there is room again the same Put goes through.
			copyDisk = io.Copy
			require.NoError(t, s.Put("big", "latest", validManifest(), diskWith(t, content)))
			_, got = requireIntact(t, root)
			assert.Equal(t, digestOf(content), got["big:latest"])
		})
	}
}

// A daemon from before atomic copies could leave a truncated disk.img in the
// right directory. Storing that image again must replace it, not trust it.
func TestFaults_TruncatedDiskFromEarlierCrashIsReplaced(t *testing.T) {
	s, root := seedStore(t)
	content := strings.Repeat("the whole disk ", 512)
	imgDir := filepath.Join(root, stripPrefix(digestOf(content)))
	require.NoError(t, os.MkdirAll(imgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(imgDir, "disk.img"), []byte(content[:100]), 0o444))

	require.NoError(t, s.Put("app", "latest", validManifest(), diskWith(t, content)))
	_, got := requireIntact(t, root)
	assert.Equal(t, digestOf(content), got["app:latest"])
}

func TestFaults_TruncatedDiskFromEarlierCrashIsReplaced_Move(t *testing.T) {
	s, root := seedStore(t)
	content := strings.Repeat("the whole disk ", 512)
	imgDir := filepath.Join(root, stripPrefix(digestOf(content)))
	require.NoError(t, os.MkdirAll(imgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(imgDir, "disk.img"), nil, 0o444))

	tmp, err := s.TempFile("pull-*.img")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tmp, []byte(content), 0o644))
	_, err = s.PutMove("app", "latest", validManifest(), tmp)
	require.NoError(t, err)
	_, got := requireIntact(t, root)
	assert.Equal(t, digestOf(content), got["app:latest"])
}

func TestFaults_StalePartialFromEarlierCrashDoesNotBlockPut(t *testing.T) {
	s, root := seedStore(t)
	content := "disk after a crash"
	imgDir := filepath.Join(root, stripPrefix(digestOf(content)))
	require.NoError(t, os.MkdirAll(imgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(imgDir, "disk.img"+partialSuffix), []byte("half"), 0o400))

	require.NoError(t, s.Put("app", "latest", validManifest(), diskWith(t, content)))
	noPartials(t, root)
	_, got := requireIntact(t, root)
	assert.Equal(t, digestOf(content), got["app:latest"])
}

func TestFaults_UnreadableRefsIsRebuilt(t *testing.T) {
	good := func(t *testing.T, root string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "refs.json"))
		require.NoError(t, err)
		return data
	}
	cases := map[string]func(valid []byte) []byte{
		"empty file":       func([]byte) []byte { return nil },
		"truncated":        func(v []byte) []byte { return v[:len(v)/2] },
		"one byte":         func([]byte) []byte { return []byte("{") },
		"null":             func([]byte) []byte { return []byte("null") },
		"wrong type":       func([]byte) []byte { return []byte(`["base:latest"]`) },
		"zero filled":      func(v []byte) []byte { return make([]byte, len(v)) },
		"binary garbage":   func([]byte) []byte { return []byte{0xff, 0xfe, 0x00, 0x7b, 0x81} },
		"trailing garbage": func(v []byte) []byte { return append(append([]byte{}, v...), "}}}"...) },
		"html error page":  func([]byte) []byte { return []byte("<html>502 Bad Gateway</html>") },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			s, root := seedStore(t)
			require.NoError(t, s.Put("app", "v1", validManifest(), diskWith(t, "app disk")))
			_, err := s.Tag("app:v1", "app", "latest")
			require.NoError(t, err)
			want := map[string]string{
				"base:latest": digestOf("base disk"),
				"app:v1":      digestOf("app disk"),
				"app:latest":  digestOf("app disk"),
			}
			damaged := damage(good(t, root))
			require.NoError(t, os.WriteFile(filepath.Join(root, "refs.json"), damaged, 0o644))

			s, got := requireIntact(t, root)
			assert.Equal(t, want, got, "the rebuilt index does not name every stored image")
			if len(damaged) > 0 {
				saved, err := os.ReadFile(filepath.Join(root, "refs.json.corrupt"))
				require.NoError(t, err)
				assert.Equal(t, damaged, saved, "the unreadable index is kept for inspection")
			}

			// The store is fully usable again, not just readable.
			require.NoError(t, s.Put("new", "latest", validManifest(), diskWith(t, "new disk")))
			require.NoError(t, s.Remove("app:v1"))
			_, got = requireIntact(t, root)
			assert.Contains(t, got, "new:latest")
			assert.Contains(t, got, "app:latest")
			assert.NotContains(t, got, "app:v1")
		})
	}
}

func TestFaults_RebuildSendsMovedTagToItsNewDisk(t *testing.T) {
	s, root := seedStore(t)
	// Another tag keeps the old disk alive, so both directories exist.
	_, err := s.Tag("base:latest", "base", "old")
	require.NoError(t, err)
	require.NoError(t, s.Put("base", "latest", validManifest(), diskWith(t, "base disk, rebuilt")))

	require.NoError(t, os.WriteFile(filepath.Join(root, "refs.json"), []byte("garbage"), 0o644))
	_, got := requireIntact(t, root)
	assert.Equal(t, digestOf("base disk, rebuilt"), got["base:latest"])
	assert.Equal(t, digestOf("base disk"), got["base:old"])
}

func TestFaults_RebuildNamesImagesStoredBeforeTagManifests(t *testing.T) {
	s, root := seedStore(t)
	sha := stripPrefix(digestOf("base disk"))
	require.NoError(t, os.RemoveAll(filepath.Join(root, sha, "tags")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "refs.json"), nil, 0o644))
	_ = s

	_, got := requireIntact(t, root)
	assert.Equal(t, map[string]string{"base:latest": digestOf("base disk")}, got)
}

func TestFaults_RebuildIgnoresWhatIsNotAnImage(t *testing.T) {
	s, root := seedStore(t)
	_ = s
	// A directory with a disk but no manifest, one with a manifest that lies
	// about its digest, a stray file and a non-digest directory.
	orphan := filepath.Join(root, strings.Repeat("a", 64))
	require.NoError(t, os.MkdirAll(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "disk.img"), []byte("x"), 0o444))

	liar := filepath.Join(root, strings.Repeat("b", 64))
	require.NoError(t, os.MkdirAll(liar, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(liar, "disk.img"), []byte("x"), 0o444))
	m := validManifest()
	m.Name, m.DiskDigest = "liar", digestOf("something else")
	data, err := Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(liar, "manifest.json"), data, 0o644))

	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-digest", "tags"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "refs.json"), []byte("{"), 0o644))

	_, got := requireIntact(t, root)
	assert.Equal(t, map[string]string{"base:latest": digestOf("base disk")}, got)
}

func TestFaults_UnreadableManifestHidesOnlyItsImage(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":     nil,
		"truncated": []byte(`{"schemaVersion":1,"name":"ap`),
		"garbage":   {0x00, 0x01, 0x02},
	} {
		t.Run(name, func(t *testing.T) {
			s, root := seedStore(t)
			require.NoError(t, s.Put("app", "v1", validManifest(), diskWith(t, "app disk")))
			imgDir := filepath.Join(root, stripPrefix(digestOf("app disk")))
			require.NoError(t, os.WriteFile(tagManifestPath(imgDir, "app:v1"), content, 0o644))

			_, got := requireIntact(t, root)
			assert.Equal(t, map[string]string{"base:latest": digestOf("base disk")}, got)

			_, _, err := s.Get("app:v1")
			require.Error(t, err, "a broken manifest must not resolve")
			// Storing the image again repairs it.
			require.NoError(t, s.Put("app", "v1", validManifest(), diskWith(t, "app disk")))
			_, got = requireIntact(t, root)
			assert.Contains(t, got, "app:v1")
		})
	}
}

func TestFaults_UnreadableVerifiedRecordIsRecomputed(t *testing.T) {
	s, root := seedStore(t)
	imgDir := filepath.Join(root, stripPrefix(digestOf("base disk")))
	for _, content := range [][]byte{nil, []byte("{"), []byte(`{"digest":"sha256:0000"}`)} {
		require.NoError(t, os.WriteFile(filepath.Join(imgDir, "verified.json"), content, 0o644))
		_, _, err := s.Get("base:latest")
		require.NoError(t, err, "a damaged verification record must only cost a re-hash")
		rec, ok := readVerified(imgDir)
		require.True(t, ok)
		assert.Equal(t, digestOf("base disk"), rec.Digest)
	}
}

// Bit rot or tampering: a stored disk whose bytes changed is never handed out,
// whatever the verification record says.
func TestFaults_ModifiedDiskIsRefused(t *testing.T) {
	changes := map[string]func(t *testing.T, disk string){
		"truncated": func(t *testing.T, disk string) { require.NoError(t, os.Truncate(disk, 3)) },
		"emptied":   func(t *testing.T, disk string) { require.NoError(t, os.Truncate(disk, 0)) },
		"same size, one byte flipped": func(t *testing.T, disk string) {
			data, err := os.ReadFile(disk)
			require.NoError(t, err)
			data[0] ^= 0xff
			require.NoError(t, os.WriteFile(disk, data, 0o644))
		},
		"appended": func(t *testing.T, disk string) {
			f, err := os.OpenFile(disk, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteString("extra")
			require.NoError(t, err)
			require.NoError(t, f.Close())
		},
		"replaced by another file": func(t *testing.T, disk string) {
			require.NoError(t, os.Remove(disk))
			require.NoError(t, os.WriteFile(disk, []byte("base disk!"), 0o444))
		},
		"deleted": func(t *testing.T, disk string) { require.NoError(t, os.Remove(disk)) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, _ := seedStore(t)
			_, disk, err := s.Get("base:latest")
			require.NoError(t, err)
			require.NoError(t, os.Chmod(disk, 0o644))
			// Keep the change from landing in the same timestamp tick as the
			// verified copy, as it would not on a real machine either.
			time.Sleep(15 * time.Millisecond)
			change(t, disk)

			_, _, err = s.Get("base:latest")
			require.Error(t, err)
			assert.False(t, s.Has(digestOf("base disk")))
			assert.False(t, s.Trusted(disk, digestOf("base disk")))

			// And the image can be stored again over the damaged copy.
			require.NoError(t, s.Put("base", "latest", validManifest(), diskWith(t, "base disk")))
			_, _, err = s.Get("base:latest")
			require.NoError(t, err)
		})
	}
}

func TestFaults_ReadOnlyStore(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	s, root := seedStore(t)
	require.NoError(t, os.Chmod(root, 0o500))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	require.Error(t, s.Put("app", "latest", validManifest(), diskWith(t, "app disk")))
	_, err := s.Tag("base:latest", "base", "stable")
	require.Error(t, err)
	require.Error(t, s.Remove("base:latest"))

	// Reading still works, and nothing was half-applied.
	require.NoError(t, os.Chmod(root, 0o755))
	_, got := requireIntact(t, root)
	assert.Equal(t, map[string]string{"base:latest": digestOf("base disk")}, got)
}

func TestFaults_MissingSourceDisk(t *testing.T) {
	s, root := seedStore(t)
	err := s.Put("app", "latest", validManifest(), filepath.Join(t.TempDir(), "absent.img"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist))
	_, got := requireIntact(t, root)
	assert.Len(t, got, 1)
}

// The real thing: a process that builds, tags and removes images in a loop is
// killed outright, over and over. After each kill the store must open, list,
// and serve only disks that match their digests.
func TestFaults_StoreSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills processes")
	}
	root := filepath.Join(t.TempDir(), "images")
	s, err := NewStore(root)
	require.NoError(t, err)
	require.NoError(t, s.Put("base", "latest", validManifest(), diskWith(t, "base disk")))

	for round := range 20 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChurnStore$") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), "IMAGE_HELPER_ROOT="+root)
		require.NoError(t, cmd.Start())
		time.Sleep(time.Duration(15+round*7) * time.Millisecond)
		require.NoError(t, cmd.Process.Kill())
		_ = cmd.Wait()

		s, got := requireIntact(t, root)
		assert.Equal(t, digestOf("base disk"), got["base:latest"], "round %d: the image that was never touched is gone", round)
		_, err := s.PruneUnreferenced(nil)
		require.NoError(t, err, "round %d", round)
		requireIntact(t, root)
	}
}

func TestHelperChurnStore(t *testing.T) {
	root := os.Getenv("IMAGE_HELPER_ROOT")
	if root == "" {
		t.Skip("helper process for TestFaults_StoreSurvivesKill")
	}
	s, err := NewStore(root)
	if err != nil {
		os.Exit(3)
	}
	scratch := t.TempDir()
	for i := 0; ; i++ {
		disk := filepath.Join(scratch, "disk.img")
		content := strings.Repeat(fmt.Sprintf("churn %d ", i%7), 20000)
		if err := os.WriteFile(disk, []byte(content), 0o644); err != nil {
			os.Exit(3)
		}
		name := fmt.Sprintf("churn%d", i%3)
		_ = s.Put(name, "latest", validManifest(), disk)
		_, _ = s.Tag(name+":latest", name, "stable")
		if i%2 == 0 {
			_ = s.Remove(name + ":stable")
		}
		if i%5 == 0 {
			_ = s.Remove(name + ":latest")
		}
	}
}
