package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/wslboot"
	"github.com/spf13/cobra"
)

const nativeService = "dev.jerboa.daemon"

func nativeDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func nativeDaemonCommand(cmd *cobra.Command, action string, opts daemonOpts) error {
	override := ""
	if flag := cmd.Flag("host"); flag != nil && flag.Changed {
		override = flag.Value.String()
	}
	if override == "" {
		if flag := cmd.Flag("socket"); flag != nil && flag.Changed {
			override = flag.Value.String()
		}
	}
	endpoint := config.ResolveEndpoint(override)
	// Managed launchd instances always use this user's private local socket.
	if endpoint != config.DefaultEndpoint() {
		return fmt.Errorf("native daemon lifecycle requires the default local socket; unset JERBOA_HOST and daemon.endpoint")
	}
	if action == "stop" || action == "restart" {
		// Only stop the named launchd service; never signal a PID from a stale file.
		if exec.CommandContext(cmd.Context(), "/bin/launchctl", "print", nativeDomain()+"/"+nativeService).Run() == nil {
			if out, err := exec.CommandContext(cmd.Context(), "/bin/launchctl", "bootout", nativeDomain()+"/"+nativeService).CombinedOutput(); err != nil {
				return fmt.Errorf("stop native daemon: %w: %s", err, out)
			}
		}
		if action == "stop" {
			return nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".jerboa")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}

	observabilityArgs := ""
	dirty := false
	for _, f := range []struct {
		name, value string
		dst         *string
	}{
		{"--metrics-addr", opts.metricsAddr, &cfg.Daemon.MetricsAddr},
		{"--ui-addr", opts.uiAddr, &cfg.Daemon.UIAddr},
		{"--trace-addr", opts.traceAddr, &cfg.Daemon.TraceAddr},
		{"--log-format", opts.logFormat, &cfg.Daemon.LogFormat},
	} {
		if f.value != "" && f.value != *f.dst {
			*f.dst = f.value
			dirty = true
		}
		if *f.dst != "" {
			observabilityArgs += "<string>" + f.name + "</string><string>" + xmlText(*f.dst) + "</string>"
		}
	}
	if dirty {
		if err := config.Save(config.DefaultPath(), cfg); err != nil {
			return err
		}
	}

	token := config.ResolveToken()
	if token == "" {
		token, err = wslboot.LoadOrCreateToken(daemonJSONPath())
		if err != nil {
			return err
		}
	}
	if wslboot.Healthy(cmd.Context(), endpoint, token) {
		return nil
	}
	bin := cfg.Daemon.JerboadPath
	if bin == "" {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		bin = filepath.Join(filepath.Dir(self), "jerboad")
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return err
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("native jerboad missing: %w", err)
	}
	hypervisor := opts.hypervisor
	if hypervisor == "" {
		hypervisor = cfg.Hypervisor
	}
	if hypervisor == "" {
		hypervisor = "qemu"
	}
	hypervisorArgs, err := nativeHypervisorArgs(hypervisor, filepath.Dir(bin))
	if err != nil {
		return err
	}
	toolsArgs := ""
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "tools", "platform.txt")); err == nil {
		toolsArgs = "<string>--tools-dir</string><string>" + xmlText(filepath.Join(filepath.Dir(bin), "tools")) + "</string>"
	}
	log := filepath.Join(dir, "jerboad.log")
	plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>
 <key>Label</key><string>` + nativeService + `</string>
 <key>ProgramArguments</key><array><string>` + xmlText(bin) + `</string><string>--host</string><string>` + xmlText(endpoint) + `</string>` + hypervisorArgs + toolsArgs + observabilityArgs + `</array>
 <key>EnvironmentVariables</key><dict><key>JERBOA_AUTH_TOKEN</key><string>` + xmlText(token) + `</string><key>PATH</key><string>/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
 <key>RunAtLoad</key><true/><key>StandardOutPath</key><string>` + xmlText(log) + `</string><key>StandardErrorPath</key><string>` + xmlText(log) + `</string></dict></plist>`
	plistPath := filepath.Join(dir, nativeService+".plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0600); err != nil {
		return err
	}
	if err := os.Chmod(plistPath, 0600); err != nil {
		return err
	}
	// Replace a registered but dead service left by a failed start.
	if exec.CommandContext(cmd.Context(), "/bin/launchctl", "print", nativeDomain()+"/"+nativeService).Run() == nil {
		if out, err := exec.CommandContext(cmd.Context(), "/bin/launchctl", "bootout", nativeDomain()+"/"+nativeService).CombinedOutput(); err != nil {
			return fmt.Errorf("replace native service: %w: %s", err, out)
		}
	}
	if out, err := exec.CommandContext(cmd.Context(), "/bin/launchctl", "bootstrap", nativeDomain(), plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("start native daemon: %w: %s", err, out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !wslboot.Healthy(cmd.Context(), endpoint, token) {
		if time.Now().After(deadline) {
			return fmt.Errorf("native daemon did not start; see %s", log)
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err := wslboot.SaveDaemonFile(daemonJSONPath(), token, endpoint); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "native ARM64 daemon running at %s\n", endpoint)
	return nil
}

func nativeHypervisorArgs(hypervisor, binDir string) (string, error) {
	name, flag := "qemu-system-aarch64", "--qemu"
	if hypervisor == "firecracker" {
		name, flag = "firecracker", "--fc-bin"
	} else if hypervisor != "qemu" {
		return "", fmt.Errorf("unknown macOS hypervisor %q", hypervisor)
	}
	bin := ""
	if hypervisor == "firecracker" {
		bin = os.Getenv("JERBOA_FIRECRACKER_BIN")
	}
	if bin == "" {
		bin = filepath.Join(binDir, name)
		if _, err := os.Stat(bin); err != nil {
			bin, err = exec.LookPath(name)
			if err != nil {
				bin = filepath.Join("/opt/homebrew/bin", name)
			}
		}
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", fmt.Errorf("native %s executable missing at %s", hypervisor, bin)
	}
	args := "<string>--hypervisor</string><string>" + xmlText(hypervisor) + "</string><string>" + flag + "</string><string>" + xmlText(bin) + "</string>"
	if hypervisor == "firecracker" {
		if policy := os.Getenv("JERBOA_FIRECRACKER_SECURITY"); policy != "" {
			policy, err = filepath.Abs(policy)
			if err != nil {
				return "", err
			}
			args += "<string>--fc-security</string><string>" + xmlText(policy) + "</string>"
		}
	}
	return args, nil
}
