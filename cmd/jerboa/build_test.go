package main

import (
	"os"
	"path/filepath"
	"testing"

	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/require"
)

func TestFindProgramBinaryExactGuestPath(t *testing.T) {
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/pkgs/openjdk-21/files/jdk-21/bin/java"), GuestPath: "jdk-21/bin/java"},
	}
	got, guest, err := findProgramBinary(pkgFiles, "jdk-21/bin/java")
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/pkgs/openjdk-21/files/jdk-21/bin/java"), got)
	require.Equal(t, "jdk-21/bin/java", guest)
}

func TestFindProgramBinarySuffixMatch(t *testing.T) {
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/pkgs/openjdk-21/files/usr/lib/jvm/openjdk-21/bin/java"), GuestPath: "usr/lib/jvm/openjdk-21/bin/java"},
	}
	got, guest, err := findProgramBinary(pkgFiles, "openjdk-21/bin/java")
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/pkgs/openjdk-21/files/usr/lib/jvm/openjdk-21/bin/java"), got)
	require.Equal(t, "usr/lib/jvm/openjdk-21/bin/java", guest)
}

func TestFindProgramBinaryBasenameLookup(t *testing.T) {
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/pkgs/openjdk-21/files/usr/lib/jvm/openjdk-21/bin/java"), GuestPath: "usr/lib/jvm/openjdk-21/bin/java"},
		{HostPath: filepath.FromSlash("/pkgs/openjdk-21/files/usr/lib/jvm/openjdk-21/bin/javac"), GuestPath: "usr/lib/jvm/openjdk-21/bin/javac"},
	}
	got, guest, err := findProgramBinary(pkgFiles, "java")
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/pkgs/openjdk-21/files/usr/lib/jvm/openjdk-21/bin/java"), got)
	require.Equal(t, "usr/lib/jvm/openjdk-21/bin/java", guest)
}

func TestFindProgramBinaryHostPathBasenameFallback(t *testing.T) {
	// GuestPath doesn't match by exact or suffix, but the HostPath basename does.
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/pkgs/openjdk-21/files/java"), GuestPath: "renamed-entry"},
	}
	got, guest, err := findProgramBinary(pkgFiles, "java")
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/pkgs/openjdk-21/files/java"), got)
	require.Equal(t, "renamed-entry", guest)
}

func TestFindProgramBinaryAbsolutePathMatchesRelativeGuestPath(t *testing.T) {
	// Package guest paths are relative (no leading slash), but users name the
	// program by its absolute in-image path. The leading slash must be normalized
	// so the absolute path still matches by exact path — and so a same-named stub
	// at the image root does NOT win the basename fallback. This is the
	// eyberg/postgresql layout: /postgres (root launcher) + the real binary under
	// /usr/local/pgsql/bin/postgres.
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/pkgs/postgresql/postgres"), GuestPath: "postgres"},
		{HostPath: filepath.FromSlash("/pkgs/postgresql/sysroot/usr/local/pgsql/bin/postgres"), GuestPath: "usr/local/pgsql/bin/postgres"},
	}
	got, guest, err := findProgramBinary(pkgFiles, "/usr/local/pgsql/bin/postgres")
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/pkgs/postgresql/sysroot/usr/local/pgsql/bin/postgres"), got)
	require.Equal(t, "usr/local/pgsql/bin/postgres", guest)
}

func TestRemapSeedFiles_Subtree(t *testing.T) {
	// Files under --src /db are rebased so /db's contents sit at the volume root
	// (mounting the volume at /db then restores them). Files outside /db and the
	// /db dir entry itself are dropped.
	files := []pkg.File{
		{HostPath: filepath.FromSlash("/pkg/db/PG_VERSION"), GuestPath: "db/PG_VERSION"},
		{HostPath: filepath.FromSlash("/pkg/db/base/1/PG_VERSION"), GuestPath: "db/base/1/PG_VERSION"},
		{GuestPath: "db", IsDir: true},
		{GuestPath: "db/pg_wal", IsDir: true},
		{HostPath: filepath.FromSlash("/pkg/usr/local/pgsql/bin/postgres"), GuestPath: "usr/local/pgsql/bin/postgres"},
	}
	got, err := remapSeedFiles(files, "/db")
	require.NoError(t, err)

	byGuest := map[string]pkg.File{}
	for _, f := range got {
		byGuest[f.GuestPath] = f
	}
	require.Contains(t, byGuest, "PG_VERSION")
	require.Contains(t, byGuest, "base/1/PG_VERSION")
	require.Contains(t, byGuest, "pg_wal")
	require.True(t, byGuest["pg_wal"].IsDir)
	// The subtree root dir and out-of-subtree files are excluded.
	require.NotContains(t, byGuest, "")
	require.NotContains(t, byGuest, "usr/local/pgsql/bin/postgres")
	require.Len(t, got, 3)
}

func TestRemapSeedFiles_WholeTree(t *testing.T) {
	files := []pkg.File{
		{HostPath: filepath.FromSlash("/pkg/a"), GuestPath: "a"},
		{HostPath: filepath.FromSlash("/pkg/sub/b"), GuestPath: "sub/b"},
	}
	got, err := remapSeedFiles(files, "/")
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestRemapSeedFiles_NoMatch(t *testing.T) {
	files := []pkg.File{
		{HostPath: filepath.FromSlash("/pkg/a"), GuestPath: "a"},
	}
	_, err := remapSeedFiles(files, "/db")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no files found under --src")
}

func TestFindProgramBinaryNotFound(t *testing.T) {
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/pkgs/node/files/bin/node"), GuestPath: "bin/node"},
	}
	_, _, err := findProgramBinary(pkgFiles, "java")
	require.Error(t, err)
	require.Contains(t, err.Error(), `program "java" not found in resolved packages (--pkg)`)
}

func TestFindProgramBinarySkipsDirEntries(t *testing.T) {
	// A directory placeholder (e.g. from [build] dirs) whose name matches the
	// program must not be resolved — it has no executable host file. The real
	// file with the same basename should win.
	pkgFiles := []pkg.File{
		{GuestPath: "var/lib/data", IsDir: true},
		{HostPath: filepath.FromSlash("/pkgs/postgres/sysroot/usr/local/bin/data"), GuestPath: "usr/local/bin/data"},
	}
	got, guest, err := findProgramBinary(pkgFiles, "data")
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/pkgs/postgres/sysroot/usr/local/bin/data"), got)
	require.Equal(t, "usr/local/bin/data", guest)
}

func TestFindProgramBinaryDirOnlyMatchFails(t *testing.T) {
	// When only a directory placeholder matches by suffix, resolution must fail
	// rather than return an empty host path.
	pkgFiles := []pkg.File{
		{GuestPath: "var/lib/postgresql/data", IsDir: true},
	}
	_, _, err := findProgramBinary(pkgFiles, "data")
	require.Error(t, err)
}

// The project's own files enter the image as build-context files, so they take
// precedence over a package file at the same guest path — the collision that
// `jerboa build examples/postgresql` hits, where the project and the
// eyberg/postgresql package each ship a README.md.
func TestSourceFilesShadowPackageFilesAtSamePath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("project docs"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('hi')\n"), 0o644))

	srcFiles, err := sourceFiles(dir)
	require.NoError(t, err)
	require.Len(t, srcFiles, 2)
	for _, f := range srcFiles {
		require.True(t, f.FromContext, "source file %q must be marked as build context", f.GuestPath)
	}

	pkgDir := t.TempDir()
	pkgReadme := filepath.Join(pkgDir, "README.md")
	pkgBinary := filepath.Join(pkgDir, "postgres")
	require.NoError(t, os.WriteFile(pkgReadme, []byte("package docs"), 0o644))
	require.NoError(t, os.WriteFile(pkgBinary, []byte("not really an ELF"), 0o755))
	merged := append([]pkg.File{
		{HostPath: pkgBinary, GuestPath: "postgres"},
		{HostPath: pkgReadme, GuestPath: "README.md"},
	}, srcFiles...)

	resolved := pkg.ApplyContextPrecedence(merged)
	require.Len(t, resolved, 3)
	for _, f := range resolved {
		require.NotEqual(t, pkgReadme, f.HostPath, "the package README must be shadowed by the project's")
	}
	require.NoError(t, pkg.ValidateFiles(resolved, "linux/amd64"))
}
