package main

import (
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

func TestFindProgramBinaryNotFound(t *testing.T) {
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/pkgs/node/files/bin/node"), GuestPath: "bin/node"},
	}
	_, _, err := findProgramBinary(pkgFiles, "java")
	require.Error(t, err)
	require.Contains(t, err.Error(), `program "java" not found in resolved packages (--pkg)`)
}
