package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/builder"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/require"
)

func writeTestFile(t *testing.T, dir, rel string, n int) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o644))
	return p
}

func TestFindRuntimeBinaryReturnsPackageGuestPath(t *testing.T) {
	dir := t.TempDir()
	node := writeTestFile(t, dir, "files/node", 10)
	libc := writeTestFile(t, dir, "files/sysroot/lib/libc.so.6", 10)
	files := []pkg.File{
		{GuestPath: "data", IsDir: true},
		{HostPath: libc, GuestPath: "lib/libc.so.6"},
		{HostPath: node, GuestPath: "node"},
	}
	host, guest, err := findRuntimeBinary(files, builder.LangNode)
	require.NoError(t, err)
	require.Equal(t, node, host)
	require.Equal(t, "node", guest)
}

func TestFindRuntimeBinaryVersionedGlobKeepsGuestPath(t *testing.T) {
	dir := t.TempDir()
	py := writeTestFile(t, dir, "files/usr/bin/python3.12", 10)
	files := []pkg.File{{HostPath: py, GuestPath: "/usr/bin/python3.12"}}
	host, guest, err := findRuntimeBinary(files, builder.LangPython)
	require.NoError(t, err)
	require.Equal(t, py, host)
	require.Equal(t, "usr/bin/python3.12", guest)
}

func TestFindRuntimeBinaryIgnoresWorkingDirectoryForDirEntries(t *testing.T) {
	// A directory placeholder has no host path. Its "directory" must not turn
	// into the process working directory, where a stray python3 could match.
	cwd := t.TempDir()
	writeTestFile(t, cwd, "python3", 10)
	t.Chdir(cwd)
	_, _, err := findRuntimeBinary([]pkg.File{{GuestPath: "data", IsDir: true}}, builder.LangPython)
	require.Error(t, err)
}

func tarEntries(t *testing.T, r io.Reader) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out[hdr.Name] = hdr.Size
	}
}

func TestBuildContextReaderStreamsRuntimeOnce(t *testing.T) {
	dir := t.TempDir()
	node := writeTestFile(t, dir, "files/node", 4096)
	app := writeTestFile(t, dir, "src/app.js", 10)
	files := []pkg.File{
		{HostPath: node, GuestPath: "node"},
		{HostPath: app, GuestPath: "app.js", FromContext: true},
	}
	pr := buildContextReader(node, "node", files)
	defer func() { _ = pr.Close() }()
	got := tarEntries(t, pr)
	require.Equal(t, map[string]int64{buildProgramPath: 4096, "app.js": 10}, got)
}

func TestBuildContextReaderKeepsCopyAtAnotherPath(t *testing.T) {
	// The same binary at a path other than the program's is a real second file
	// in the image and must still be sent.
	dir := t.TempDir()
	node := writeTestFile(t, dir, "files/node", 4096)
	files := []pkg.File{{HostPath: node, GuestPath: "node"}}
	pr := buildContextReader(node, "", files)
	defer func() { _ = pr.Close() }()
	got := tarEntries(t, pr)
	require.Equal(t, map[string]int64{buildProgramPath: 4096, "node": 4096}, got)
}

func TestBuildSizeReportCountsSecondRuntimeCopy(t *testing.T) {
	dir := t.TempDir()
	node := writeTestFile(t, dir, "files/node", 5000)
	files := []pkg.File{{HostPath: node, GuestPath: "node"}}

	// Program placed at /program while the package keeps /node: two copies.
	r, err := buildSizeReport(node, "", files)
	require.NoError(t, err)
	require.Equal(t, int64(10000), r.TotalBytes)

	// Program run from the package path: one copy.
	r, err = buildSizeReport(node, "/node", files)
	require.NoError(t, err)
	require.Equal(t, int64(5000), r.TotalBytes)
	require.Equal(t, 1, r.TotalFiles)
}
