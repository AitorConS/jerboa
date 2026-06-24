package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/spf13/cobra"
)

const (
	cliGithubAPIBase = "https://api.github.com/repos/AitorConS/jerboa"
	cliReleaseBase   = "https://github.com/AitorConS/jerboa/releases/download"

	daemonReadyTimeout = 15 * time.Second
	daemonStopTimeout  = 10 * time.Second
	daemonPollInterval = 100 * time.Millisecond
)

func newUpgradeCmd(socketPath *string, verbose *bool) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade jerboa and jerboad to the latest version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			return runUpgrade(ctx, cmd, *socketPath, yes, *verbose)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.AddCommand(newUpgradeCheckCmd(socketPath), newUpgradeListCmd())
	return cmd
}

func runUpgrade(ctx context.Context, cmd *cobra.Command, socketPath string, yes bool, verbose bool) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// 1. Check versions.
	remote, err := latestCLIVersion(ctx)
	if err != nil {
		return fmt.Errorf("upgrade: check latest version: %w", err)
	}
	daemonVer := queryDaemonVersion(socketPath)

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

	// 2. Locate the directory holding the running jerboa binary.
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

	// 3. Download both binaries to temp files before touching anything on disk.
	sp.Start("Downloading jerboa " + remote)
	jerboaTmp, err := downloadBinary(ctx, dir, "jerboa", remote)
	if err != nil {
		sp.Fail("Download failed")
		return fmt.Errorf("upgrade: download jerboa: %w", err)
	}
	sp.Done("Downloaded jerboa " + remote)
	defer func() { _ = os.Remove(jerboaTmp) }()

	sp.Start("Downloading jerboad " + remote)
	jerboadTmp, err := downloadBinary(ctx, dir, "jerboad", remote)
	if err != nil {
		sp.Fail("Download failed")
		return fmt.Errorf("upgrade: download jerboad: %w", err)
	}
	sp.Done("Downloaded jerboad " + remote)
	defer func() { _ = os.Remove(jerboadTmp) }()

	// 4. Stop the daemon gracefully if it is running.
	daemonWasRunning := stopDaemon(ctx, socketPath, out, errOut)

	// 5. Atomically replace both binaries.
	jerboaDest := exe
	jerboadDest := filepath.Join(dir, binaryName("jerboad"))

	if err := installBinary(jerboaTmp, jerboaDest); err != nil {
		return fmt.Errorf("upgrade: install jerboa: %w", err)
	}
	fmt.Fprintf(out, "jerboa  → %s\n", jerboaDest)

	if err := installBinary(jerboadTmp, jerboadDest); err != nil {
		return fmt.Errorf("upgrade: install jerboad: %w", err)
	}
	fmt.Fprintf(out, "jerboad → %s\n", jerboadDest)

	// 6. Clean up old .bak files now that the old processes have exited.
	cleanupBackups(dir)

	// 7. Restart daemon if it was running before.
	if daemonWasRunning {
		sp.Start("Starting new jerboad")
		if err := launchDaemon(jerboadDest, socketPath); err != nil {
			sp.Fail("Could not start jerboad")
			fmt.Fprintf(errOut, "warning: start jerboad: %v\n", err)
			fmt.Fprintln(errOut, "Start jerboad manually: jerboad --socket "+socketPath)
		} else if err := waitForSocket(socketPath, daemonReadyTimeout); err != nil {
			sp.Fail("Daemon did not become ready")
			fmt.Fprintf(errOut, "warning: jerboad did not become ready: %v\n", err)
		} else {
			sp.Done("Daemon restarted and ready")
		}
	}

	fmt.Fprintf(out, "Upgraded to %s.\n", remote)
	return nil
}

// queryDaemonVersion dials the daemon and returns its version string, or "" if
// the daemon is not reachable.
func queryDaemonVersion(socketPath string) string {
	client, err := api.Dial(socketPath)
	if err != nil {
		return ""
	}
	defer func() { _ = client.Close() }()
	ver, err := client.DaemonVersion(context.Background())
	if err != nil {
		return ""
	}
	return ver
}

// stopDaemon shuts the daemon down via RPC and waits for its socket to disappear.
// Returns true if the daemon was running.
func stopDaemon(ctx context.Context, socketPath string, out, errOut io.Writer) bool {
	client, err := api.Dial(socketPath)
	if err != nil {
		return false // daemon not running
	}
	fmt.Fprintln(out, "Stopping daemon...")
	_ = client.Shutdown(ctx) // ignore error — daemon may close conn before responding
	_ = client.Close()

	deadline := time.Now().Add(daemonStopTimeout)
	for time.Now().Before(deadline) {
		if _, err := net.Dial("unix", socketPath); err != nil { //nolint:noctx // polling loop has no request context
			return true // socket gone — daemon exited
		}
		time.Sleep(daemonPollInterval)
	}
	fmt.Fprintln(errOut, "warning: daemon did not exit within timeout; replacing binary anyway")
	return true
}

// waitForSocket polls until the Unix socket at path accepts connections or the
// timeout elapses.
func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath) //nolint:noctx // polling loop has no request context
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(daemonPollInterval)
	}
	return fmt.Errorf("socket %s not ready after %s", socketPath, timeout)
}

// launchDaemon starts a new jerboad process detached from the current terminal.
func launchDaemon(jerboadBin, socketPath string) error {
	cmd := exec.Command(jerboadBin, "--socket", socketPath) //nolint:noctx // daemon outlives the CLI process; no context to pass
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", jerboadBin, err)
	}
	// Detach — we don't wait for it.
	go func() { _ = cmd.Wait() }()
	return nil
}

// downloadBinary downloads the named binary for the current platform into a
// temp file in dir and returns its path.
func downloadBinary(ctx context.Context, dir, name, ver string) (string, error) {
	artifact := fmt.Sprintf("%s-%s-%s%s", name, runtime.GOOS, runtime.GOARCH, binaryExt())
	url := fmt.Sprintf("%s/%s/%s", cliReleaseBase, ver, artifact)

	tmp, err := os.CreateTemp(dir, name+"-upgrade-*"+binaryExt())
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	hash := sha256.New()
	mw := io.MultiWriter(tmp, hash)

	if err := downloadToVerified(ctx, url, mw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}

	expectedSHA, shaErr := fetchUpgradeChecksum(ctx, url+".sha256")
	if shaErr != nil {
		fmt.Printf("warning: could not verify checksum for %s: %v\n", name, shaErr)
	} else {
		got := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(got, expectedSHA) {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			gotShort := got
			wantShort := expectedSHA
			if len(gotShort) > 16 {
				gotShort = gotShort[:16]
			}
			if len(wantShort) > 16 {
				wantShort = wantShort[:16]
			}
			return "", fmt.Errorf("upgrade: checksum mismatch for %s (got %s..., want %s...)", name, gotShort, wantShort)
		}
	}

	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close temp: %w", err)
	}
	return tmpPath, nil
}

// installBinary atomically replaces dest with the file at src.
//
// On Unix, os.Rename is atomic within the same filesystem.
// On Windows, a running exe cannot be overwritten directly; we rename it to
// a .bak first (which works even while the process is open), then place the
// new binary. Old .bak files are removed by cleanupBackups after the upgrade
// finishes, when the previous process has already exited.
func installBinary(src, dest string) error {
	if runtime.GOOS == "windows" {
		return windowsReplaceFile(src, dest)
	}
	if err := os.Rename(src, dest); err != nil {
		return fmt.Errorf("install %s: %w", dest, err)
	}
	return nil
}

// cleanupBackups removes all .bak files in dir. It is called after the new
// binaries are in place and the old processes have exited, so the .bak files
// are no longer locked.
func cleanupBackups(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bak") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func windowsReplaceFile(src, dest string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer srcFile.Close()

	destFile, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("open dest %s: %w", dest, err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dest, err)
	}
	if err := destFile.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dest, err)
	}
	_ = os.Remove(src)
	return nil
}

func binaryName(name string) string { return name + binaryExt() }
func binaryExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// ── subcommands ────────────────────────────────────────────────────────────

func newUpgradeCheckCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check whether a newer CLI version is available",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			out := cmd.OutOrStdout()
			remote, err := latestCLIVersion(ctx)
			if err != nil {
				fmt.Fprintf(out, "Latest: (unavailable — %v)\n", err)
				return nil
			}
			daemonVer := queryDaemonVersion(*socketPath)
			fmt.Fprintf(out, "Installed CLI:  %s\n", version)
			if daemonVer != "" {
				fmt.Fprintf(out, "Running daemon: %s\n", daemonVer)
			} else {
				fmt.Fprintln(out, "Running daemon: not running")
			}
			fmt.Fprintf(out, "Latest:         %s\n", remote)
			cliOutdated := cliIsNewer(version, remote)
			daemonOutdated := daemonVer != "" && cliIsNewer(daemonVer, remote)
			if cliOutdated || daemonOutdated {
				fmt.Fprintf(out, "Update available. Run `jerboa upgrade` to install %s.\n", remote)
			} else {
				fmt.Fprintln(out, "Already up to date.")
			}
			return nil
		},
	}
}

func newUpgradeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all available CLI versions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			versions, err := listCLIVersions(ctx)
			if err != nil {
				return fmt.Errorf("upgrade list: %w", err)
			}
			if len(versions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No releases found.")
				return nil
			}
			for _, v := range versions {
				marker := "  "
				if v == version {
					marker = "* "
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s%s\n", marker, v)
			}
			return nil
		},
	}
}

// ── GitHub release helpers ─────────────────────────────────────────────────

func latestCLIVersion(ctx context.Context) (string, error) {
	vers, err := listCLIVersions(ctx)
	if err != nil {
		return "", err
	}
	if len(vers) == 0 {
		return "", fmt.Errorf("no CLI releases found")
	}
	return vers[0], nil
}

func listCLIVersions(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cliGithubAPIBase+"/releases", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	var releases []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	var versions []string
	for _, r := range releases {
		tag := r.TagName
		if strings.HasPrefix(tag, "v") && !strings.HasPrefix(tag, "vkernel") {
			versions = append(versions, tag)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return cliSemverGT(versions[i], versions[j]) })
	return versions, nil
}

func downloadToVerified(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP %d for %s", resp.StatusCode, url)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func fetchUpgradeChecksum(ctx context.Context, url string) (string, error) {
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

func cliIsNewer(local, remote string) bool { return cliSemverGT(remote, local) }

func cliSemverGT(a, b string) bool {
	av, bv := cliParseSemver(a), cliParseSemver(b)
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

func cliParseSemver(s string) [3]int {
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
