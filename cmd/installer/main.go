//go:build windows

// Jerboa Windows installer — enables WSL2, imports the jerboa distro, and
// installs the jerboa CLI into %LOCALAPPDATA%\Programs\Jerboa\jerboa.exe.
//
// Must run as Administrator (auto-elevates via ShellExecuteW "runas" if not).
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	githubBase = "https://github.com/AitorConS/jerboa/releases/download/latest"
	distroName = "jerboa"
	distroTar  = "jerboa-rootfs-amd64.tar.gz"
	cliBinary  = "jerboa-windows-amd64.exe"
	installExe = "jerboa.exe"

	swShowNormal = uintptr(1)
)

var (
	modShell32        = syscall.NewLazyDLL("shell32.dll")
	procIsUserAnAdmin = modShell32.NewProc("IsUserAnAdmin")
	procShellExecuteW = modShell32.NewProc("ShellExecuteW")
)

func main() {
	fmt.Println("Jerboa Installer")
	fmt.Println("================")
	fmt.Println()

	if !isAdmin() {
		fmt.Println("Requesting administrator privileges...")
		if err := relaunchAsAdmin(); err != nil {
			die("Failed to elevate: %v\nPlease right-click and run as Administrator.", err)
		}
		os.Exit(0)
	}

	step(1, 6, "Checking system requirements")
	checkRequirements()

	step(2, 6, "Installing WSL2")
	installWSL()

	tmpDir, err := os.MkdirTemp("", "jerboa-install-*")
	must(err, "create temp dir")
	defer os.RemoveAll(tmpDir)

	step(3, 6, "Downloading Jerboa distro")
	rootfsDst := filepath.Join(tmpDir, distroTar)
	download(githubBase+"/"+distroTar, rootfsDst)

	step(4, 6, "Importing Jerboa WSL2 distro")
	importDistro(rootfsDst)

	step(5, 6, "Downloading Jerboa CLI")
	cliBinaryDst := filepath.Join(tmpDir, cliBinary)
	download(githubBase+"/"+cliBinary, cliBinaryDst)
	installDir := installCLI(cliBinaryDst)

	step(6, 6, "Adding Jerboa to PATH")
	addToPath(installDir)

	fmt.Println()
	fmt.Println("Installation complete!")
	fmt.Printf("Open a new terminal and run: jerboa --version\n")
	fmt.Println()
	pause()
}

// ── steps ────────────────────────────────────────────────────────────────────

func checkRequirements() {
	if strings.ToLower(os.Getenv("PROCESSOR_ARCHITECTURE")) == "arm64" {
		die("ARM64 Windows is not yet supported.")
	}
	ok("x86_64 architecture detected")
}

func installWSL() {
	// Check if WSL is already functional.
	out, err := exec.Command("wsl", "--status").CombinedOutput()
	if err == nil && !strings.Contains(string(out), "not installed") {
		ok("WSL2 already installed")
		return
	}

	fmt.Println("  Installing WSL2 (this may take a few minutes)...")
	cmd := exec.Command("wsl", "--install", "--no-distribution")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Exit code 1 with a reboot-required message is acceptable.
		if strings.Contains(fmt.Sprint(err), "1") {
			fmt.Println()
			fmt.Println("  WSL2 installed. A system restart may be required.")
			fmt.Println("  Please reboot and run this installer again to complete setup.")
			pause()
			os.Exit(0)
		}
		die("WSL2 installation failed: %v", err)
	}
	ok("WSL2 installed")

	// Ensure WSL2 is the default version.
	run("wsl", "--set-default-version", "2")
}

func importDistro(rootfsTar string) {
	// Skip if distro already exists.
	if distroExists() {
		ok("Distro '%s' already imported (skipping)", distroName)
		return
	}

	distroDir := distroInstallDir()
	if err := os.MkdirAll(distroDir, 0o700); err != nil {
		die("Create distro dir: %v", err)
	}

	fmt.Printf("  Importing distro '%s'...\n", distroName)
	run("wsl", "--import", distroName, distroDir, rootfsTar, "--version", "2")
	ok("Distro '%s' imported", distroName)
}

func installCLI(src string) string {
	installDir := cliInstallDir()
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		die("Create install dir: %v", err)
	}
	dst := filepath.Join(installDir, installExe)
	if err := copyFile(src, dst); err != nil {
		die("Install CLI: %v", err)
	}
	ok("Installed to %s", dst)
	return installDir
}

func addToPath(dir string) {
	// Read current user PATH from registry via PowerShell.
	psGet := `[Environment]::GetEnvironmentVariable('PATH', 'User')`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", psGet).Output()
	if err != nil {
		die("Read user PATH: %v", err)
	}
	current := strings.TrimSpace(string(out))

	// Check if already present (case-insensitive).
	lDir := strings.ToLower(dir)
	for p := range strings.SplitSeq(current, ";") {
		if strings.ToLower(strings.TrimSpace(p)) == lDir {
			ok("Already in PATH")
			return
		}
	}

	newPath := current
	if newPath != "" && !strings.HasSuffix(newPath, ";") {
		newPath += ";"
	}
	newPath += dir

	psSet := fmt.Sprintf(`[Environment]::SetEnvironmentVariable('PATH', %s, 'User')`,
		strconv.Quote(newPath))
	if err := exec.Command("powershell", "-NoProfile", "-Command", psSet).Run(); err != nil {
		die("Update PATH: %v", err)
	}
	ok("Added to user PATH")
}

// ── download ─────────────────────────────────────────────────────────────────

func download(url, dst string) {
	fmt.Printf("  %s\n", url)
	resp, err := http.Get(url) //nolint:noctx // installer; short-lived
	if err != nil {
		die("HTTP GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		die("HTTP %d from %s", resp.StatusCode, url)
	}

	f, err := os.Create(dst)
	if err != nil {
		die("Create %s: %v", dst, err)
	}
	defer f.Close()

	total := resp.ContentLength
	pw := &progressWriter{total: total}
	if _, err := io.Copy(f, io.TeeReader(resp.Body, pw)); err != nil {
		die("Download %s: %v", url, err)
	}
	fmt.Println()
	ok("Downloaded %s", filepath.Base(dst))
}

type progressWriter struct {
	written int64
	total   int64
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	pw.written += int64(len(p))
	if pw.total > 0 {
		pct := pw.written * 100 / pw.total
		fmt.Printf("\r  %s / %s (%d%%)",
			humanBytes(pw.written), humanBytes(pw.total), pct)
	} else {
		fmt.Printf("\r  %s", humanBytes(pw.written))
	}
	return len(p), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n := n / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ── helpers ───────────────────────────────────────────────────────────────────

func distroInstallDir() string {
	local, _ := os.UserCacheDir()
	return filepath.Join(local, "Jerboa", "distro")
}

func cliInstallDir() string {
	local, _ := localAppData()
	return filepath.Join(local, "Programs", "Jerboa")
}

func localAppData() (string, error) {
	p := os.Getenv("LOCALAPPDATA")
	if p == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, "AppData", "Local")
	}
	return p, nil
}

func distroExists() bool {
	out, err := exec.Command("wsl", "--list", "--quiet").Output()
	if err != nil {
		return false
	}
	// WSL outputs UTF-16-LE; decode manually or just check for the ASCII bytes.
	// In practice, wsl --list --quiet on modern Windows returns UTF-8 in some
	// versions and UTF-16-LE in others. Convert to string and strip NUL bytes.
	s := strings.ReplaceAll(string(out), "\x00", "")
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) == distroName {
			return true
		}
	}
	return false
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...) //nolint:gosec // controlled args
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		die("Command %q failed: %v", name+" "+strings.Join(args, " "), err)
	}
}

func must(err error, msg string) {
	if err != nil {
		die("%s: %v", msg, err)
	}
}

func step(n, total int, label string) {
	fmt.Printf("[%d/%d] %s...\n", n, total, label)
}

func ok(format string, args ...any) {
	fmt.Printf("  ✓ "+format+"\n", args...)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\nERROR: "+format+"\n", args...)
	pause()
	os.Exit(1)
}

func pause() {
	fmt.Print("Press Enter to exit...")
	bufio.NewReader(os.Stdin).ReadString('\n') //nolint:errcheck // best-effort
}

// ── Windows elevation ─────────────────────────────────────────────────────────

func isAdmin() bool {
	ret, _, _ := procIsUserAnAdmin.Call()
	return ret != 0
}

func relaunchAsAdmin() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exeW, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	verbW, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	ret, _, lastErr := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verbW)),
		uintptr(unsafe.Pointer(exeW)),
		0,
		0,
		swShowNormal,
	)
	// ShellExecuteW returns a value > 32 on success.
	if ret <= 32 {
		return fmt.Errorf("ShellExecuteW returned %d: %w", ret, lastErr)
	}
	return nil
}
