package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/httpclient"
)

const (
	versionFileName = "kernel-version.txt"
	kernelTagPrefix = "kernel-"
)

var (
	githubAPIBase = "https://api.github.com/repos/AitorConS/UniCLi"
	releaseBase   = "https://github.com/AitorConS/UniCLi/releases/download"
)

// artifactNames are the files that make up the kernel toolset.
var artifactNames = []string{"mkfs-linux-amd64", "kernel.img", "boot.img", "dump-linux-amd64"}

const fcKernelArtifact = "kernel-fc.img"
const fcKernelLocalName = "kernel-fc.img"

// LocalVersion returns the semver string (e.g. "v0.1.0") cached in toolsDir.
// Returns "(unknown)" if the file is absent or unreadable.
func LocalVersion(toolsDir string) string {
	data, err := os.ReadFile(filepath.Join(toolsDir, versionFileName))
	if err != nil {
		return "(unknown)"
	}
	return strings.TrimSpace(string(data))
}

// RemoteVersion returns the semver of the latest kernel release on GitHub.
func RemoteVersion(ctx context.Context) (string, error) {
	vers, err := ListRemoteVersions(ctx)
	if err != nil {
		return "", err
	}
	if len(vers) == 0 {
		return "", fmt.Errorf("tools: no kernel releases found")
	}
	return vers[0], nil
}

// ListRemoteVersions returns all available kernel versions from GitHub releases,
// sorted newest-first by semver.
func ListRemoteVersions(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		githubAPIBase+"/releases", nil)
	if err != nil {
		return nil, fmt.Errorf("tools: build releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tools: fetch releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tools: GitHub releases API returned HTTP %d", resp.StatusCode)
	}

	var releases []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("tools: parse releases response: %w", err)
	}

	var versions []string
	for _, r := range releases {
		if strings.HasPrefix(r.TagName, kernelTagPrefix) {
			ver := "v" + strings.TrimPrefix(r.TagName, kernelTagPrefix+"v")
			versions = append(versions, ver)
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		return semverGT(versions[i], versions[j])
	})
	return versions, nil
}

// IsNewer reports whether remote is a strictly higher semver than local.
// Unknown/malformed versions are never treated as newer.
func IsNewer(local, remote string) bool {
	return semverGT(remote, local)
}

// SaveLocalVersion writes version to toolsDir/kernel-version.txt.
func SaveLocalVersion(toolsDir, version string) error {
	path := filepath.Join(toolsDir, versionFileName)
	if err := os.WriteFile(path, []byte(version+"\n"), 0o644); err != nil {
		return fmt.Errorf("tools: save kernel version: %w", err)
	}
	return nil
}

// ClearCachedTools deletes the kernel artifacts and version file from toolsDir.
func ClearCachedTools(toolsDir string) error {
	names := append([]string{versionFileName}, artifactNames...)
	names = append(names, "mkfs", "dump") // local names differ from remote artifact names
	for _, name := range names {
		if err := os.Remove(filepath.Join(toolsDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("tools: clear %s: %w", name, err)
		}
	}
	return nil
}

// Exist returns true when all three kernel artifacts are present in toolsDir.
func Exist(toolsDir string) bool {
	for _, name := range []string{"mkfs", "kernel.img", "boot.img"} {
		if _, err := os.Stat(filepath.Join(toolsDir, name)); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// ArtifactURL returns the download URL for a named artifact at a given semver.
// version must be in "vMAJOR.MINOR.PATCH" form; artifact is e.g. "kernel.img".
// Passing version="latest" downloads from the rolling latest release.
func ArtifactURL(version, artifact string) string {
	if version == "latest" {
		return fmt.Sprintf("%s/latest/%s", releaseBase, artifact)
	}
	ver := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("%s/%sv%s/%s", releaseBase, kernelTagPrefix, ver, artifact)
}

// DownloadVersion downloads all kernel artifacts for the given semver into
// toolsDir and saves the version file. Use version="latest" for the rolling release.
func DownloadVersion(ctx context.Context, toolsDir, version string) error {
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return fmt.Errorf("tools: create tools dir: %w", err)
	}
	artifacts := []struct{ remote, local string }{
		{"mkfs-linux-amd64", "mkfs"},
		{"kernel.img", "kernel.img"},
		{"boot.img", "boot.img"},
	}
	for _, a := range artifacts {
		dest := filepath.Join(toolsDir, a.local)
		if err := downloadArtifact(ctx, ArtifactURL(version, a.remote), dest); err != nil {
			return fmt.Errorf("tools: download %s: %w", a.remote, err)
		}
	}
	// dump is optional — older releases may not include it.
	if err := downloadArtifact(ctx, ArtifactURL(version, "dump-linux-amd64"), filepath.Join(toolsDir, "dump")); err != nil {
		fmt.Printf("warning: dump tool not available in this release: %v\n", err)
	}
	// Resolve the real semver when downloading "latest".
	resolved := version
	if version == "latest" {
		if ver, err := RemoteVersion(ctx); err == nil {
			resolved = ver
		}
	}
	return SaveLocalVersion(toolsDir, resolved)
}

// FCKernelPath returns the path where the Firecracker-compatible kernel is cached.
func FCKernelPath(toolsDir string) string {
	return filepath.Join(toolsDir, fcKernelLocalName)
}

// FCKernelExists returns true when the Firecracker kernel is present in toolsDir.
func FCKernelExists(toolsDir string) bool {
	_, err := os.Stat(FCKernelPath(toolsDir))
	return err == nil
}

// EnsureFCKernel downloads kernel-fc.img from the latest release into toolsDir
// if it is not already present. Returns the local path to the kernel.
func EnsureFCKernel(ctx context.Context, toolsDir string) (string, error) {
	dest := FCKernelPath(toolsDir)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return "", fmt.Errorf("tools: create tools dir: %w", err)
	}
	url := ArtifactURL("latest", fcKernelArtifact)
	if err := downloadArtifact(ctx, url, dest); err != nil {
		return "", fmt.Errorf("tools: download %s: %w", fcKernelArtifact, err)
	}
	return dest, nil
}

// semverGT returns true when a is strictly greater than b.
// Both strings may have a leading "v". Malformed versions are treated as "0.0.0".
func semverGT(a, b string) bool {
	av := parseSemver(a)
	bv := parseSemver(b)
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

func parseSemver(s string) [3]int {
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, ".", 3)
	var out [3]int
	for i, p := range parts {
		if i >= 3 {
			break
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out
}
