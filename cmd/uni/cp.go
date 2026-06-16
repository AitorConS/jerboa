package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/AitorConS/unikernel-engine/internal/tools"
	"github.com/spf13/cobra"
)

func newCpCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy files to or from a stopped VM",
		Long: `Copy files to or from a stopped VM disk image.

Requires the 'dump' and 'mkfs' tools from the kernel release. These are
downloaded automatically on first use.

Copying FROM a VM extracts the entire filesystem and copies the requested file.
Copying TO a VM extracts the filesystem, adds the file, and rebuilds the image.

The VM must be stopped before using cp.

Examples:
  uni cp myvm:/etc/config.json ./config.json
  uni cp ./config.json myvm:/etc/config.json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, dst := args[0], args[1]
			fromVM, srcID, srcVMPath := parseCpSpec(src)
			toVM, dstID, dstVMPath := parseCpSpec(dst)

			if fromVM && toVM {
				return fmt.Errorf("cp: cannot copy between two VMs")
			}
			if !fromVM && !toVM {
				return fmt.Errorf("cp: at least one operand must be a VM reference (id:path)")
			}

			client, err := api.Dial(*socketPath)
			if err != nil {
				return fmt.Errorf("cp: connect to daemon: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: close client: %v\n", closeErr)
				}
			}()

			vmID := ""
			vmPath := ""
			localPath := ""
			if fromVM {
				vmID = srcID
				vmPath = srcVMPath
				localPath = dst
			}
			if toVM {
				vmID = dstID
				vmPath = dstVMPath
				localPath = src
			}

			detail, err := client.Inspect(cmd.Context(), vmID)
			if err != nil {
				return fmt.Errorf("cp: %w", err)
			}
			if detail.State != "stopped" {
				return fmt.Errorf("cp: VM %s is %s; cp only works on stopped VMs", vmID, detail.State)
			}

			dumpBin, err := tools.ResolveDump(cmd.Context(), defaultToolsPath(), "")
			if err != nil {
				return fmt.Errorf("cp: resolve dump tool: %w", err)
			}

			if toVM {
				return cpToVM(cmd, dumpBin, detail.Image, localPath, vmPath)
			}
			return cpFromVM(cmd, dumpBin, detail.Image, vmPath, localPath)
		},
	}
}

// cpFromVM extracts a file from a stopped VM disk image.
func cpFromVM(cmd *cobra.Command, dumpBin, imagePath, vmPath, localPath string) error {
	tmpDir, err := os.MkdirTemp("", "uni-cp-from-*")
	if err != nil {
		return fmt.Errorf("cp: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	execCmd := exec.Command(dumpBin, "-f", imagePath, "-o", tmpDir)
	if out, err := execCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp: dump failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	srcFile := filepath.Join(tmpDir, filepath.FromSlash(vmPath))
	info, err := os.Stat(srcFile)
	if err != nil {
		return fmt.Errorf("cp: file not found in VM image: %s", vmPath)
	}

	if info.IsDir() {
		return fmt.Errorf("cp: %s is a directory; only files are supported", vmPath)
	}

	if err := copyFile(srcFile, localPath); err != nil {
		return fmt.Errorf("cp: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "copied :%s → %s\n", vmPath, localPath)
	return nil
}

// cpToVM copies a local file into a stopped VM disk image by extracting the
// filesystem, adding the file, and rebuilding the image.
func cpToVM(cmd *cobra.Command, dumpBin, imagePath, localPath, vmPath string) error {
	localInfo, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("cp: stat local file: %w", err)
	}
	if localInfo.IsDir() {
		return fmt.Errorf("cp: directories are not supported; copy individual files")
	}

	tmpDir, err := os.MkdirTemp("", "uni-cp-to-*")
	if err != nil {
		return fmt.Errorf("cp: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	execCmd := exec.Command(dumpBin, "-f", imagePath, "-o", tmpDir)
	if out, err := execCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp: dump failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	dstDir := filepath.Join(tmpDir, filepath.FromSlash(filepath.Dir(vmPath)))
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("cp: create directory in VM image: %w", err)
	}

	dstFile := filepath.Join(tmpDir, filepath.FromSlash(vmPath))
	if err := copyFile(localPath, dstFile); err != nil {
		return fmt.Errorf("cp: copy into VM: %w", err)
	}

	backupPath := imagePath + ".bak"
	if err := os.Rename(imagePath, backupPath); err != nil {
		return fmt.Errorf("cp: backup original image: %w", err)
	}
	defer func() {
		if _, err := os.Stat(imagePath); err != nil {
			_ = os.Rename(backupPath, imagePath)
		}
		_ = os.Remove(backupPath)
	}()

	mkfsRun, err := tools.ResolveMkfs(cmd.Context(), defaultToolsPath(), "")
	if err != nil {
		return fmt.Errorf("cp: resolve mkfs: %w", err)
	}

	programPath := findProgram(tmpDir)
	if programPath == "" {
		return fmt.Errorf("cp: could not find /program in VM image")
	}

	manifest := buildRebuildManifest(tmpDir)
	rebuildCmd := mkfsRun(cmd.Context(), imagePath, programPath, manifest)
	if out, err := rebuildCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp: rebuild image: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	fmt.Fprintf(cmd.OutOrStdout(), "copied %s → :%s\n", localPath, vmPath)
	return nil
}

// findProgram locates the /program binary in the dumped filesystem.
// The program path is stored at the root of the TFS dump.
func findProgram(rootDir string) string {
	programPath := filepath.Join(rootDir, "program")
	if _, err := os.Stat(programPath); err == nil {
		return programPath
	}
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && e.Type().IsRegular() {
			return filepath.Join(rootDir, e.Name())
		}
	}
	return ""
}

// buildRebuildManifest constructs a Nanos manifest that includes the original
// program and any additional files from the dumped filesystem.
// The manifest format is: (children:(child_name:(contents:(host:/absolute/path)))...)
func buildRebuildManifest(rootDir string) string {
	var children []string
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return fmt.Sprintf("(children:(program:(contents:(host:%s)))program:/program)", filepath.Join(rootDir, "program"))
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		abs, _ := filepath.Abs(filepath.Join(rootDir, e.Name()))
		children = append(children, fmt.Sprintf("%s:(contents:(host:%s))", e.Name(), abs))
	}
	manifest := "(children:(" + strings.Join(children, " ") + ")program:/program)"
	return manifest
}

// parseCpSpec parses a string like "id:/path" into (isVM, id, path).
// For non-VM paths it returns (false, "", s).
// Single-letter prefixes (e.g. "C:\...") are Windows drive letters, not VM IDs.
func parseCpSpec(s string) (bool, string, string) {
	if idx := strings.Index(s, ":"); idx > 0 {
		prefix := s[:idx]
		if len(prefix) == 1 && (prefix[0] >= 'A' && prefix[0] <= 'Z' || prefix[0] >= 'a' && prefix[0] <= 'z') {
			return false, "", s
		}
		return true, prefix, s[idx+1:]
	}
	return false, "", s
}

// copyFile copies a regular file from src to dst, preserving permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy data: %w", err)
	}
	return nil
}
