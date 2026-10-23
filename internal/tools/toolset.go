package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/AitorConS/jerboa/internal/release"
)

// versionsDirName holds mkfs binaries for toolset versions other than the
// installed one, one directory per version: <toolsDir>/versions/<v>/mkfs.
const versionsDirName = "versions"

// MkfsForVersion returns an mkfs binary for the kernel toolset version an
// image recipe names. The installed toolset's mkfs is used when its version
// matches; otherwise that version's mkfs for this host is downloaded once from
// its signed toolset document and kept beside the installed toolset.
func MkfsForVersion(ctx context.Context, toolsDir, version string) (string, error) {
	return mkfsForVersion(ctx, toolsDir, version, release.Default)
}

func mkfsForVersion(ctx context.Context, toolsDir, version string, client func() (*release.Client, error)) (string, error) {
	v, err := release.NormalizeToolsetVersion(version)
	if err != nil {
		return "", fmt.Errorf("tools: %w", err)
	}
	if err := release.CheckToolsetVersion(v); err != nil {
		return "", fmt.Errorf("tools: %w", err)
	}
	if HasLocalVersion(toolsDir) && sameToolsetVersion(LocalVersion(toolsDir), v) {
		installed := filepath.Join(toolsDir, "mkfs")
		if isExecutableFile(installed) {
			return installed, nil
		}
	}
	dir := filepath.Join(toolsDir, versionsDirName, v)
	cached := filepath.Join(dir, "mkfs")
	if isExecutableFile(cached) {
		return cached, nil
	}

	cl, err := client()
	if err != nil {
		return "", fmt.Errorf("tools: release client: %w", err)
	}
	ts, err := cl.FetchToolset(ctx, v)
	if err != nil {
		return "", fmt.Errorf("tools: toolset %s: %w", v, err)
	}
	// The daemon runs mkfs: on Windows that is inside WSL2, a Linux host.
	a, err := ts.MkfsAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", fmt.Errorf("tools: %w", err)
	}
	// Only the daemon's user may replace the binaries it runs.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("tools: create %s: %w", dir, err)
	}
	if err := cl.DownloadArtifact(ctx, a, cached); err != nil {
		return "", fmt.Errorf("tools: download mkfs %s: %w", v, err)
	}
	if err := os.Chmod(cached, 0o755); err != nil {
		return "", fmt.Errorf("tools: chmod mkfs %s: %w", v, err)
	}
	return cached, nil
}

// sameToolsetVersion compares versions with or without a leading "v".
func sameToolsetVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
