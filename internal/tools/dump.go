package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AitorConS/jerboa/internal/release"
)

// ResolveDump returns the path to the dump binary, downloading the kernel
// toolset (which includes dump) from the signed release manifest if it is not
// already present in toolsDir. If override is non-empty it is returned as-is
// without any checks.
func ResolveDump(ctx context.Context, toolsDir, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	// The manifest download writes the tool under its local name "dump".
	dumpPath := filepath.Join(toolsDir, "dump")
	if _, err := os.Stat(dumpPath); err == nil {
		return dumpPath, nil
	}

	cl, err := release.Default()
	if err != nil {
		return "", fmt.Errorf("tools: release client: %w", err)
	}
	k, err := KernelComponentFromManifest(ctx, cl, release.ChannelStable)
	if err != nil {
		return "", fmt.Errorf("tools: resolve dump: %w", err)
	}
	a, ok := k.Files["dump"]
	if !ok {
		return "", fmt.Errorf("tools: manifest kernel component has no dump tool")
	}
	// The toolset download leaves dump out; fetch just this file.
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return "", fmt.Errorf("tools: create tools dir: %w", err)
	}
	if err := cl.DownloadArtifact(ctx, a, dumpPath); err != nil {
		return "", fmt.Errorf("tools: download dump: %w", err)
	}
	if err := os.Chmod(dumpPath, 0o755); err != nil {
		return "", fmt.Errorf("tools: download dump: %w", err)
	}
	if _, err := os.Stat(dumpPath); err != nil {
		return "", fmt.Errorf("tools: dump missing after download: %w", err)
	}
	return dumpPath, nil
}
