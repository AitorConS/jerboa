package tools

import (
	"context"
	"debug/elf"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/AitorConS/jerboa/internal/image"
)

// ResolveMkfs returns an image.MkfsFunc ready to invoke.
//
// Downloads the latest kernel artifacts to toolsDir on first use and caches them.
// If override is non-empty it is used as the mkfs binary path; kernel/boot still
// come from toolsDir.
func ResolveMkfs(ctx context.Context, toolsDir, override string) (image.MkfsFunc, error) {
	if err := EnsureKernelTools(ctx, toolsDir); err != nil {
		return nil, err
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
		selectedBoot, selectedKernel := bootImg, kernelImg
		if runtime.GOOS == "darwin" {
			if f, err := elf.Open(binaryPath); err == nil {
				if f.Machine == elf.EM_X86_64 {
					selectedBoot = filepath.Join(filepath.Dir(bootImg), "x86", "boot.img")
					selectedKernel = filepath.Join(filepath.Dir(kernelImg), "x86", "kernel.img")
				}
				f.Close()
			}
		}
		cmd := exec.CommandContext(ctx, mkfsBin,
			"-b", selectedBoot,
			"-k", selectedKernel,
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
