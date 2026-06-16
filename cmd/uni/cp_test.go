package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCpSpec(t *testing.T) {
	tests := []struct {
		input  string
		isVM   bool
		vmID   string
		vmPath string
	}{
		{"abc123:/etc/config.json", true, "abc123", "/etc/config.json"},
		{"myvm:/var/data.txt", true, "myvm", "/var/data.txt"},
		{"/local/path/file.txt", false, "", "/local/path/file.txt"},
		{"./relative.txt", false, "", "./relative.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			isVM, id, path := parseCpSpec(tt.input)
			require.Equal(t, tt.isVM, isVM)
			require.Equal(t, tt.vmID, id)
			require.Equal(t, tt.vmPath, path)
		})
	}
}

func TestParseCpSpec_WindowsDrive(t *testing.T) {
	// Windows drive letters (single char before ':') must not be treated as VM IDs.
	tests := []struct {
		input string
	}{
		{`C:\Users\file.txt`},
		{`c:/Users/file.txt`},
		{`D:\data\file.txt`},
	}
	for _, tt := range tests {
		isVM, _, _ := parseCpSpec(tt.input)
		require.False(t, isVM, "drive letter path %q should not be parsed as a VM reference", tt.input)
	}
}

func TestCopyFile(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := []byte("hello world copy test")
	srcFile := filepath.Join(srcDir, "source.txt")
	require.NoError(t, os.WriteFile(srcFile, content, 0o644))

	dstFile := filepath.Join(dstDir, "dest.txt")
	require.NoError(t, copyFile(srcFile, dstFile))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestCopyFile_PreservesPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("permission bits behave differently on Windows")
	}

	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcFile := filepath.Join(srcDir, "exec.bin")
	require.NoError(t, os.WriteFile(srcFile, []byte("binary"), 0o755))

	dstFile := filepath.Join(dstDir, "exec.bin")
	require.NoError(t, copyFile(srcFile, dstFile))

	info, err := os.Stat(dstFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestCopyFile_SourceNotFound(t *testing.T) {
	dstDir := t.TempDir()
	err := copyFile("/nonexistent/file.txt", filepath.Join(dstDir, "out.txt"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "open source")
}

func TestFindProgram(t *testing.T) {
	t.Run("program exists", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "program"), []byte("elf"), 0o755))
		got := findProgram(dir)
		require.Equal(t, filepath.Join(dir, "program"), got)
	})

	t.Run("no program but regular file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "app"), []byte("elf"), 0o755))
		got := findProgram(dir)
		require.NotEmpty(t, got)
	})

	t.Run("empty dir", func(t *testing.T) {
		dir := t.TempDir()
		got := findProgram(dir)
		require.Empty(t, got)
	})
}

func TestBuildRebuildManifest(t *testing.T) {
	t.Run("with files", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "program"), []byte("x"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644))

		got := buildRebuildManifest(dir)
		require.Contains(t, got, "(children:(")
		require.Contains(t, got, "program:(contents:(host:")
		require.Contains(t, got, "config.json:(contents:(host:")
		require.Contains(t, got, ")program:/program)")
	})

	t.Run("empty dir", func(t *testing.T) {
		dir := t.TempDir()
		got := buildRebuildManifest(dir)
		require.Contains(t, got, "program:/program")
	})
}

func TestCp_BothVMs(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()
	msg := execRootExpectError(t, socketPath, storePath, "cp", "vm1:/a", "vm2:/b")
	require.Contains(t, msg, "cannot copy between two VMs")
}

func TestCp_NoVM(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()
	msg := execRootExpectError(t, socketPath, storePath, "cp", "/local/a", "/local/b")
	require.Contains(t, msg, "at least one operand must be a VM reference")
}

func TestCp_VMNotFound(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	localFile := filepath.Join(t.TempDir(), "test.txt")
	require.NoError(t, os.WriteFile(localFile, []byte("data"), 0o644))

	msg := execRootExpectError(t, socketPath, storePath, "cp", localFile, "nonexistent:/etc/test.txt")
	require.Contains(t, msg, "cp")
}

func TestCpFromVM_DumpFails(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "disk.img")
	require.NoError(t, os.WriteFile(imagePath, []byte("fake"), 0o600))

	root := newRootCmd()
	var buf strings.Builder
	root.SetOut(&buf)
	root.SetErr(&buf)

	err := cpFromVM(root, "/nonexistent/dump", imagePath, "/etc/hosts", filepath.Join(dir, "out.txt"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "dump failed")
}

func TestCpToVM_LocalFileNotFound(t *testing.T) {
	root := newRootCmd()
	var buf strings.Builder
	root.SetOut(&buf)
	root.SetErr(&buf)

	err := cpToVM(root, "/nonexistent/dump", "disk.img", "/nonexistent/local.txt", "/etc/hosts")
	require.Error(t, err)
	require.Contains(t, err.Error(), "stat local file")
}

func TestCpToVM_DirectoryNotSupported(t *testing.T) {
	dir := t.TempDir()
	localDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(localDir, 0o755))

	root := newRootCmd()
	var buf strings.Builder
	root.SetOut(&buf)
	root.SetErr(&buf)

	err := cpToVM(root, "/nonexistent/dump", "disk.img", localDir, "/etc/hosts")
	require.Error(t, err)
	require.Contains(t, err.Error(), "directories are not supported")
}
