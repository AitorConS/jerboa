package pkg

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about what the package stores do when things go wrong: the
// disk fills up, the device errors, the process is killed or the machine loses
// power in the middle of a write, or a file is found damaged afterwards. The
// rule they hold the stores to: a failure may cost the operation that was in
// flight, never a package that was already usable, and a package is only ever
// handed to a build when its bytes are the ones its metadata names.

const faultPlatform = "linux/arm64"

// builtPackage is a real package (an ELF program plus data files) as an index
// would publish it: its metadata, its archive, and the bytes of every file.
type builtPackage struct {
	meta    Package
	archive []byte
	content map[string][]byte
}

// buildPackage creates name:version in a scratch store and returns what a
// registry would serve for it.
func buildPackage(t *testing.T, name, version string) builtPackage {
	t.Helper()
	root := t.TempDir()
	content := map[string][]byte{
		"bin/app":      syntheticELF(elf.EM_AARCH64, "", nil, ""),
		"etc/app.conf": []byte("listen = 8080\nname = " + name + " " + version + "\n"),
		"lib/data.bin": bytes.Repeat([]byte("payload "), 512),
	}
	files := []File{{HostPath: putFixture(t, root, "src/bin/app", content["bin/app"]), GuestPath: "bin/app"}}
	for _, guest := range []string{"etc/app.conf", "lib/data.bin"} {
		files = append(files, File{HostPath: putFixture(t, root, "src/"+guest, content[guest]), GuestPath: guest})
	}
	scratch, err := NewStore(filepath.Join(root, "scratch"))
	require.NoError(t, err)
	sel, err := scratch.ForPlatform(faultPlatform)
	require.NoError(t, err)
	require.NoError(t, sel.CreateFromFiles(name, version, files, "test package", ""))
	archive, err := os.ReadFile(filepath.Join(sel.PackageDir(name, version), "files.tar.gz"))
	require.NoError(t, err)
	return builtPackage{meta: readMeta(t, sel, name, version), archive: archive, content: content}
}

// fakeIndex is a loopback package index: packages.json plus the archives it
// names. It can be switched off to stand for a machine with no network.
type fakeIndex struct {
	srv   *httptest.Server
	down  atomic.Bool
	mu    sync.Mutex
	index []byte
	blobs map[string][]byte
}

// newFakeIndex serves pkgs and points IndexURL at itself for the test.
func newFakeIndex(t *testing.T, pkgs ...builtPackage) *fakeIndex {
	t.Helper()
	f := &fakeIndex{blobs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		f.mu.Lock()
		index, blob, ok := f.index, f.blobs[r.URL.Path], false
		_, ok = f.blobs[r.URL.Path]
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/packages.json":
			_, _ = w.Write(index)
		case ok:
			sum := sha256.Sum256(blob)
			w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	withIndexURL(t, f.srv.URL+"/packages.json")
	f.publish(t, pkgs...)
	return f
}

// publish replaces the index with one listing exactly pkgs.
func (f *fakeIndex) publish(t *testing.T, pkgs ...builtPackage) {
	t.Helper()
	idx := Index{Packages: map[string][]Package{}}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range pkgs {
		path := "/blob/" + b.meta.Name + "-" + b.meta.Version + ".tar.gz"
		meta := b.meta
		meta.URL = f.srv.URL + path
		idx.Packages[meta.Name] = append(idx.Packages[meta.Name], meta)
		f.blobs[path] = b.archive
	}
	data, err := json.Marshal(idx)
	require.NoError(t, err)
	f.index = data
}

// requireResolved checks that what Resolve returned is exactly the package
// that was published: the archive digest the index names, and every file with
// the bytes it was built from.
func requireResolved(t *testing.T, files []File, ref Reference, want builtPackage) {
	t.Helper()
	require.Equal(t, want.meta.SHA256, ref.SHA256, "resolved an archive other than the one the index names")
	require.Equal(t, want.meta.ContentDigest, ref.ContentDigest)
	require.Len(t, files, len(want.content))
	for _, f := range files {
		got, err := os.ReadFile(f.HostPath)
		require.NoError(t, err)
		expected, ok := want.content[f.GuestPath]
		require.True(t, ok, "resolved a file the package does not have: %s", f.GuestPath)
		require.True(t, bytes.Equal(expected, got), "file %s does not hold the bytes that were packaged", f.GuestPath)
	}
}

// noDurableTemps checks that a write that failed with an error (not a crash)
// cleaned up after itself.
func noDurableTemps(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		require.NoError(t, err)
		assert.False(t, durable.IsTemp(filepath.Base(path)), "temporary file left behind: %s", path)
		return nil
	}))
}

// requireFired checks the injected failure was reached. The digest index is
// replaced atomically but not flushed, so its write has no flush to fail:
// those steps are skipped for it.
func requireFired(t *testing.T, inj *durabletest.Injection, f durabletest.Failure) {
	t.Helper()
	if !inj.Fired() && (f.Step == durable.StepSync || f.Step == durable.StepSyncDir) {
		t.Skip("this write is not flushed, so it has no such step")
	}
	require.True(t, inj.Fired(), "the failure was never reached")
}

func newFaultStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "packages")
	s, err := NewStore(root)
	require.NoError(t, err)
	return s, root
}

// Resolving a package for the first time downloads it, records its metadata,
// caches the index and saves a digest index. Whichever of those writes fails,
// and however, the next build must get the package the index names, online or
// not.
func TestFaults_ResolveFromIndex(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")

	var writes int
	{
		s, _ := newFaultStore(t)
		newFakeIndex(t, b)
		writes = durabletest.Count(t, nil, func() {
			_, _, err := s.Resolve("app", "1.0.0", faultPlatform)
			require.NoError(t, err)
		})
	}
	require.GreaterOrEqual(t, writes, 3, "expected the index cache, the metadata and the digest index to be written")

	for nth := range writes {
		for _, f := range durabletest.Failures() {
			t.Run(fmt.Sprintf("write%d/%s", nth, f.Name), func(t *testing.T) {
				s, root := newFaultStore(t)
				idx := newFakeIndex(t, b)

				inj := durabletest.Inject(t, f, nth, nil)
				files, ref, err := s.Resolve("app", "1.0.0", faultPlatform)
				requireFired(t, inj, f)
				durable.SetFault(nil)
				if err == nil {
					requireResolved(t, files, ref, b)
				}

				// The build is simply run again.
				files, ref, err = s.Resolve("app", "1.0.0", faultPlatform)
				require.NoError(t, err, "a failed write left the package unusable")
				requireResolved(t, files, ref, b)

				// And once more with the network gone.
				idx.down.Store(true)
				files, ref, err = s.Resolve("app", "1.0.0", faultPlatform)
				require.NoError(t, err, "the package no longer resolves offline")
				requireResolved(t, files, ref, b)

				if !f.Crash() {
					noDurableTemps(t, root)
				}
			})
		}
	}
}

// The metadata of a downloaded package names the digest its archive is checked
// against. Rewriting it must leave the old record or the new one, whole.
func TestFaults_SaveMetaKeepsARecord(t *testing.T) {
	old := Package{Name: "node", Version: "20.0.0", SHA256: strings.Repeat("a", 64), Size: 10, Description: "old"}
	next := old
	next.SHA256 = strings.Repeat("b", 64)
	next.Description = "new"

	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, root := newFaultStore(t)
			require.NoError(t, s.SaveMeta(old))

			inj := durabletest.Inject(t, f, 0, nil)
			err := s.SaveMeta(next)
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			raw, rerr := os.ReadFile(filepath.Join(s.PackageDir("node", "20.0.0"), "meta.json"))
			require.NoError(t, rerr)
			var got Package
			require.NoError(t, json.Unmarshal(raw, &got), "a failed save left metadata that does not parse")
			want := next
			if err != nil && f.Step != durable.StepSyncDir {
				want = old
			}
			assert.Equal(t, want.SHA256, got.SHA256)
			assert.Equal(t, want.Description, got.Description)

			list, lerr := s.List()
			require.NoError(t, lerr)
			require.Len(t, list, 1)
			assert.Equal(t, want.SHA256, list[0].SHA256)
			if !f.Crash() {
				noDurableTemps(t, root)
			}
		})
	}
}

// Creating a package writes its archive and then its metadata. A failure of
// the metadata write must not leave an archive nobody can describe in the way
// of the retry.
func TestFaults_CreateFromFiles(t *testing.T) {
	src := t.TempDir()
	bin := putFixture(t, src, "app", syntheticELF(elf.EM_AARCH64, "", nil, ""))
	create := func(s *Store) error {
		sel, err := s.ForPlatform(faultPlatform)
		if err != nil {
			return err
		}
		return sel.CreateFromFiles("app", "1.0.0", []File{{HostPath: bin, GuestPath: "app"}}, "", "")
	}
	offlineIndex(t)

	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, root := newFaultStore(t)
			inj := durabletest.Inject(t, f, 0, nil)
			err := create(s)
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			_, lerr := s.List()
			require.NoError(t, lerr, "the store no longer lists")

			if err != nil {
				if f.Crash() {
					// A killed process cleans nothing up; whatever it left is
					// removed like any other package.
					_ = s.Remove("app", "1.0.0")
				}
				require.NoError(t, create(s), "a failed create blocks the retry")
			}
			files, ref, rerr := s.Resolve("app", "1.0.0", faultPlatform)
			require.NoError(t, rerr)
			require.Len(t, files, 1)
			meta := readMeta(t, mustForPlatform(t, s), "app", "1.0.0")
			assert.Equal(t, meta.SHA256, ref.SHA256)
			if !f.Crash() {
				noDurableTemps(t, root)
			}
		})
	}
}

func mustForPlatform(t *testing.T, s *Store) *Store {
	t.Helper()
	sel, err := s.ForPlatform(faultPlatform)
	require.NoError(t, err)
	return sel
}

// offlineIndex points IndexURL at a server that always fails.
func offlineIndex(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	withIndexURL(t, srv.URL+"/packages.json")
}

// The cached index is what an offline build resolves from. Refreshing it must
// never trade a good copy for a broken one.
func TestFaults_IndexCacheWrite(t *testing.T) {
	v1 := buildPackage(t, "app", "1.0.0")
	v2 := buildPackage(t, "app", "2.0.0")

	versions := func(idx *Index) []string {
		var out []string
		for _, p := range idx.Packages["app"] {
			out = append(out, p.Version)
		}
		sort.Strings(out)
		return out
	}

	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, _ := newFaultStore(t)
			idx := newFakeIndex(t, v1)
			_, err := s.FetchIndexCached()
			require.NoError(t, err)

			idx.publish(t, v1, v2)
			inj := durabletest.Inject(t, f, 0, nil)
			got, err := s.FetchIndexCached()
			require.True(t, inj.Fired())
			durable.SetFault(nil)
			require.NoError(t, err, "a cache write failure must not fail the build")
			assert.Equal(t, []string{"1.0.0", "2.0.0"}, versions(got))

			idx.down.Store(true)
			got, err = s.FetchIndexCached()
			require.NoError(t, err, "the cached index was lost")
			if f.Step == durable.StepSyncDir {
				assert.Equal(t, []string{"1.0.0", "2.0.0"}, versions(got))
			} else {
				assert.Equal(t, []string{"1.0.0"}, versions(got))
			}
		})
	}
}

// The digest index only saves work. Failing to write it must not fail the
// build, and must not weaken the next check of the package.
func TestFaults_DigestIndexWrite(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")

	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, root := newFaultStore(t)
			newFakeIndex(t, b)
			match := func(path string) bool { return filepath.Base(path) == digestsFile }

			inj := durabletest.Inject(t, f, 0, match)
			files, ref, err := s.Resolve("app", "1.0.0", faultPlatform)
			requireFired(t, inj, f)
			durable.SetFault(nil)
			require.NoError(t, err)
			requireResolved(t, files, ref, b)

			files, ref, err = s.Resolve("app", "1.0.0", faultPlatform)
			require.NoError(t, err)
			requireResolved(t, files, ref, b)

			// The package is still checked: a file changed behind its back is
			// refused whatever state the digest index was left in.
			require.NoError(t, os.WriteFile(files[len(files)-1].HostPath, []byte("changed"), 0o644))
			_, _, err = s.Resolve("app", "1.0.0", faultPlatform)
			require.Error(t, err, "a modified package file was accepted")
			if !f.Crash() {
				noDurableTemps(t, root)
			}
		})
	}
}

// opsFixture places an extracted ops package in a store, as Download and
// Extract leave it, and returns the store and the archive's SHA-256.
func opsFixture(t *testing.T) (*OpsStore, string) {
	t.Helper()
	archive := createOpsPackageArchive(t, map[string]string{
		"package.manifest":      `{"Program":"node","Args":["/node"],"Version":"v20.0.0"}`,
		"node":                  string(syntheticELF(elf.EM_AARCH64, "", nil, "")),
		"sysroot/lib/libnss.so": "lib content",
		"sysroot/etc/hosts":     "127.0.0.1 localhost\n",
	})
	s, err := NewOpsStoreArch(t.TempDir(), "arm64")
	require.NoError(t, err)
	dir := s.PackageDir("eyberg", "node", "v20.0.0")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "arm64.tar.gz"), archive, 0o644))
	require.NoError(t, s.Extract("eyberg", "node", "v20.0.0"))
	sum := sha256.Sum256(archive)
	return s, hex.EncodeToString(sum[:])
}

func guestPaths(files []File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.GuestPath)
	}
	sort.Strings(out)
	return out
}

// An ops package keeps its digest index inside the directory whose files go
// into the image. Whatever an interrupted write leaves there must never be
// packed into an image or change the package's identity.
func TestFaults_OpsDigestIndexWrite(t *testing.T) {
	var wantPaths []string
	var wantContent string
	{
		s, sha := opsFixture(t)
		files, ref, err := s.VerifiedFiles("eyberg", "node", "v20.0.0", faultPlatform, sha)
		require.NoError(t, err)
		wantPaths, wantContent = guestPaths(files), ref.ContentDigest
		require.Equal(t, []string{"etc/hosts", "lib/libnss.so", "node"}, wantPaths)
	}

	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, sha := opsFixture(t)
			inj := durabletest.Inject(t, f, 0, nil)
			files, ref, err := s.VerifiedFiles("eyberg", "node", "v20.0.0", faultPlatform, sha)
			requireFired(t, inj, f)
			durable.SetFault(nil)
			require.NoError(t, err, "a digest index write failure must not fail the build")
			assert.Equal(t, wantPaths, guestPaths(files))
			assert.Equal(t, wantContent, ref.ContentDigest)

			files, ref, err = s.VerifiedFiles("eyberg", "node", "v20.0.0", faultPlatform, sha)
			require.NoError(t, err)
			assert.Equal(t, wantPaths, guestPaths(files), "a leftover of the failed write is listed as package content")
			assert.Equal(t, wantContent, ref.ContentDigest)
			assert.Equal(t, sha, ref.SHA256)
		})
	}
}

// The cached ops manifest names the SHA-256 of every ops package. Refreshing
// it must leave the old manifest or the new one, whole.
func TestFaults_OpsManifestWrite(t *testing.T) {
	manifest := func(sha string) []byte {
		data, err := json.Marshal(OpsPackageList{Version: 1, Packages: []OpsPackage{
			{Namespace: "eyberg", Name: "node", Version: "v20.0.0", SHA256: sha},
		}})
		require.NoError(t, err)
		return data
	}
	oldSHA, newSHA := strings.Repeat("a", 64), strings.Repeat("b", 64)

	var served atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(*served.Load())
	}))
	t.Cleanup(srv.Close)
	prevURL, prevTTL := OpsPackageManifestURL, OpsManifestTTL
	OpsPackageManifestURL, OpsManifestTTL = srv.URL+"/manifest.json", 0
	t.Cleanup(func() { OpsPackageManifestURL, OpsManifestTTL = prevURL, prevTTL })

	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			s, err := NewOpsStoreArch(t.TempDir(), "amd64")
			require.NoError(t, err)
			require.NoError(t, s.SaveManifest(manifest(oldSHA)))

			next := manifest(newSHA)
			served.Store(&next)
			inj := durabletest.Inject(t, f, 0, nil)
			got, err := s.FetchManifestCached()
			require.True(t, inj.Fired())
			durable.SetFault(nil)
			require.NoError(t, err, "a cache write failure must not fail the build")
			require.Len(t, got.Packages, 1)
			assert.Equal(t, newSHA, got.Packages[0].SHA256, "the build must use the manifest it just fetched")

			cached, err := s.LoadCachedManifest()
			require.NoError(t, err, "a failed save left a manifest that does not parse")
			require.Len(t, cached.Packages, 1)
			if f.Step == durable.StepSyncDir {
				assert.Equal(t, newSHA, cached.Packages[0].SHA256)
			} else {
				assert.Equal(t, oldSHA, cached.Packages[0].SHA256)
			}
			if !f.Crash() {
				noDurableTemps(t, s.root)
			}
		})
	}
}

// damage rewrites a file the ways a power cut, a failing disk or a stray
// process can leave it.
var damages = []struct {
	name  string
	apply func(t *testing.T, path string)
}{
	{"truncated", func(t *testing.T, path string) {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data[:len(data)/2], 0o644))
	}},
	{"empty", func(t *testing.T, path string) {
		require.NoError(t, os.WriteFile(path, nil, 0o644))
	}},
	{"zeroed", func(t *testing.T, path string) {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, make([]byte, info.Size()), 0o644))
	}},
	{"bit-flipped", func(t *testing.T, path string) {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data[len(data)/2] ^= 0x40
		require.NoError(t, os.WriteFile(path, data, 0o644))
	}},
	{"missing", func(t *testing.T, path string) {
		require.NoError(t, os.Remove(path))
	}},
}

// resolvedStore returns a store that has already resolved b once, so every
// file a build leaves behind exists, and the directory the package lives in.
func resolvedStore(t *testing.T, b builtPackage) (s *Store, idx *fakeIndex, pkgDir string) {
	t.Helper()
	s, _ = newFaultStore(t)
	idx = newFakeIndex(t, b)
	files, ref, err := s.Resolve(b.meta.Name, b.meta.Version, faultPlatform)
	require.NoError(t, err)
	requireResolved(t, files, ref, b)
	return s, idx, mustForPlatform(t, s).PackageDir(b.meta.Name, b.meta.Version)
}

// Metadata, the cached index and the digest index are all derived from what
// the index and the archive say. Losing or mangling any of them must not cost
// the package: the next build repairs or ignores the file, online or not.
func TestFaults_DamagedStateFilesAreRecovered(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	targets := map[string]func(root, pkgDir string) string{
		"meta.json":     func(_, pkgDir string) string { return filepath.Join(pkgDir, "meta.json") },
		"index.json":    func(root, _ string) string { return filepath.Join(root, "index.json") },
		".digests.json": func(_, pkgDir string) string { return filepath.Join(pkgDir, digestsFile) },
	}
	for target, pathOf := range targets {
		for _, d := range damages {
			for _, online := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/online=%v", target, d.name, online), func(t *testing.T) {
					s, idx, pkgDir := resolvedStore(t, b)
					d.apply(t, pathOf(s.root, pkgDir))
					idx.down.Store(!online)

					for range 2 {
						files, ref, err := s.Resolve("app", "1.0.0", faultPlatform)
						if err != nil && target == "meta.json" && d.name == "bit-flipped" {
							// Metadata that still parses but names another
							// digest cannot be told from a damaged archive:
							// refusing the package is the safe answer.
							require.ErrorIs(t, err, ErrChecksumMismatch)
							return
						}
						require.NoError(t, err, "a damaged %s made the package unusable", target)
						requireResolved(t, files, ref, b)
					}
				})
			}
		}
	}
}

// The archive and the files extracted from it are the package. When either is
// damaged the package must be refused, never served with other bytes, and it
// must work again once it is removed and fetched anew.
func TestFaults_DamagedPackageContentIsRefused(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	targets := map[string]string{
		"archive":        "files.tar.gz",
		"extracted file": filepath.Join("files", "lib", "data.bin"),
		"program":        filepath.Join("files", "bin", "app"),
	}
	for target, rel := range targets {
		for _, d := range damages {
			t.Run(target+"/"+d.name, func(t *testing.T) {
				s, _, pkgDir := resolvedStore(t, b)
				d.apply(t, filepath.Join(pkgDir, rel))

				for range 2 {
					_, _, err := s.Resolve("app", "1.0.0", faultPlatform)
					require.Error(t, err, "a package with a damaged %s was handed to the build", target)
				}

				require.NoError(t, mustForPlatform(t, s).Remove("app", "1.0.0"))
				files, ref, err := s.Resolve("app", "1.0.0", faultPlatform)
				require.NoError(t, err, "the package cannot be fetched again after being removed")
				requireResolved(t, files, ref, b)
			})
		}
	}
}

// A file added to the extracted tree is not part of the package and must not
// ride along into an image.
func TestFaults_FileAddedToExtractedTreeIsRefused(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	s, _, pkgDir := resolvedStore(t, b)
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files", "lib", "implant.so"), []byte("not in the archive"), 0o755))
	_, _, err := s.Resolve("app", "1.0.0", faultPlatform)
	require.Error(t, err)
}

// Packages published before content digests existed have only the archive to
// be checked against. Once the digest index says the extraction was verified,
// a file that later disappears must still be noticed: the package would
// otherwise go into an image without it.
func TestFaults_LegacyPackageMissingAFileIsRefused(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	b.meta.ContentDigest = ""
	s, _ := newFaultStore(t)
	newFakeIndex(t, b)
	for range 2 {
		files, _, err := s.Resolve("app", "1.0.0", faultPlatform)
		require.NoError(t, err)
		require.Len(t, files, 3)
	}
	pkgDir := mustForPlatform(t, s).PackageDir("app", "1.0.0")
	require.NoError(t, os.Remove(filepath.Join(pkgDir, "files", "lib", "data.bin")))

	files, _, err := s.Resolve("app", "1.0.0", faultPlatform)
	require.Error(t, err, "the package was accepted with only %d of its 3 files", len(files))
}

// A file rewritten with the same size and its modification time put back is
// the change a digest cache keyed on size and mtime would miss.
func TestFaults_SameSizeSameMtimeChangeIsNoticed(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	s, _, pkgDir := resolvedStore(t, b)
	target := filepath.Join(pkgDir, "files", "lib", "data.bin")
	info, err := os.Stat(target)
	require.NoError(t, err)
	if id, err := statID(target); err != nil || (id.Ino == 0 && id.ChangeNs == 0) {
		t.Skip("no file identity beyond size and modification time on this platform")
	}

	data := bytes.Repeat([]byte("PAYLOAD "), 512)
	require.Len(t, data, int(info.Size()))
	require.NoError(t, os.WriteFile(target, data, 0o755))
	require.NoError(t, os.Chtimes(target, info.ModTime(), info.ModTime()))

	_, _, err = s.Resolve("app", "1.0.0", faultPlatform)
	require.Error(t, err, "a rewritten file was accepted on the strength of its old digest")
}

// A digest index is trusted only for files that are still the same file, and
// only when it is well formed. Anything else is treated as no index at all.
func TestFaults_DigestIndexGarbageIsIgnored(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "lib.so")
	require.NoError(t, os.WriteFile(file, []byte("library"), 0o644))
	want := sha256.Sum256([]byte("library"))
	id, err := statID(file)
	require.NoError(t, err)
	idJSON, err := json.Marshal(id)
	require.NoError(t, err)

	cases := map[string]string{
		"garbage":        "\x00\xff\x00",
		"truncated":      `{"files":{"lib.so":{"id":`,
		"null":           `null`,
		"null files":     `{"files":null}`,
		"array":          `[]`,
		"short digest":   `{"files":{"lib.so":{"id":` + string(idJSON) + `,"sha256":"abcd"}}}`,
		"not hex":        `{"files":{"lib.so":{"id":` + string(idJSON) + `,"sha256":"` + strings.Repeat("z", 64) + `"}}}`,
		"other identity": `{"files":{"lib.so":{"id":{"size":7,"mod_ns":1,"change_ns":1,"ino":1},"sha256":"` + strings.Repeat("0", 64) + `"}}}`,
		"escaping path":  `{"files":{"../lib.so":{"id":{"size":7,"mod_ns":1,"change_ns":1,"ino":1},"sha256":"` + strings.Repeat("0", 64) + `"}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			resetDigestCache()
			require.NoError(t, os.WriteFile(filepath.Join(dir, digestsFile), []byte(content), 0o600))
			SeedDigests(dir)
			got, err := fileDigest(file)
			require.NoError(t, err)
			assert.Equal(t, want, got, "a damaged digest index changed what a file hashes to")
			assert.False(t, archiveVerified(dir, strings.Repeat("0", 64), []File{{HostPath: file, GuestPath: "lib.so"}}))
		})
	}
}

// What a process killed between the download and the metadata write leaves
// behind: an archive in a platform directory with nothing describing it. It
// must not shadow the package the index offers.
func TestFaults_ArchiveWithoutMetadata(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	for _, meta := range []string{"", "{}", "null", `{"name":"app","version":"1.0.0"}`} {
		t.Run("meta="+meta, func(t *testing.T) {
			s, _ := newFaultStore(t)
			newFakeIndex(t, b)
			pkgDir := mustForPlatform(t, s).PackageDir("app", "1.0.0")
			require.NoError(t, os.MkdirAll(pkgDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files.tar.gz"), b.archive, 0o644))
			if meta != "" {
				require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "meta.json"), []byte(meta), 0o644))
			}

			files, ref, err := s.Resolve("app", "1.0.0", faultPlatform)
			require.NoError(t, err)
			requireResolved(t, files, ref, b)
		})
	}
}

// The same leftover holding bytes that are not the ones the index names (a
// half-written archive from a killed create, a stale download) must be
// refused rather than adopted.
func TestFaults_UndescribedArchiveIsCheckedAgainstTheIndex(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	other := buildPackage(t, "other", "1.0.0")
	s, _ := newFaultStore(t)
	newFakeIndex(t, b)
	pkgDir := mustForPlatform(t, s).PackageDir("app", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files.tar.gz"), other.archive, 0o644))

	_, _, err := s.Resolve("app", "1.0.0", faultPlatform)
	require.ErrorIs(t, err, ErrChecksumMismatch)
}

// Several builds started together resolve the same package for the first
// time. Each must get the whole package or an error, never a partly written
// one.
func TestFaults_ConcurrentFirstResolve(t *testing.T) {
	b := buildPackage(t, "app", "1.0.0")
	s, _ := newFaultStore(t)
	newFakeIndex(t, b)

	const builds = 8
	var wg sync.WaitGroup
	errs := make([]error, builds)
	results := make([][]File, builds)
	refs := make([]Reference, builds)
	start := make(chan struct{})
	for i := range builds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], refs[i], errs[i] = s.Resolve("app", "1.0.0", faultPlatform)
		}()
	}
	close(start)
	wg.Wait()

	for i := range builds {
		require.NoError(t, errs[i], "build %d", i)
		requireResolved(t, results[i], refs[i], b)
	}
}

// The real thing: a process that keeps rewriting package metadata, the ops
// manifest and a digest index is killed outright, over and over. After each kill every one of those files must be whole.
func TestFaults_StoreSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills processes")
	}
	root := t.TempDir()
	for round := range 12 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChurnPackageStore$") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), "PKG_HELPER_ROOT="+root)
		out, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		// The helper prints one byte per completed pass; it is killed in the
		// middle of the pass that follows.
		r := bufio.NewReader(out)
		for range 2 + round*3 {
			_, err := r.ReadByte()
			require.NoError(t, err, "the helper died on its own")
		}
		require.NoError(t, cmd.Process.Kill())
		_ = cmd.Wait()

		s, err := NewStore(filepath.Join(root, "packages"))
		require.NoError(t, err)
		list, err := s.List()
		require.NoError(t, err, "round %d", round)
		require.Len(t, list, 1, "round %d", round)
		assert.Equal(t, "node", list[0].Name)
		assert.Len(t, list[0].SHA256, 64, "round %d: metadata lost its digest", round)

		for _, state := range []string{
			filepath.Join(root, "packages", "node", "20.0.0", "meta.json"),
			filepath.Join(root, "packages", "node", "20.0.0", digestsFile),
			filepath.Join(root, "ops", "manifest.json"),
		} {
			data, err := os.ReadFile(state)
			require.NoError(t, err, "round %d", round)
			assert.True(t, json.Valid(data), "round %d: %s is not whole after the kill", round, state)
		}
	}
}

func TestHelperChurnPackageStore(t *testing.T) {
	root := os.Getenv("PKG_HELPER_ROOT")
	if root == "" {
		t.Skip("helper process for TestFaults_StoreSurvivesKill")
	}
	s, err := NewStore(filepath.Join(root, "packages"))
	if err != nil {
		os.Exit(3)
	}
	ops, err := NewOpsStoreArch(filepath.Join(root, "ops"), "amd64")
	if err != nil {
		os.Exit(3)
	}
	pkgDir := s.PackageDir("node", "20.0.0")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		os.Exit(3)
	}
	lib := filepath.Join(pkgDir, "lib.so")
	if err := os.WriteFile(lib, []byte("library"), 0o644); err != nil {
		os.Exit(3)
	}
	if _, err := fileDigest(lib); err != nil {
		os.Exit(3)
	}
	for i := 0; ; i++ {
		meta := Package{Name: "node", Version: "20.0.0", SHA256: strings.Repeat("a", 64), Description: strings.Repeat("d", (i%40)*500)}
		manifest, _ := json.Marshal(OpsPackageList{Version: i, Packages: make([]OpsPackage, i%200)})
		if s.SaveMeta(meta) != nil || ops.SaveManifest(manifest) != nil ||
			SaveDigests(pkgDir, []File{{HostPath: lib}}, strings.Repeat("b", i%64)) != nil {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString(".")
	}
}
