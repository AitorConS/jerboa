package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A package archive comes from the network. These tests hand the extractor
// archives a download cut short would leave and archives built to write
// outside the package, and hold it to two rules: nothing is ever written
// outside the package's own directory, and a failed extraction leaves no
// files for the next build to mistake for the package.

type tarEntry struct {
	hdr  tar.Header
	body []byte
}

func regular(name, body string) tarEntry {
	return tarEntry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}, body: []byte(body)}
}

// tarGz builds an archive from entries without any of the checks a
// well-behaved writer applies: headers are written as given.
func tarGz(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		h := e.hdr
		if err := tw.WriteHeader(&h); err != nil {
			t.Skipf("the tar writer refuses this header: %v", err)
		}
		if len(e.body) > 0 {
			_, err := tw.Write(e.body)
			require.NoError(t, err)
		}
	}
	// Close errors when a header promised more than was written; such an
	// archive is exactly what a truncation test wants, so flush regardless.
	_ = tw.Close()
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

// planted puts archive in a fresh store as evil:1.0.0 and returns the store,
// the package directory and the directory that holds everything.
func planted(t testing.TB, archive []byte) (s *Store, pkgDir, sandbox string) {
	t.Helper()
	sandbox = t.TempDir()
	s, err := NewStore(filepath.Join(sandbox, "store", "packages"))
	require.NoError(t, err)
	pkgDir = s.PackageDir("evil", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files.tar.gz"), archive, 0o644))
	return s, pkgDir, sandbox
}

// requireContained checks nothing exists in sandbox outside the package
// directory, and inside it nothing but the archive and its extraction.
func requireContained(t testing.TB, sandbox, pkgDir string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(sandbox, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(pkgDir, path)
		require.NoError(t, rerr)
		require.False(t, strings.HasPrefix(rel, ".."), "the archive wrote outside its package: %s", path)
		if rel != "files.tar.gz" && rel != digestsFile {
			require.True(t, strings.HasPrefix(rel, "files"+string(filepath.Separator)), "the archive wrote beside its extraction: %s", path)
		}
		require.Zero(t, d.Type()&os.ModeSymlink, "the extraction created a link: %s", path)
		return nil
	}))
}

func requireNotExtracted(t testing.TB, s *Store, pkgDir string) {
	t.Helper()
	assert.NoDirExists(t, filepath.Join(pkgDir, "files"), "a failed extraction left files behind")
	assert.False(t, s.IsExtracted("evil", "1.0.0"))
}

// An archive cut at any point (a dropped download accepted by mistake, a
// disk that filled up) never extracts as a smaller package.
func TestFaults_TruncatedArchiveNeverExtracts(t *testing.T) {
	whole := tarGz(t,
		regular("bin/app", strings.Repeat("program ", 400)),
		regular("etc/app.conf", "listen = 8080\n"),
		regular("lib/data.bin", strings.Repeat("\x00\x01\x02", 3000)),
	)
	s, pkgDir, sandbox := planted(t, whole)
	require.NoError(t, s.Extract(Package{Name: "evil", Version: "1.0.0"}))
	require.NoError(t, os.RemoveAll(filepath.Join(pkgDir, "files")))

	for cut := 0; cut < len(whole); cut += max(1, len(whole)/61) {
		require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files.tar.gz"), whole[:cut], 0o644))
		err := s.Extract(Package{Name: "evil", Version: "1.0.0"})
		if err != nil {
			requireNotExtracted(t, s, pkgDir)
			continue
		}
		// Only the checksum that ends the gzip stream may be missing: every
		// file must then be there, whole.
		require.GreaterOrEqual(t, cut, len(whole)-8, "an archive cut at byte %d of %d extracted", cut, len(whole))
		files, err := s.ExtractedFileList("evil", "1.0.0")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files.tar.gz"), whole, 0o644))
		require.NoError(t, verifyExtractedArchive(filepath.Join(pkgDir, "files.tar.gz"), files))
		require.NoError(t, os.RemoveAll(filepath.Join(pkgDir, "files")))
	}
	requireContained(t, sandbox, pkgDir)
}

// The gzip stream is whole and the tar inside it is cut: the tar's own end
// marker is missing, or an entry's body stops short of its declared size.
func TestFaults_TruncatedTarInsideAWholeGzip(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range []tarEntry{regular("bin/app", strings.Repeat("program ", 400)), regular("etc/conf", "x=1\n")} {
		h := e.hdr
		require.NoError(t, tw.WriteHeader(&h))
		_, err := tw.Write(e.body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	tarBytes := raw.Bytes()
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, err := w.Write(b)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		return buf.Bytes()
	}
	// Cuts inside the first header, inside the first body, in the padding
	// after it, and inside the second header.
	// ... and exactly between the two entries, and after the second with
	// none, half and one block of the end marker.
	const secondEntry, afterSecond = 512 + 3584, 512 + 3584 + 512 + 512
	for _, cut := range []int{100, 511, 600, 512 + 1000, 512 + 3200 + 100, secondEntry, secondEntry + 300, afterSecond, afterSecond + 256, afterSecond + 512} {
		t.Run(fmt.Sprintf("cut at %d", cut), func(t *testing.T) {
			require.Less(t, cut, len(tarBytes))
			s, pkgDir, sandbox := planted(t, gz(tarBytes[:cut]))
			require.Error(t, s.Extract(Package{Name: "evil", Version: "1.0.0"}))
			requireNotExtracted(t, s, pkgDir)
			requireContained(t, sandbox, pkgDir)
		})
	}
}

// Archives built to write somewhere else, or to hand the build something
// that is not a file of the package.
func TestFaults_HostileArchives(t *testing.T) {
	link := func(name, target string, kind byte) tarEntry {
		return tarEntry{hdr: tar.Header{Name: name, Linkname: target, Typeflag: kind, Mode: 0o777}}
	}
	special := func(name string, kind byte) tarEntry {
		return tarEntry{hdr: tar.Header{Name: name, Typeflag: kind, Mode: 0o644}}
	}
	cases := map[string][]tarEntry{
		"parent directory":                   {regular("../escaped", "x")},
		"parent through a subdirectory":      {regular("a/b/../../../escaped", "x")},
		"many parents":                       {regular(strings.Repeat("../", 12)+"tmp/escaped", "x")},
		"parent after a real file":           {regular("bin/app", "ok"), regular("../escaped", "x")},
		"only dots":                          {regular("..", "x")},
		"directory that climbs":              {{hdr: tar.Header{Name: "../outside/", Typeflag: tar.TypeDir, Mode: 0o755}}},
		"symlink out, then write through it": {link("out", "../../..", tar.TypeSymlink), regular("out/escaped", "x")},
		"symlink to an absolute path":        {link("etc", "/etc", tar.TypeSymlink), regular("etc/escaped", "x")},
		"symlink alone":                      {link("passwd", "/etc/passwd", tar.TypeSymlink)},
		"hard link to a host file":           {link("passwd", "/etc/passwd", tar.TypeLink)},
		"hard link out of the package":       {link("meta", "../meta.json", tar.TypeLink)},
		"character device":                   {special("null", tar.TypeChar)},
		"block device":                       {special("sda", tar.TypeBlock)},
		"fifo":                               {special("pipe", tar.TypeFifo)},
		"file under a file":                  {regular("bin", "a file"), regular("bin/app", "x")},
		"more data than declared budget":     {{hdr: tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1 << 40}, body: []byte("only this much")}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			s, pkgDir, sandbox := planted(t, tarGz(t, entries...))
			err := s.Extract(Package{Name: "evil", Version: "1.0.0"})
			require.Error(t, err)
			requireNotExtracted(t, s, pkgDir)
			requireContained(t, sandbox, pkgDir)
			// The refusal is stable: a second build does not get further.
			require.Error(t, s.Extract(Package{Name: "evil", Version: "1.0.0"}))
			requireNotExtracted(t, s, pkgDir)
		})
	}
}

// Entries that look hostile and are harmless once placed under the package:
// they must land inside it, never at the path they spell.
func TestFaults_RootedAndOddNamesStayInside(t *testing.T) {
	cases := []string{"/etc/passwd-planted", "//double/slash", "./dot/./file", "a//b", "trailing/dot/.", "sp ace/üñí/文件", strings.Repeat("d/", 40) + "deep"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s, pkgDir, sandbox := planted(t, tarGz(t, regular(name, "content")))
			if err := s.Extract(Package{Name: "evil", Version: "1.0.0"}); err != nil {
				requireNotExtracted(t, s, pkgDir)
			}
			requireContained(t, sandbox, pkgDir)
			assert.NoFileExists(t, "/etc/passwd-planted")
		})
	}
}

// The same path twice. The archive check that follows extraction compares
// every file with the archive, so two different contents for one name are an
// error rather than "last one wins".
func TestFaults_DuplicateEntries(t *testing.T) {
	s, pkgDir, sandbox := planted(t, tarGz(t, regular("bin/app", "first"), regular("bin/app", "second, different")))
	require.NoError(t, s.Extract(Package{Name: "evil", Version: "1.0.0"}))
	requireContained(t, sandbox, pkgDir)
	files, err := s.ExtractedFileList("evil", "1.0.0")
	require.NoError(t, err)
	require.Len(t, files, 1)
	err = verifyExtractedArchive(filepath.Join(pkgDir, "files.tar.gz"), files)
	require.ErrorContains(t, err, "collision")

	// The same content twice is one file, and fine.
	s, pkgDir, _ = planted(t, tarGz(t, regular("bin/app", "same"), regular("./bin/app", "same")))
	require.NoError(t, s.Extract(Package{Name: "evil", Version: "1.0.0"}))
	files, err = s.ExtractedFileList("evil", "1.0.0")
	require.NoError(t, err)
	require.NoError(t, verifyExtractedArchive(filepath.Join(pkgDir, "files.tar.gz"), files))
}

// Things that are not archives at all.
func TestFaults_NotAnArchive(t *testing.T) {
	var emptyGz bytes.Buffer
	w := gzip.NewWriter(&emptyGz)
	require.NoError(t, w.Close())
	var garbageGz bytes.Buffer
	w = gzip.NewWriter(&garbageGz)
	_, err := w.Write(bytes.Repeat([]byte("this is not a tar archive. "), 100))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	refused := map[string][]byte{
		"empty file":      nil,
		"zeroed":          make([]byte, 4096),
		"an error page":   []byte("<html><body>404 Not Found</body></html>"),
		"gzip magic only": {0x1f, 0x8b},
		"gzip of garbage": garbageGz.Bytes(),
		"a bare tar":      func() []byte { var b bytes.Buffer; tw := tar.NewWriter(&b); _ = tw.Close(); return b.Bytes() }(),
	}
	for name, data := range refused {
		t.Run(name, func(t *testing.T) {
			s, pkgDir, sandbox := planted(t, data)
			require.Error(t, s.Extract(Package{Name: "evil", Version: "1.0.0"}))
			requireNotExtracted(t, s, pkgDir)
			requireContained(t, sandbox, pkgDir)
		})
	}
	t.Run("gzip of nothing", func(t *testing.T) {
		// An empty archive extracts to nothing, which is not an extracted
		// package: the build fails on "no files" instead of using it.
		s, pkgDir, _ := planted(t, emptyGz.Bytes())
		_ = s.Extract(Package{Name: "evil", Version: "1.0.0"})
		assert.False(t, s.IsExtracted("evil", "1.0.0"))
		_ = pkgDir
	})
	t.Run("archive missing", func(t *testing.T) {
		s, pkgDir, _ := planted(t, nil)
		require.NoError(t, os.Remove(filepath.Join(pkgDir, "files.tar.gz")))
		require.Error(t, s.Extract(Package{Name: "evil", Version: "1.0.0"}))
		requireNotExtracted(t, s, pkgDir)
	})
}

func FuzzExtractStaysInside(f *testing.F) {
	f.Add("bin/app", "", byte(tar.TypeReg), "content")
	f.Add("../escaped", "", byte(tar.TypeReg), "x")
	f.Add("link", "../../..", byte(tar.TypeSymlink), "")
	f.Add("/abs/olute", "", byte(tar.TypeReg), "x")
	f.Add("a/../../b", "/etc/passwd", byte(tar.TypeLink), "")
	f.Add("dir/", "", byte(tar.TypeDir), "")
	f.Add(".", "", byte(tar.TypeDir), "")
	f.Add("a\x00b", "", byte(0), "x")
	f.Add(`..\..\windows`, "", byte(tar.TypeReg), "x")
	f.Fuzz(func(t *testing.T, name, linkname string, kind byte, body string) {
		if len(name) > 200 || len(body) > 4096 {
			return
		}
		first := tarEntry{hdr: tar.Header{Name: name, Linkname: linkname, Typeflag: kind, Mode: 0o644}}
		if kind == tar.TypeReg || kind == 0 {
			first.hdr.Size = int64(len(body))
			first.body = []byte(body)
		}
		// A second, ordinary-looking entry written through whatever the
		// first one created.
		s, pkgDir, sandbox := planted(t, tarGz(t, first, regular(name+"/through", "x")))
		err := s.Extract(Package{Name: "evil", Version: "1.0.0"})
		if err != nil {
			require.NoDirExists(t, filepath.Join(pkgDir, "files"), "a failed extraction left files behind")
		}
		requireContained(t, sandbox, pkgDir)
	})
}

func FuzzTarEntryName(f *testing.F) {
	for _, seed := range []string{"bin/app", "/bin/app", "../x", "a/../../x", "./a/./b", "", ".", "..", "/", "//", `a\..\b`, "a/b/", "..."} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, guest string) {
		name := tarEntryName(guest)
		if name == "" {
			return
		}
		require.False(t, strings.HasPrefix(name, "/"), "%q became rooted name %q", guest, name)
		for _, seg := range strings.Split(name, "/") {
			require.NotEqual(t, "..", seg, "%q kept a parent component in %q", guest, name)
			require.NotEqual(t, ".", seg)
			require.NotEmpty(t, seg)
		}
		require.Equal(t, name, tarEntryName(name), "normalizing twice changes the name")
	})
}

func FuzzCompareVersions(f *testing.F) {
	for _, seed := range [][2]string{{"1.0", "1.0"}, {"v20.11.0", "20.9"}, {"1.0-rc1", "1.0"}, {"", "0"}, {"1.0.0", "1.0"}, {"a", "b"}, {"1.-", "1.-0"}, {"99999999999999999999999", "1"}, {"1.0-", "1.0"}, {"v", "vv"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := CompareVersions(a, b), CompareVersions(b, a)
		require.Zero(t, CompareVersions(a, a), "%q is not equal to itself", a)
		// The order has no cycles of two: resolving "latest" sorts with it.
		require.Equal(t, sign(ab), -sign(ba), "%q vs %q: %d one way, %d the other", a, b, ab, ba)
		m := VersionMatch(a, b)
		require.Contains(t, []int{0, 1, 2}, m)
		if a != "" && a != "latest" && m == 2 {
			require.Equal(t, strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v"))
		}
	})
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func FuzzParseOpsIdentifier(f *testing.F) {
	for _, seed := range []string{"eyberg/node:20.5.0", "eyberg/node", "node", "/node", "a/b/c:1", "a/b:", "a/:1", "../../etc/passwd:1", "a/b:1:2", "a/..:..", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		id, err := ParseOpsIdentifier(s)
		if err != nil {
			require.Equal(t, OpsPackageIdentifier{}, id)
			return
		}
		require.NotEmpty(t, id.Namespace)
		require.NotContains(t, id.Name, "/")
		// Whatever was parsed, removing it from a store never removes
		// anything but a package: a file planted beside the store and the
		// store itself are still there.
		sandbox := t.TempDir()
		root := filepath.Join(sandbox, "ops")
		store, err := NewOpsStoreArch(root, "amd64")
		require.NoError(t, err)
		bystander := filepath.Join(sandbox, "bystander")
		require.NoError(t, os.WriteFile(bystander, []byte("x"), 0o644))
		_ = store.Remove(id.Namespace, id.Name, id.Version)
		require.FileExists(t, bystander, "removing %q deleted a file outside the store", s)
		require.DirExists(t, root, "removing %q removed the store", s)
	})
}

func FuzzParseDockerConfig(f *testing.F) {
	f.Add([]byte(`{"config":{"Entrypoint":["/app"],"Cmd":["--port","80"],"Env":["A=b"],"WorkingDir":"/srv","User":"1000:1000","ExposedPorts":{"80/tcp":{}}}}`))
	f.Add([]byte(`{"config":{"Entrypoint":"/app"}}`))
	f.Add([]byte(`{"config":null}`))
	f.Add([]byte(`[{"Config":{}}]`))
	f.Add([]byte(` `))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := ParseDockerConfig(data)
		if err != nil {
			require.Nil(t, cfg)
			return
		}
		require.NotNil(t, cfg)
	})
}
