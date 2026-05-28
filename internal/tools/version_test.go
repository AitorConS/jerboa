package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestArtifactURL(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		artifact string
		want     string
	}{
		{
			"specific version",
			"v0.1.2",
			"kernel.img",
			"https://github.com/AitorConS/UniCLi/releases/download/kernel-v0.1.2/kernel.img",
		},
		{
			"latest version",
			"latest",
			"mkfs-linux-amd64",
			"https://github.com/AitorConS/UniCLi/releases/download/latest/mkfs-linux-amd64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ArtifactURL(tt.version, tt.artifact)
			require.Equal(t, tt.want, got)
		})
	}
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
	allFiles = append(allFiles, "mkfs", "dump")
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

func TestListRemoteVersions(t *testing.T) {
	releases := []struct {
		TagName string `json:"tag_name"`
	}{
		{"kernel-v0.1.2"},
		{"kernel-v0.1.0"},
		{"kernel-v0.2.0"},
		{"cli-v0.2.0"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(releases)
		w.Write(data)
	}))
	defer srv.Close()

	origAPI := githubAPIBase
	githubAPIBase = srv.URL + "/repos/AitorConS/UniCLi"
	defer func() { githubAPIBase = origAPI }()

	vers, err := ListRemoteVersions(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"v0.2.0", "v0.1.2", "v0.1.0"}, vers)
}

func TestListRemoteVersions_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	origAPI := githubAPIBase
	githubAPIBase = srv.URL + "/repos/AitorConS/UniCLi"
	defer func() { githubAPIBase = origAPI }()

	_, err := ListRemoteVersions(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 500")
}

func TestRemoteVersion(t *testing.T) {
	releases := []struct {
		TagName string `json:"tag_name"`
	}{
		{"kernel-v0.3.0"},
		{"kernel-v0.1.0"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(releases)
		w.Write(data)
	}))
	defer srv.Close()

	origAPI := githubAPIBase
	githubAPIBase = srv.URL + "/repos/AitorConS/UniCLi"
	defer func() { githubAPIBase = origAPI }()

	ver, err := RemoteVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.3.0", ver)
}

func TestRemoteVersion_NoReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	origAPI := githubAPIBase
	githubAPIBase = srv.URL + "/repos/AitorConS/UniCLi"
	defer func() { githubAPIBase = origAPI }()

	_, err := RemoteVersion(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no kernel releases found")
}

func TestDownloadVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("fake-artifact-content"))
	}))
	defer srv.Close()

	origRelease := releaseBase
	releaseBase = srv.URL
	defer func() { releaseBase = origRelease }()

	origAPI := githubAPIBase
	githubAPIBase = fmt.Sprintf("%s/repos/AitorConS/UniCLi", srv.URL)
	defer func() { githubAPIBase = origAPI }()

	dir := t.TempDir()
	require.NoError(t, DownloadVersion(context.Background(), dir, "v0.1.0"))

	for _, name := range []string{"mkfs", "kernel.img", "boot.img"} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, "expected %s to exist", name)
	}

	got := LocalVersion(dir)
	require.Equal(t, "v0.1.0", got)
}
