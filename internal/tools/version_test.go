package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"v0.1.0", [3]int{0, 1, 0}},
		{"0.1.0", [3]int{0, 1, 0}},
		{"v1.2.3", [3]int{1, 2, 3}},
		{"1.2.3", [3]int{1, 2, 3}},
		{"v10.20.30", [3]int{10, 20, 30}},
		{"", [3]int{0, 0, 0}},
		{"v", [3]int{0, 0, 0}},
		{"1", [3]int{1, 0, 0}},
		{"1.2", [3]int{1, 2, 0}},
		{"notsemver", [3]int{0, 0, 0}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseSemver(tt.input)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSemverGT(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"greater patch", "v0.1.1", "v0.1.0", true},
		{"greater minor", "v0.2.0", "v0.1.9", true},
		{"greater major", "v1.0.0", "v0.99.99", true},
		{"equal", "v0.1.0", "v0.1.0", false},
		{"less than", "v0.1.0", "v0.2.0", false},
		{"without v prefix", "0.2.0", "0.1.0", true},
		{"mixed prefixes", "v0.2.0", "0.1.0", true},
		{"malformed a", "bad", "v0.1.0", false},
		{"malformed b", "v0.1.0", "bad", true},
		{"both malformed", "bad", "bad", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := semverGT(tt.a, tt.b)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsNewer(t *testing.T) {
	require.True(t, IsNewer("v0.1.0", "v0.2.0"))
	require.False(t, IsNewer("v0.2.0", "v0.1.0"))
	require.False(t, IsNewer("v0.1.0", "v0.1.0"))
}

func TestExist(t *testing.T) {
	t.Run("all artifacts present", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"mkfs", "kernel.img", "boot.img"} {
			f, err := os.Create(filepath.Join(dir, name))
			require.NoError(t, err)
			f.Close()
		}
		require.True(t, Exist(dir))
	})

	t.Run("missing kernel.img", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"mkfs", "boot.img"} {
			f, err := os.Create(filepath.Join(dir, name))
			require.NoError(t, err)
			f.Close()
		}
		require.False(t, Exist(dir))
	})

	t.Run("empty directory", func(t *testing.T) {
		dir := t.TempDir()
		require.False(t, Exist(dir))
	})
}

func TestLocalVersion(t *testing.T) {
	t.Run("file present", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, versionFileName), []byte("v0.1.2\n"), 0o644))
		require.Equal(t, "v0.1.2", LocalVersion(dir))
	})

	t.Run("file absent", func(t *testing.T) {
		dir := t.TempDir()
		require.Equal(t, "(unknown)", LocalVersion(dir))
	})
}

func TestSaveLocalVersion(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveLocalVersion(dir, "v0.1.2"))
	got, err := os.ReadFile(filepath.Join(dir, versionFileName))
	require.NoError(t, err)
	require.Equal(t, "v0.1.2\n", string(got))
}

func TestClearCachedTools(t *testing.T) {
	dir := t.TempDir()
	allFiles := append([]string{versionFileName}, artifactNames...)
	allFiles = append(allFiles, "mkfs", "dump", fcKernelLocalName)
	for _, name := range allFiles {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}
	require.NoError(t, ClearCachedTools(dir))
	for _, name := range allFiles {
		_, err := os.Stat(filepath.Join(dir, name))
		require.True(t, os.IsNotExist(err), "expected %s to be deleted", name)
	}
}

func TestClearCachedTools_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ClearCachedTools(dir))
}

// TestHasLocalVersion is the F-006 regression: an unmarked tools dir (a distro-
// baked toolchain) reports no known version, so callers avoid the false "update
// available" prompt; a marked dir reports its version.
func TestHasLocalVersion(t *testing.T) {
	dir := t.TempDir()
	if HasLocalVersion(dir) {
		t.Fatal("empty tools dir should have no known version")
	}
	if got := LocalVersion(dir); got != UnknownVersion {
		t.Fatalf("LocalVersion on empty dir = %q, want %q", got, UnknownVersion)
	}
	if err := SaveLocalVersion(dir, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	if !HasLocalVersion(dir) {
		t.Fatal("after SaveLocalVersion the version should be known")
	}
	if got := LocalVersion(dir); got != "v0.2.0" {
		t.Fatalf("LocalVersion = %q, want v0.2.0", got)
	}
}
