package wsldistro

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetNestedVirtualization(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{
			name:        "empty file appends section",
			in:          "",
			want:        "[wsl2]\nnestedVirtualization=true\n",
			wantChanged: true,
		},
		{
			name:        "already true is a no-op",
			in:          "[wsl2]\nnestedVirtualization=true\n",
			want:        "[wsl2]\nnestedVirtualization=true\n",
			wantChanged: false,
		},
		{
			name:        "case-insensitive true is a no-op",
			in:          "[wsl2]\nnestedVirtualization = True\n",
			want:        "[wsl2]\nnestedVirtualization = True\n",
			wantChanged: false,
		},
		{
			name:        "flips false to true",
			in:          "[wsl2]\nnestedVirtualization=false\n",
			want:        "[wsl2]\nnestedVirtualization=true\n",
			wantChanged: true,
		},
		{
			name:        "inserts key into existing wsl2 section preserving others",
			in:          "[wsl2]\nmemory=4GB\n",
			want:        "[wsl2]\nnestedVirtualization=true\nmemory=4GB\n",
			wantChanged: true,
		},
		{
			name:        "appends wsl2 section after another section",
			in:          "[experimental]\nsparseVhd=true\n",
			want:        "[experimental]\nsparseVhd=true\n[wsl2]\nnestedVirtualization=true\n",
			wantChanged: true,
		},
		{
			name:        "only touches wsl2, not a like-named key elsewhere",
			in:          "[other]\nnestedVirtualization=false\n\n[wsl2]\nmemory=2GB\n",
			want:        "[other]\nnestedVirtualization=false\n\n[wsl2]\nnestedVirtualization=true\nmemory=2GB\n",
			wantChanged: true,
		},
		{
			name:        "appends newline before new section when missing trailing newline",
			in:          "[experimental]\nsparseVhd=true",
			want:        "[experimental]\nsparseVhd=true\n[wsl2]\nnestedVirtualization=true\n",
			wantChanged: true,
		},
		{
			name:        "recognizes wsl2 header behind a UTF-8 BOM",
			in:          bom + "[wsl2]\nnestedVirtualization=false\n",
			want:        "[wsl2]\nnestedVirtualization=true\n",
			wantChanged: true,
		},
		{
			name:        "already-true behind a BOM is a no-op",
			in:          bom + "[wsl2]\nnestedVirtualization=true\n",
			want:        bom + "[wsl2]\nnestedVirtualization=true\n",
			wantChanged: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := setNestedVirtualization(tc.in)
			require.Equal(t, tc.wantChanged, changed)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestEnsureNestedVirtualization_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)

	changed, path, err := EnsureNestedVirtualization()
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, filepath.Join(dir, ".wslconfig"), path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "[wsl2]\nnestedVirtualization=true\n", string(data))

	// Second run is idempotent.
	changed, _, err = EnsureNestedVirtualization()
	require.NoError(t, err)
	require.False(t, changed)
}

func TestEnsureNestedVirtualization_UpdatesExisting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)

	path := filepath.Join(dir, ".wslconfig")
	require.NoError(t, os.WriteFile(path, []byte("[wsl2]\nnestedVirtualization=false\nmemory=4GB\n"), 0o600))

	changed, got, err := EnsureNestedVirtualization()
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, path, got)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// Flips the key to true while preserving unrelated settings.
	require.Equal(t, "[wsl2]\nnestedVirtualization=true\nmemory=4GB\n", string(data))

	// Re-running on the now-correct file is a no-op.
	changed, _, err = EnsureNestedVirtualization()
	require.NoError(t, err)
	require.False(t, changed)
}
