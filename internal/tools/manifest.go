package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/AitorConS/jerboa/internal/release"
)

// kernelFileLocalNames maps the manifest's kernel file keys to their on-disk
// names in the tools dir (mkfs/dump drop their -linux-amd64 suffix).
var kernelFileLocalNames = map[string]string{
	"kernel.img":    "kernel.img",
	"boot.img":      "boot.img",
	"kernel-fc.img": fcKernelLocalName,
	"mkfs":          "mkfs",
	"dump":          "dump",
}

// optionalKernelFiles are absent from older releases and must not fail a fetch.
var optionalKernelFiles = map[string]bool{"dump": true, "kernel-fc.img": true}

// EnsureKernelTools makes sure the kernel toolset (mkfs, boot.img, kernel.img,
// plus kernel-fc.img/dump when published) is present in toolsDir. When any of
// the core artifacts is missing it downloads the full set named by the signed
// release manifest, each file SHA-256 verified. R2 is the single source of
// truth — there is no GitHub fallback.
func EnsureKernelTools(ctx context.Context, toolsDir string) error {
	if Exist(toolsDir) {
		if runtime.GOOS == "darwin" {
			return ValidateNativeTools(toolsDir)
		}
		return executableTools(toolsDir)
	}
	cl, err := release.Default()
	if err != nil {
		return fmt.Errorf("tools: release client: %w", err)
	}
	k, err := KernelComponentFromManifest(ctx, cl, release.ChannelStable)
	if err != nil {
		return err
	}
	return DownloadKernelFromManifest(ctx, cl, toolsDir, k)
}

// ensureFCKernelFromManifest downloads the Firecracker kernel (kernel-fc.img)
// named by the signed stable manifest into dest, SHA-256 verified. It returns an
// error whenever no manifest is reachable, the client cannot be built, or the
// component omits the file.
func ensureFCKernelFromManifest(ctx context.Context, dest string) error {
	cl, err := release.Default()
	if err != nil {
		return fmt.Errorf("tools: release client: %w", err)
	}
	k, err := KernelComponentFromManifest(ctx, cl, release.ChannelStable)
	if err != nil {
		return err
	}
	a, ok := k.Files["kernel-fc.img"]
	if !ok {
		return fmt.Errorf("tools: manifest kernel component has no kernel-fc.img")
	}
	if err := cl.DownloadArtifact(ctx, a, dest); err != nil {
		return fmt.Errorf("tools: download kernel-fc.img: %w", err)
	}
	return nil
}

// KernelComponentFromManifest fetches and verifies the signed channel manifest
// and returns its kernel component. Manifest errors abort the download.
func KernelComponentFromManifest(ctx context.Context, cl *release.Client, channel string) (release.Component, error) {
	m, err := cl.FetchManifest(ctx, channel)
	if err != nil {
		return release.Component{}, err
	}
	k, ok := m.Component(release.ComponentKernel)
	if !ok {
		return release.Component{}, fmt.Errorf("tools: manifest has no kernel component")
	}
	return k, nil
}

// DownloadKernelFromManifest downloads every kernel file named by the (already
// signature-verified) manifest component into toolsDir and records the version.
// Each artifact is SHA-256 verified against the signed manifest.
func DownloadKernelFromManifest(ctx context.Context, cl *release.Client, toolsDir string, k release.Component) error {
	if runtime.GOOS == "darwin" {
		return fmt.Errorf("macOS ARM64 kernel toolsets must be built locally; the published kernel files target Linux/x86_64")
	}
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return fmt.Errorf("tools: create tools dir: %w", err)
	}

	// Resolve the download plan up front so a manifest missing a required file
	// fails before we touch disk.
	type kernelFile struct {
		local string
		asset release.Asset
	}
	var plan []kernelFile
	for key, local := range kernelFileLocalNames {
		a, ok := k.Files[key]
		if !ok {
			if optionalKernelFiles[key] {
				continue
			}
			return fmt.Errorf("tools: kernel manifest missing required file %q", key)
		}
		plan = append(plan, kernelFile{local: local, asset: a})
	}

	// Download into a staging dir on the same filesystem, then promote only once
	// every artifact is verified, so a mid-download failure never leaves a
	// half-updated toolset with a stale version marker.
	staging, err := os.MkdirTemp(toolsDir, ".staging-*")
	if err != nil {
		return fmt.Errorf("tools: create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	for _, f := range plan {
		if err := cl.DownloadArtifact(ctx, f.asset, filepath.Join(staging, f.local)); err != nil {
			return fmt.Errorf("tools: download kernel %s: %w", f.local, err)
		}
	}
	if err := executableTools(staging); err != nil {
		return err
	}
	for _, f := range plan {
		dst := filepath.Join(toolsDir, f.local)
		_ = os.Remove(dst) // Windows rename fails when dst already exists.
		if err := os.Rename(filepath.Join(staging, f.local), dst); err != nil {
			return fmt.Errorf("tools: install kernel %s: %w", f.local, err)
		}
	}
	return SaveLocalVersion(toolsDir, k.Version)
}

func executableTools(dir string) error {
	for _, name := range []string{"mkfs", "dump"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); os.IsNotExist(err) && name == "dump" {
			continue
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return fmt.Errorf("tools: executable %s: %w", name, err)
		}
	}
	return nil
}
