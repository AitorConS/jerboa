package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/httpclient"
	"github.com/AitorConS/unikernel-engine/internal/image"
)

// ResolveMkfs returns an image.MkfsFunc ready to invoke.
//
// Downloads the latest kernel artifacts to toolsDir on first use and caches them.
// If override is non-empty it is used as the mkfs binary path; kernel/boot still
// come from toolsDir. On Windows all three binaries are invoked through WSL2.
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

	if runtime.GOOS == "windows" {
		return wslFunc(mkfsPath, bootImg, kernelImg)
	}
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
				"Then run: uni build --mkfs <path/to/mkfs>",
			name, resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("tools: create %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	size, err := io.Copy(f, resp.Body)
	if err != nil {
		return fmt.Errorf("tools: write %s: %w", name, err)
	}
	fmt.Printf("%s downloaded (%.1f MB) → %s\n", name, float64(size)/(1<<20), dest)
	return nil
}
