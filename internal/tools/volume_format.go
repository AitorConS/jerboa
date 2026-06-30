package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AitorConS/jerboa/internal/volume"
)

// ResolveVolumeFormatter returns a volume.Formatter that formats a raw disk as
// an empty, labeled TFS filesystem using the mkfs toolchain. Like ResolveMkfs
// it downloads the toolchain to toolsDir on first use; if override is non-empty
// it is used as the mkfs binary path.
//
// mkfs runs on the daemon's Linux filesystem (on Windows the daemon lives in
// WSL2), so the disk path passed to the returned Formatter must already be a
// daemon-visible path.
func ResolveVolumeFormatter(ctx context.Context, toolsDir, override string) (volume.Formatter, error) {
	if !Exist(toolsDir) {
		if err := DownloadVersion(ctx, toolsDir, "latest"); err != nil {
			return nil, err
		}
	}
	mkfsPath := override
	if mkfsPath == "" {
		mkfsPath = filepath.Join(toolsDir, "mkfs")
	}
	return func(ctx context.Context, diskPath, label string, sizeBytes int64) error {
		// mkfs -e -l <label> -s <size> <disk>: an empty (no boot/kernel) TFS
		// volume of at least sizeBytes, labeled so the guest kernel's
		// volume_match can bind it to a mount point. No manifest on stdin is
		// needed for an empty filesystem.
		args := []string{"-e", "-l", label}
		if sizeBytes > 0 {
			args = append(args, "-s", strconv.FormatInt(sizeBytes, 10))
		}
		args = append(args, diskPath)
		cmd := exec.CommandContext(ctx, mkfsPath, args...)
		cmd.Stderr = os.Stderr
		if out, err := cmd.Output(); err != nil {
			return fmt.Errorf("mkfs format %s: %w (output: %s)", diskPath, err, string(out))
		}
		return nil
	}, nil
}

// ResolveVolumeSeeder returns a volume.Seeder that formats a raw disk as a
// labeled TFS filesystem populated from a Nanos manifest, using the mkfs
// toolchain. Like ResolveVolumeFormatter it downloads the toolchain to toolsDir
// on first use; if override is non-empty it is used as the mkfs binary path.
//
// mkfs runs on the daemon's Linux filesystem (on Windows the daemon lives in
// WSL2), so the disk path passed to the returned Seeder must already be a
// daemon-visible path.
func ResolveVolumeSeeder(ctx context.Context, toolsDir, override string) (volume.Seeder, error) {
	if !Exist(toolsDir) {
		if err := DownloadVersion(ctx, toolsDir, "latest"); err != nil {
			return nil, err
		}
	}
	mkfsPath := override
	if mkfsPath == "" {
		mkfsPath = filepath.Join(toolsDir, "mkfs")
	}
	return func(ctx context.Context, diskPath, label string, sizeBytes int64, manifest string) error {
		// mkfs -l <label> -s <size> <disk> < manifest: a TFS volume populated
		// from the manifest's children tree. No -b/-k (boot/kernel) is passed, so
		// the result is a pure, non-bootable data volume — labeled so the guest
		// kernel's volume_match can bind it to a mount point. The manifest is read
		// from stdin, exactly as in an image build.
		args := []string{"-l", label}
		if sizeBytes > 0 {
			args = append(args, "-s", strconv.FormatInt(sizeBytes, 10))
		}
		args = append(args, diskPath)
		cmd := exec.CommandContext(ctx, mkfsPath, args...)
		cmd.Stdin = strings.NewReader(manifest)
		cmd.Stderr = os.Stderr
		if out, err := cmd.Output(); err != nil {
			return fmt.Errorf("mkfs seed %s: %w (output: %s)", diskPath, err, string(out))
		}
		return nil
	}, nil
}
