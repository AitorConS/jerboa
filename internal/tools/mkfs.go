package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/AitorConS/jerboa/internal/image"
)

// ResolveMkfs returns an image.MkfsFunc ready to invoke.
//
// Downloads the latest kernel artifacts to toolsDir on first use and caches them.
// If override is non-empty it is used as the mkfs binary path; kernel/boot still
// come from toolsDir.
func ResolveMkfs(ctx context.Context, toolsDir, override string) (image.MkfsFunc, error) {
	if !Exist(toolsDir) {
		if err := DownloadVersion(ctx, toolsDir, "latest"); err != nil {
			return nil, err
		}
	}

	mkfsPath := override
	if mkfsPath == "" {
		mkfsPath = filepath.Join(toolsDir, "mkfs")
	}
	bootImg := filepath.Join(toolsDir, "boot.img")
	kernelImg := filepath.Join(toolsDir, "kernel.img")

	// mkfs always runs on the daemon's Linux filesystem; on Windows the daemon
	// lives in WSL2 (see internal/wslboot), so no host-side WSL tunneling.
	return directFunc(mkfsPath, bootImg, kernelImg), nil
}

// directFunc returns an image.MkfsFunc that calls mkfsBin with a generated Nanos manifest on stdin.
func directFunc(mkfsBin, bootImg, kernelImg string) image.MkfsFunc {
	return func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd {
		absBin, _ := filepath.Abs(binaryPath)
		if manifest == "" {
			manifest = buildNanosManifest(absBin)
		}
		cmd := exec.CommandContext(ctx, mkfsBin,
			"-b", bootImg,
			"-k", kernelImg,
			imgPath,
		)
		cmd.Stdin = strings.NewReader(manifest)
		return cmd
	}
}

// buildNanosManifest returns a minimal Nanos manifest that packages absBinaryPath as /program.
func buildNanosManifest(absBinaryPath string) string {
	return fmt.Sprintf(
		"(\n    children:(\n        program:(contents:(host:%s))\n    )\n    program:/program\n    environment:()\n)",
		absBinaryPath,
	)
}

func downloadArtifact(ctx context.Context, url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("tools: create dir: %w", err)
	}
	name := filepath.Base(dest)
	fmt.Printf("Downloading %s...\n", name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("tools: build request: %w", err)
	}
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return fmt.Errorf("tools: download %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"tools: download %s failed (HTTP %d)\n"+
				"Build artifacts from source:\n"+
				"  cd kernel && make tools && make kernel\n"+
				"Then run: jerboa build --mkfs <path/to/mkfs>",
			name, resp.StatusCode)
	}

	checksumURL := url + ".sha256"
	expectedSHA, shaErr := fetchChecksum(ctx, checksumURL)
	if shaErr != nil {
		fmt.Printf("warning: could not verify checksum for %s: %v\n", name, shaErr)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("tools: create %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	hash := sha256.New()
	mw := io.MultiWriter(f, hash)

	size, err := io.Copy(mw, resp.Body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("tools: write %s: %w", name, err)
	}

	if expectedSHA != "" {
		got := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(got, expectedSHA) {
			_ = f.Close()
			_ = os.Remove(dest)
			gotShort := got
			wantShort := expectedSHA
			if len(gotShort) > 16 {
				gotShort = gotShort[:16]
			}
			if len(wantShort) > 16 {
				wantShort = wantShort[:16]
			}
			return fmt.Errorf("tools: checksum mismatch for %s (got %s..., want %s...)", name, gotShort, wantShort)
		}
		fmt.Printf("%s downloaded (%.1f MB) → %s [verified]\n", name, float64(size)/(1<<20), dest)
	} else {
		fmt.Printf("%s downloaded (%.1f MB) → %s\n", name, float64(size)/(1<<20), dest)
	}
	return nil
}

func fetchChecksum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build checksum request: %w", err)
	}
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch checksum: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read checksum: %w", err)
	}
	parts := strings.Fields(strings.TrimSpace(string(data)))
	if len(parts) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	return parts[0], nil
}
