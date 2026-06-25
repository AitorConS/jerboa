package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AitorConS/jerboa/internal/wslboot"
	"github.com/AitorConS/jerboa/internal/wsldistro"
	"github.com/spf13/cobra"
)

// distroArch is the architecture of the jerboad binary that runs inside the
// distro. The distro ships as an amd64 rootfs (wsldistro.RootfsArtifact), so its
// daemon binary is linux/amd64 regardless of the Windows host architecture.
const distroArch = "amd64"

// hostCLIArch is the architecture of the Windows jerboa.exe published in
// releases. Only windows/amd64 is built, so the host CLI is always amd64.
const hostCLIArch = "amd64"

// runUpgradeWSL upgrades jerboa.exe on the host and the jerboad binary inside the
// dedicated WSL2 distro to the latest release. The daemon is updated in place —
// only its binary is swapped, so images, volumes, and other distro data survive
// (unlike `jerboa daemon install --force`, which re-imports the whole rootfs).
func runUpgradeWSL(ctx context.Context, cmd *cobra.Command, endpoint string, yes, verbose bool) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	installed, err := wsldistro.Exists()
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("upgrade: the %q WSL2 distro is not installed — run: jerboa daemon install", wsldistro.Name)
	}

	// 1. Check versions.
	remote, err := latestCLIVersion(ctx)
	if err != nil {
		return fmt.Errorf("upgrade: check latest version: %w", err)
	}
	daemonVer := queryDaemonVersion(endpoint)

	fmt.Fprintf(out, "Installed CLI:  %s\n", version)
	if daemonVer != "" {
		fmt.Fprintf(out, "Running daemon: %s\n", daemonVer)
	} else {
		fmt.Fprintln(out, "Running daemon: not running")
	}
	fmt.Fprintf(out, "Latest:         %s\n", remote)

	cliOutdated := cliIsNewer(version, remote)
	daemonOutdated := daemonVer != "" && cliIsNewer(daemonVer, remote)
	if !cliOutdated && !daemonOutdated {
		fmt.Fprintln(out, "Already up to date.")
		return nil
	}
	fmt.Fprintf(out, "New version available: %s\n", remote)
	if !yes && !confirmPrompt("Upgrade? [y/N] ") {
		fmt.Fprintln(out, "Aborted.")
		return nil
	}

	// 2. Locate the directory holding the running jerboa.exe.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("upgrade: locate binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("upgrade: resolve symlink: %w", err)
	}
	dir := filepath.Dir(exe)

	sp := newSpinner(errOut, verbose)

	// 3. Download both binaries before touching anything: the Windows CLI for the
	// host and the linux daemon for the distro.
	sp.Start("Downloading jerboa " + remote)
	jerboaTmp, err := downloadBinaryPlatform(ctx, dir, "jerboa", "windows", hostCLIArch, remote)
	if err != nil {
		sp.Fail("Download failed")
		return fmt.Errorf("upgrade: download jerboa: %w", err)
	}
	sp.Done("Downloaded jerboa " + remote)
	defer func() { _ = os.Remove(jerboaTmp) }()

	sp.Start("Downloading jerboad " + remote)
	jerboadTmp, err := downloadBinaryPlatform(ctx, dir, "jerboad", "linux", distroArch, remote)
	if err != nil {
		sp.Fail("Download failed")
		return fmt.Errorf("upgrade: download jerboad: %w", err)
	}
	sp.Done("Downloaded jerboad " + remote)
	defer func() { _ = os.Remove(jerboadTmp) }()

	// 4. Stop the daemon so its binary is not busy (ETXTBSY) when replaced.
	if err := wslboot.Stop(wsldistro.Name, daemonLaunchUser); err != nil && !errors.Is(err, wslboot.ErrNoDaemon) {
		return fmt.Errorf("upgrade: stop daemon: %w", err)
	}

	// 5. Replace the host CLI.
	if err := installBinary(jerboaTmp, exe); err != nil {
		return fmt.Errorf("upgrade: install jerboa: %w", err)
	}
	fmt.Fprintf(out, "jerboa  → %s\n", exe)

	// 6. Replace the daemon binary inside the distro.
	sp.Start("Installing jerboad into the distro")
	if err := wsldistro.InstallDaemonBinary(jerboadTmp); err != nil {
		sp.Fail("Install failed")
		return fmt.Errorf("upgrade: %w", err)
	}
	sp.Done("Installed jerboad into the distro")
	fmt.Fprintf(out, "jerboad → %s:%s\n", wsldistro.Name, wsldistro.DaemonBinaryPath)

	cleanupBackups(dir)

	// 7. Restart the daemon on the new binary.
	wcfg, token, err := resolveDaemonConfig(daemonOpts{})
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	sp.Start("Starting new jerboad")
	if err := launchAndWait(cmd, wcfg, token); err != nil {
		sp.Fail("Daemon did not become ready")
		fmt.Fprintf(errOut, "warning: %v\n", err)
		fmt.Fprintln(errOut, "Start it manually: jerboa daemon start")
	} else {
		sp.Done("Daemon restarted and ready")
	}

	fmt.Fprintf(out, "Upgraded to %s.\n", remote)
	return nil
}
