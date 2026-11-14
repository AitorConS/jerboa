package main

import (
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/volume"
)

// The fuzz targets below feed the parsers of everything a user types on the
// command line. Each holds its parser to more than "does not panic": what a
// parser accepts must be something the daemon can act on.

// A port specification that is accepted names two real ports, a known
// protocol and, when it has one, a bind address that is an IP.
func FuzzParsePortMap(f *testing.F) {
	for _, seed := range []string{
		"8080:80", "8080:80/tcp", "53:53/udp", "127.0.0.1:8080:80", "0:80", "80:0", "65536:80", "80:65535",
		"::1:80:80", "[::1]:80:80", "80", ":", "::", "80:80/", "80:80/sctp", " 80 : 80 ", "-1:80", "+80:80",
		"80:80:80:80", "a:b", "8080:80/TCP", "0x50:80", "80:80/tcp/udp", "1e3:80", "٨٠:80",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		pm, err := api.ParsePortMap(spec)
		if err != nil {
			if err.Error() == "" {
				t.Fatalf("empty error for %q", spec)
			}
			return
		}
		if pm.HostPort == 0 || pm.GuestPort == 0 {
			t.Fatalf("%q accepted with a zero port: %+v", spec, pm)
		}
		if pm.Protocol != "tcp" && pm.Protocol != "udp" {
			t.Fatalf("%q accepted with protocol %q", spec, pm.Protocol)
		}
		if pm.BindAddr != "" && net.ParseIP(pm.BindAddr) == nil {
			t.Fatalf("%q accepted with bind address %q", spec, pm.BindAddr)
		}
		// One bad specification refuses the whole list.
		if _, err := api.ParsePortMaps([]string{spec, "not-a-port"}); err == nil {
			t.Fatalf("a list with a bad entry was accepted")
		}
	})
}

// A volume specification that is accepted has a name and an absolute guest
// path, and is read-only only when it says so.
func FuzzParseVolumeMount(f *testing.F) {
	for _, seed := range []string{
		"data:/var/lib", "data:/var/lib:ro", "data:/var/lib:RO", "data:/var/lib:rw", ":/x", "data:", "data",
		"data:relative", "data:/a:ro:extra", "a:/", "../x:/y", "data:/a\x00b", "C:\\x:/y", "data:/a b", "::",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		name, guest, ro, err := api.ParseVolumeMount(spec)
		mounts, listErr := resolveVolumes([]string{spec})
		if (err == nil) != (listErr == nil) {
			t.Fatalf("%q: the CLI and the wire parser disagree: %v vs %v", spec, err, listErr)
		}
		if err != nil {
			return
		}
		if name == "" || !strings.HasPrefix(guest, "/") {
			t.Fatalf("%q accepted as name %q, path %q", spec, name, guest)
		}
		if ro != strings.HasSuffix(strings.ToLower(spec), ":ro") {
			t.Fatalf("%q: read-only is %v", spec, ro)
		}
		if len(mounts) != 1 || mounts[0].GuestPath != guest || mounts[0].ReadOnly != ro {
			t.Fatalf("%q resolved to %+v", spec, mounts)
		}
	})
}

// A byte size that is accepted is positive and did not overflow.
func FuzzParseByteSize(f *testing.F) {
	for _, seed := range []string{
		"512M", "1G", "10MB", "10MiB", "4k", "0", "-1G", "", " 1g ", "9223372036854775807", "9223372036854775807G",
		"8589934592G", "1T", "1.5G", "G", "B", "IB", "1GG", "0x10M", "1e3", "١G", "1_000",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := parseByteSize("memory-max", s)
		if err != nil {
			if !strings.Contains(err.Error(), "memory-max") {
				t.Fatalf("the error for %q does not name the flag: %v", s, err)
			}
			return
		}
		if s == "" {
			if got != 0 {
				t.Fatalf("an unset size parsed as %d", got)
			}
			return
		}
		if got <= 0 {
			t.Fatalf("%q accepted as %d", s, got)
		}
	})
}

// A volume size keeps the sign and at least the magnitude of the number the
// user wrote: a size that does not fit is refused, not wrapped around.
func FuzzVolumeParseSize(f *testing.F) {
	for _, seed := range []string{"1G", "512M", "64K", "1024", "", "0", "-1G", "9223372036854775807G", "8589934592G", "-8589934593G", "G", "1g", "1 G", "1.5G", "+5M"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := volume.ParseSize(s)
		if err != nil || s == "" {
			return
		}
		negative := strings.HasPrefix(s, "-")
		if (got < 0) != negative && got != 0 {
			t.Fatalf("%q wrapped around to %d", s, got)
		}
		if got == math.MinInt64 {
			return
		}
		digits := strings.TrimRight(strings.TrimLeft(s, "+-0"), "KMG")
		if digits != "" && got == 0 {
			t.Fatalf("%q parsed as zero", s)
		}
	})
}

// An env file never yields an entry that is blank or a comment, and a
// variable given twice keeps its last value only.
func FuzzBuildEnvFile(f *testing.F) {
	for _, seed := range []string{
		"A=1\nB=2\n", "# comment\nA=1\n\n  B = 2  \n", "A=1\nA=2\n", "NOEQUALS\n", "=value\n", "A=1\r\nB=2\r\n",
		"\xff\xfe\x00", "A=" + strings.Repeat("x", 70000) + "\n", "A=1", "export A=1\n", "A=\"quoted value\"\n", "\x00=\x00\n",
	} {
		f.Add(seed, "A=flag")
	}
	f.Fuzz(func(t *testing.T, content, flag string) {
		path := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		env, err := buildEnv([]string{flag}, path)
		if err != nil {
			// The only way to fail on a readable file is a line too long.
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("the error does not name the file: %v", err)
			}
			return
		}
		keys := map[string]bool{}
		for _, entry := range env {
			key, _, _ := strings.Cut(entry, "=")
			if keys[key] {
				t.Fatalf("variable %q appears twice in %q", key, env)
			}
			keys[key] = true
			if entry != flag && (strings.TrimSpace(entry) == "" || strings.HasPrefix(entry, "#")) {
				t.Fatalf("blank or comment line passed through: %q", entry)
			}
		}
		// A flag wins over the file for the same variable.
		flagKey, _, _ := strings.Cut(flag, "=")
		for _, entry := range env {
			if key, _, _ := strings.Cut(entry, "="); key == flagKey && entry != flag {
				t.Fatalf("the file's %q beat the flag %q", entry, flag)
			}
		}
	})
}

// Health checks and restart policies that are accepted are within what the
// daemon implements.
func FuzzParseHealthCheckAndRestart(f *testing.F) {
	for _, seed := range []string{
		"tcp:80", "http:8080:/health", "http:8080:health", "HTTP:1:/", "tcp:0", "tcp:65536", "icmp:1", "tcp", "tcp:", ":80",
		"always", "always:3", "on-failure:0", "never", "always:-1", "always:x", "ALWAYS", "sometimes", "", "always:99999999999999999999",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		if hc, err := parseHealthCheck(spec); err == nil {
			if hc.Port < 1 || hc.Port > 65535 || (hc.Type != "tcp" && hc.Type != "http") {
				t.Fatalf("health check %q accepted as %+v", spec, hc)
			}
			if hc.Path != "" && !strings.HasPrefix(hc.Path, "/") {
				t.Fatalf("health check %q has path %q", spec, hc.Path)
			}
		}
		if rp, err := parseRestartPolicy(spec); err == nil {
			if rp.MaxRetries < 0 || (rp.Policy != "never" && rp.Policy != "on-failure" && rp.Policy != "always") {
				t.Fatalf("restart policy %q accepted as %+v", spec, rp)
			}
		}
	})
}
