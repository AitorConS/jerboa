package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/release"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about installing and using the kernel toolset when the
// download fails or lies, the install is interrupted, or the files on disk
// are incomplete. The rule: a toolset is replaced by a whole, verified one or
// left as it was, its version marker never names files it does not describe,
// and an incomplete toolset is noticed rather than used.

// kernelOrigin serves kernel files and lets a test change what each answers.
type kernelOrigin struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies map[string]string
	serve  map[string]func(w http.ResponseWriter, body string)
	hits   map[string]int
}

func newKernelOrigin(t *testing.T, version string) *kernelOrigin {
	t.Helper()
	o := &kernelOrigin{
		bodies: map[string]string{
			"kernel.img":    "KERNEL " + version,
			"boot.img":      "BOOT " + version,
			"kernel-fc.img": "FC KERNEL " + version,
			"mkfs":          "#!/bin/sh\n# mkfs " + version + "\n",
		},
		serve: map[string]func(http.ResponseWriter, string){},
		hits:  map[string]int{},
	}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		o.mu.Lock()
		body, ok := o.bodies[name]
		serve := o.serve[name]
		o.hits[name]++
		o.mu.Unlock()
		switch {
		case !ok:
			http.NotFound(w, r)
		case serve != nil:
			serve(w, body)
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

// component describes the origin's files as a signed manifest would.
func (o *kernelOrigin) component(version string) release.Component {
	k := release.Component{Version: version, Files: map[string]release.Asset{}}
	for name, body := range o.bodies {
		sum := sha256.Sum256([]byte(body))
		k.Files[name] = release.Asset{URL: o.srv.URL + "/" + name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	}
	return k
}

func (o *kernelOrigin) client() *release.Client { return &release.Client{HTTP: o.srv.Client()} }

var toolsetFiles = []string{"kernel.img", "boot.img", fcKernelLocalName, "mkfs"}

// installed installs version from a fresh origin and returns the directory.
func installed(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	o := newKernelOrigin(t, version)
	require.NoError(t, installKernelFiles(context.Background(), o.client(), dir, o.component(version)))
	return dir
}

// requireToolset checks dir holds exactly the files of version, with the
// marker naming it, and nothing an interrupted install would leave.
func requireToolset(t *testing.T, dir, version string) {
	t.Helper()
	want := newKernelOrigin(t, version).bodies
	for _, name := range toolsetFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, want[name], string(data), "%s is not the %s file", name, version)
	}
	assert.Equal(t, version, LocalVersion(dir))
	assert.True(t, Exist(dir))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".staging-"), "a staging directory was left behind")
		assert.False(t, strings.HasSuffix(e.Name(), ".partial"), "a partial download was left behind: %s", e.Name())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "mkfs"))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode().Perm()&0o111, "mkfs is not executable")
	}
}

// An update whose download fails for any one file, in any way a server can
// fail, must leave the installed toolset exactly as it was: every file is
// downloaded and verified before the first one is replaced.
func TestFaults_UpdateWithOneFileFailingKeepsTheOldToolset(t *testing.T) {
	failures := map[string]func(w http.ResponseWriter, body string){
		"not found":    func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusNotFound) },
		"forbidden":    func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusForbidden) },
		"other bytes":  func(w http.ResponseWriter, body string) { _, _ = w.Write([]byte(strings.ToUpper(body) + "!")) },
		"same length":  func(w http.ResponseWriter, body string) { _, _ = w.Write([]byte(strings.Repeat("x", len(body)))) },
		"empty":        func(http.ResponseWriter, string) {},
		"an HTML page": func(w http.ResponseWriter, _ string) { _, _ = w.Write([]byte("<html>login required</html>")) },
		"too long": func(w http.ResponseWriter, body string) {
			_, _ = w.Write([]byte(body + strings.Repeat("padding", 1000)))
		},
	}
	for _, file := range []string{"kernel.img", "boot.img", "kernel-fc.img", "mkfs"} {
		for name, serve := range failures {
			t.Run(file+"/"+name, func(t *testing.T) {
				dir := installed(t, "v0.2.0")
				o := newKernelOrigin(t, "v0.3.0")
				o.serve[file] = serve
				err := installKernelFiles(context.Background(), o.client(), dir, o.component("v0.3.0"))
				require.Error(t, err)
				requireToolset(t, dir, "v0.2.0")

				// Once the origin is healthy the same update goes through.
				delete(o.serve, file)
				require.NoError(t, installKernelFiles(context.Background(), o.client(), dir, o.component("v0.3.0")))
				requireToolset(t, dir, "v0.3.0")
			})
		}
	}
}

// A first install that fails leaves no toolset at all, so the next run
// downloads one instead of using half of one.
func TestFaults_FailedFirstInstallLeavesNoToolset(t *testing.T) {
	for _, file := range []string{"kernel.img", "boot.img", "mkfs"} {
		t.Run(file, func(t *testing.T) {
			dir := t.TempDir()
			o := newKernelOrigin(t, "v0.3.0")
			o.serve[file] = func(w http.ResponseWriter, body string) { _, _ = w.Write([]byte(body[:len(body)/2])) }
			require.Error(t, installKernelFiles(context.Background(), o.client(), dir, o.component("v0.3.0")))
			assert.False(t, Exist(dir), "a failed install looks like an installed toolset")
			assert.False(t, HasLocalVersion(dir), "a failed install recorded a version")
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

// A manifest that does not describe a whole toolset, or describes a file
// without a digest to check it against, installs nothing.
func TestFaults_IncompleteManifestInstallsNothing(t *testing.T) {
	o := newKernelOrigin(t, "v0.3.0")
	for _, missing := range []string{"kernel.img", "boot.img", "mkfs"} {
		t.Run("without "+missing, func(t *testing.T) {
			dir := installed(t, "v0.2.0")
			k := o.component("v0.3.0")
			delete(k.Files, missing)
			err := installKernelFiles(context.Background(), o.client(), dir, k)
			require.ErrorContains(t, err, missing)
			requireToolset(t, dir, "v0.2.0")
		})
		t.Run("no digest for "+missing, func(t *testing.T) {
			dir := installed(t, "v0.2.0")
			k := o.component("v0.3.0")
			a := k.Files[missing]
			a.SHA256 = ""
			k.Files[missing] = a
			err := installKernelFiles(context.Background(), o.client(), dir, k)
			require.ErrorContains(t, err, "no sha256")
			requireToolset(t, dir, "v0.2.0")
		})
	}
	// The optional Firecracker kernel may be absent.
	dir := t.TempDir()
	k := o.component("v0.3.0")
	delete(k.Files, "kernel-fc.img")
	require.NoError(t, installKernelFiles(context.Background(), o.client(), dir, k))
	assert.True(t, Exist(dir))
	assert.False(t, FCKernelExists(dir))
}

// The update is canceled while files are downloading (Ctrl-C, a daemon
// shutting down): nothing of the new toolset is installed.
func TestFaults_CanceledUpdateKeepsTheOldToolset(t *testing.T) {
	dir := installed(t, "v0.2.0")
	o := newKernelOrigin(t, "v0.3.0")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	o.serve["boot.img"] = func(w http.ResponseWriter, body string) {
		// Send half the file, then hold the connection until the client
		// gives up.
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write([]byte(body[:len(body)/2]))
		w.(http.Flusher).Flush()
		once.Do(func() { close(started) })
		<-ctx.Done()
	}
	go func() {
		<-started
		cancel()
	}()
	err := installKernelFiles(ctx, o.client(), dir, o.component("v0.3.0"))
	require.Error(t, err)
	requireToolset(t, dir, "v0.2.0")
}

// The version marker is replaced whole. A save that fails at any step leaves
// the previous version, never an empty or half-written marker that would make
// an installed toolset look unversioned.
func TestFaults_SaveLocalVersionFailsAtEveryStep(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, SaveLocalVersion(dir, "v0.2.0"))
			inj := durabletest.Inject(t, f, 0, nil)
			err := SaveLocalVersion(dir, "v0.3.0")
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			require.True(t, HasLocalVersion(dir), "the marker was lost")
			if err != nil && f.Step != durable.StepSyncDir {
				assert.Equal(t, "v0.2.0", LocalVersion(dir))
			} else {
				assert.Equal(t, "v0.3.0", LocalVersion(dir))
			}
			require.NoError(t, SaveLocalVersion(dir, "v0.3.0"))
			assert.Equal(t, "v0.3.0", LocalVersion(dir))
		})
	}
}

// The files are in place and recording the version fails: the install
// reports it, so the caller does not go on believing the update is complete.
func TestFaults_InstallReportsAFailedVersionMarker(t *testing.T) {
	dir := installed(t, "v0.2.0")
	o := newKernelOrigin(t, "v0.3.0")
	inj := durabletest.Inject(t, durabletest.Failure{Step: durable.StepRename, Err: os.ErrPermission}, 0, nil)
	err := installKernelFiles(context.Background(), o.client(), dir, o.component("v0.3.0"))
	require.True(t, inj.Fired())
	durable.SetFault(nil)
	require.Error(t, err)
	assert.Equal(t, "v0.2.0", LocalVersion(dir), "the marker names a version whose install failed")
	// Repeating the install completes it.
	require.NoError(t, installKernelFiles(context.Background(), o.client(), dir, o.component("v0.3.0")))
	requireToolset(t, dir, "v0.3.0")
}

// A marker that is missing, empty or garbage means the version is unknown.
// An unknown version is never newer or older than a real one, so it neither
// triggers nor hides an update, and no recipe's mkfs is taken from it.
func TestFaults_DamagedVersionMarker(t *testing.T) {
	cases := map[string][]byte{
		"empty":       nil,
		"whitespace":  []byte(" \n\t\n"),
		"zeroed":      make([]byte, 7),
		"garbage":     {0xff, 0xfe, 0x00, 0x01},
		"half a line": []byte("v0."),
		"a word":      []byte("latest\n"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "mkfs"), []byte("installed mkfs"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, versionFileName), content, 0o644))
			v := LocalVersion(dir)
			assert.False(t, IsNewer(v, "v0.3.0") && IsNewer("v0.3.0", v))
			assert.False(t, IsNewer("v0.3.0", v), "a damaged marker reads as newer than a release")

			// A recipe pinned to a real version must not get the installed
			// mkfs on the strength of a marker that does not name it.
			unreachable := func() (*release.Client, error) { return nil, fmt.Errorf("asked the network") }
			_, err := mkfsForVersion(context.Background(), dir, "0.3.0", unreachable)
			require.ErrorContains(t, err, "asked the network")
		})
	}
	dir := t.TempDir()
	assert.Equal(t, UnknownVersion, LocalVersion(dir))
	assert.False(t, HasLocalVersion(dir))
	assert.False(t, IsNewer("v0.0.1", UnknownVersion), "an unknown version is not an update to install")
	require.NoError(t, os.Mkdir(filepath.Join(dir, versionFileName), 0o755))
	assert.Equal(t, UnknownVersion, LocalVersion(dir), "a directory in place of the marker")
	assert.False(t, HasLocalVersion(dir))
}

// A toolset directory that lost any one of its files is not a toolset: it is
// reported as absent, which is what makes the caller fetch it again.
func TestFaults_ToolsetMissingAFileIsNotUsed(t *testing.T) {
	for _, missing := range []string{"mkfs", "kernel.img", "boot.img"} {
		t.Run(missing, func(t *testing.T) {
			dir := installed(t, "v0.2.0")
			require.True(t, Exist(dir))
			require.NoError(t, os.Remove(filepath.Join(dir, missing)))
			assert.False(t, Exist(dir), "a toolset without %s is taken for a whole one", missing)

			// Fetching it again repairs it in place.
			o := newKernelOrigin(t, "v0.2.0")
			require.NoError(t, installKernelFiles(context.Background(), o.client(), dir, o.component("v0.2.0")))
			requireToolset(t, dir, "v0.2.0")
		})
	}
	dir := installed(t, "v0.2.0")
	require.NoError(t, os.Remove(filepath.Join(dir, fcKernelLocalName)))
	assert.True(t, Exist(dir), "the Firecracker kernel is fetched on demand, not part of the base toolset")
	assert.False(t, FCKernelExists(dir))
}

// Clearing the cache removes the marker with the files, so a cleared
// directory is never described by the version it used to hold.
func TestFaults_ClearCachedToolsLeavesNothingBehind(t *testing.T) {
	dir := installed(t, "v0.2.0")
	require.NoError(t, ClearCachedTools(dir))
	assert.False(t, Exist(dir))
	assert.False(t, HasLocalVersion(dir))
	assert.False(t, FCKernelExists(dir))
	require.NoError(t, ClearCachedTools(dir), "clearing an empty directory is not an error")
	require.NoError(t, ClearCachedTools(filepath.Join(dir, "absent")))
}

// The marker says the installed toolset is the version a recipe wants, but
// its mkfs is missing or cannot run: the recipe's mkfs is fetched for that
// version instead of failing or running something else.
func TestFaults_InstalledMkfsUnusableFallsBackToTheVersionCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable bits")
	}
	body := []byte("#!/bin/sh\nexit 0\n")
	breaks := map[string]func(t *testing.T, mkfs string){
		"missing":        func(t *testing.T, mkfs string) { require.NoError(t, os.Remove(mkfs)) },
		"not executable": func(t *testing.T, mkfs string) { require.NoError(t, os.Chmod(mkfs, 0o644)) },
		"a directory": func(t *testing.T, mkfs string) {
			require.NoError(t, os.Remove(mkfs))
			require.NoError(t, os.Mkdir(mkfs, 0o755))
		},
	}
	for name, breakIt := range breaks {
		t.Run(name, func(t *testing.T) {
			cl, downloads := toolsetOrigin(t, body)
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "mkfs"), []byte("installed"), 0o755))
			require.NoError(t, SaveLocalVersion(dir, "v0.2.3"))
			breakIt(t, filepath.Join(dir, "mkfs"))

			path, err := mkfsForVersion(context.Background(), dir, "0.2.3", func() (*release.Client, error) { return cl, nil })
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(dir, versionsDirName, "v0.2.3", "mkfs"), path)
			assert.Equal(t, int32(1), downloads.Load())
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, body, got)
		})
	}
}

// What a killed download leaves in the per-version cache: a partial file, or
// the whole file not yet made executable. Neither is run; the mkfs is
// fetched again and the result is the published one.
func TestFaults_InterruptedMkfsDownloadIsFetchedAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable bits")
	}
	body := []byte("#!/bin/sh\n# the published mkfs\nexit 0\n")
	leftovers := map[string]func(t *testing.T, cached string){
		"partial file only": func(t *testing.T, cached string) {
			require.NoError(t, os.WriteFile(cached+".partial", body[:10], 0o644))
		},
		"whole file, not executable": func(t *testing.T, cached string) {
			require.NoError(t, os.WriteFile(cached, body, 0o644))
		},
		"truncated file, not executable": func(t *testing.T, cached string) {
			require.NoError(t, os.WriteFile(cached, body[:10], 0o644))
		},
		"partial file of something else": func(t *testing.T, cached string) {
			require.NoError(t, os.WriteFile(cached+".partial", []byte("left by another version of the file"), 0o644))
		},
	}
	for name, leave := range leftovers {
		t.Run(name, func(t *testing.T) {
			cl, _ := toolsetOrigin(t, body)
			dir := t.TempDir()
			cached := filepath.Join(dir, versionsDirName, "v0.2.3", "mkfs")
			require.NoError(t, os.MkdirAll(filepath.Dir(cached), 0o700))
			leave(t, cached)

			path, err := mkfsForVersion(context.Background(), dir, "0.2.3", func() (*release.Client, error) { return cl, nil })
			require.NoError(t, err)
			require.Equal(t, cached, path)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, body, got, "an mkfs other than the published one would be run")
			assert.True(t, isExecutableFile(path))
			assert.NoFileExists(t, cached+".partial")
		})
	}
}

// The origin serves an mkfs that is not the one the signed toolset names.
// It is not kept, not made executable, and not found by a later call.
func TestFaults_MkfsThatDoesNotMatchTheSignedToolsetIsNotKept(t *testing.T) {
	signedFor := []byte("#!/bin/sh\n# the published mkfs\nexit 0\n")
	key := newMinisignKey(t)
	var doc []byte
	served := []byte("#!/bin/sh\nrm -rf /\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kernel/v0.2.3/toolset.json":
			_, _ = w.Write(doc)
		case "/kernel/v0.2.3/toolset.json.minisig":
			_, _ = w.Write(key.sign(doc))
		default:
			_, _ = w.Write(served)
		}
	}))
	defer srv.Close()
	sum := sha256.Sum256(signedFor)
	doc = fmt.Appendf(nil, `{"version":"v0.2.3","mkfs":{"%s-%s":{"url":"%s/kernel/v0.2.3/mkfs-host","sha256":"%s","size":%d}}}`,
		runtime.GOOS, runtime.GOARCH, srv.URL, hex.EncodeToString(sum[:]), len(signedFor))
	cl, err := release.NewClient(srv.URL, key.public())
	require.NoError(t, err)
	client := func() (*release.Client, error) { return cl, nil }
	dir := t.TempDir()
	cached := filepath.Join(dir, versionsDirName, "v0.2.3", "mkfs")

	for range 2 {
		_, err = mkfsForVersion(context.Background(), dir, "0.2.3", client)
		require.Error(t, err)
		assert.NoFileExists(t, cached, "an unverified mkfs was left where the next build would run it")
	}

	// A toolset signed by another key is refused before anything is fetched.
	stranger := newMinisignKey(t)
	other, err := release.NewClient(srv.URL, stranger.public())
	require.NoError(t, err)
	_, err = mkfsForVersion(context.Background(), dir, "0.2.3", func() (*release.Client, error) { return other, nil })
	require.Error(t, err)
	assert.NoFileExists(t, cached)
}

// A recipe names its toolset version, and that name becomes a directory and
// part of a URL: one that is not a version must be refused before either.
func TestFaults_HostileToolsetVersions(t *testing.T) {
	unreachable := func() (*release.Client, error) { return nil, fmt.Errorf("asked the network") }
	dir := t.TempDir()
	for _, v := range []string{"", "..", "../../etc", "v1.2.3/../../x", `v1.2.3\..\x`, "v1.2.3\x00", "latest", "v1", "1.2", "v1.2.3 && rm -rf /", strings.Repeat("9", 400)} {
		_, err := mkfsForVersion(context.Background(), dir, v, unreachable)
		require.ErrorContains(t, err, "invalid toolset version", "%q", v)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a refused version created something on disk")
}
