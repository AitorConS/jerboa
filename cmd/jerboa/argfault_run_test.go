package main

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mistake in the arguments of `jerboa run` is the user's most common
// failure. Each must be answered with one message that names the flag or the
// value, and the daemon must not hear about a request that was never valid.

func TestRunRejectsBadInputBeforeReachingTheDaemon(t *testing.T) {
	e := afIsolate(t)
	envDir := t.TempDir()
	elf := afWrite(t, filepath.Join(t.TempDir(), "prog"), "\x7fELF\x02\x01\x01\x00 not a disk image")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no image", []string{"run"}, "accepts 1 arg(s), received 0"},
		{"two images", []string{"run", "a:latest", "b:latest"}, "accepts 1 arg(s), received 2"},
		{"unknown flag", []string{"run", "--no-such-flag", "web:latest"}, "unknown flag: --no-such-flag"},
		{"cpus not a number", []string{"run", "--cpus", "many", "web:latest"}, `invalid argument "many" for "--cpus"`},
		{"cpus zero", []string{"run", "--cpus", "0", "web:latest"}, "--cpus must be at least 1"},
		{"cpus negative", []string{"run", "--cpus", "-2", "web:latest"}, "--cpus must be at least 1"},
		{"cpu shares negative", []string{"run", "--cpu-shares", "-1", "web:latest"}, `invalid argument "-1" for "--cpu-shares"`},
		{"disk iops not a number", []string{"run", "--disk-iops", "fast", "web:latest"}, `invalid argument "fast" for "--disk-iops"`},

		{"port without guest", []string{"run", "-p", "8080", "web:latest"}, "expected [bindaddr:]host:guest format"},
		{"port empty", []string{"run", "-p", "", "web:latest"}, "expected [bindaddr:]host:guest format"},
		{"port names", []string{"run", "-p", "http:web", "web:latest"}, `host port: invalid port "http"`},
		{"host port zero", []string{"run", "-p", "0:80", "web:latest"}, "must be greater than 0"},
		{"guest port zero", []string{"run", "-p", "80:0", "web:latest"}, "must be greater than 0"},
		{"host port too large", []string{"run", "-p", "65536:80", "web:latest"}, `invalid port "65536"`},
		{"guest port negative", []string{"run", "-p", "80:-1", "web:latest"}, `invalid port "-1"`},
		{"unknown protocol", []string{"run", "-p", "80:80/sctp", "web:latest"}, `unknown protocol "sctp"`},
		{"bind address not an IP", []string{"run", "-p", "localhost:80:80", "web:latest"}, `bind address "localhost" is not a valid IP`},
		{"too many port fields", []string{"run", "-p", "1:2:3:4", "web:latest"}, "expected [bindaddr:]host:guest format"},
		{"second port bad", []string{"run", "-p", "80:80", "-p", "x:y", "web:latest"}, `invalid port "x"`},

		{"env file missing", []string{"run", "--env-file", filepath.Join(envDir, "absent.env"), "web:latest"}, "open env-file"},
		{"env file is a directory", []string{"run", "--env-file", envDir, "web:latest"}, "env-file"},

		{"volume without path", []string{"run", "-v", "data", "web:latest"}, "expected name:/guestpath[:ro]"},
		{"volume without name", []string{"run", "-v", ":/data", "web:latest"}, "expected name:/guestpath[:ro]"},
		{"volume relative path", []string{"run", "-v", "data:var/lib", "web:latest"}, "expected name:/guestpath[:ro]"},
		{"volume unknown mode", []string{"run", "-v", "data:/data:rw", "web:latest"}, "mode must be ro"},
		{"volume extra field", []string{"run", "-v", "data:/data:ro:z", "web:latest"}, "expected name:/guestpath[:ro]"},
		{"volume empty", []string{"run", "-v", "", "web:latest"}, "expected name:/guestpath[:ro]"},

		{"ip not an address", []string{"run", "--ip", "999.1.1.1", "--network", "n", "web:latest"}, `invalid IP address "999.1.1.1"`},
		{"ip is a hostname", []string{"run", "--ip", "gateway", "--network", "n", "web:latest"}, `invalid IP address "gateway"`},
		{"ip without network", []string{"run", "--ip", "10.0.0.5", "web:latest"}, "--ip requires --network"},

		{"binary instead of image", []string{"run", elf}, "is an ELF binary, not a bootable disk image"},
		{"verify policy unknown", []string{"run", "--verify", "maybe", "./disk.img"}, "maybe"},
		{"enforce on a file", []string{"run", "--verify", "enforce", "./disk.img"}, "requires a name:tag image, not a file path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := e.dials.Load()
			res := e.run(tc.args...)
			afCleanError(t, res.err, tc.want)
			assert.Empty(t, res.stdout, "a rejected run must not print a VM id")
			assert.Equal(t, before, e.dials.Load(), "the daemon was contacted for a request that could never be valid")
		})
	}
}

// Publishing a port needs a managed network on Linux (the macOS backend
// forwards ports itself). Without one the run must be refused up front, not
// started with a port that silently goes nowhere.
func TestRunPortWithoutNetwork(t *testing.T) {
	e := afIsolate(t)
	res := e.run("run", "-p", "8080:80", "web:latest")
	require.Error(t, res.err)
	if runtime.GOOS == "darwin" {
		// The request is valid here; it fails only because the daemon is mute.
		require.NotContains(t, res.err.Error(), "--port requires --network")
		return
	}
	afCleanError(t, res.err, "--port requires --network")
	require.Zero(t, e.dials.Load())
}

// These flags are checked once the connection is open. They must still be
// refused by the CLI, with the flag named, before any VM is requested: the
// idle daemon drops the connection on the first byte, so an error about the
// connection would mean a request was sent.
func TestRunRejectsBadLimitsWithoutRequestingAVM(t *testing.T) {
	e := afIsolate(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"memory-max words", []string{"--memory-max", "lots"}, `memory-max: invalid value "LOTS"`},
		{"memory-max zero", []string{"--memory-max", "0"}, "memory-max: must be positive, got 0"},
		{"memory-max negative", []string{"--memory-max", "-512M"}, "memory-max: must be positive, got -512"},
		{"memory-max fraction", []string{"--memory-max", "1.5G"}, `memory-max: invalid value "1.5"`},
		{"memory-max overflow", []string{"--memory-max", "9223372036854775807G"}, "memory-max"},
		{"disk-bps words", []string{"--disk-bps", "fast"}, `disk-bps: invalid value "FAST"`},
		{"disk-bps negative", []string{"--disk-bps", "-1"}, "disk-bps: must be positive, got -1"},
		{"health-check no port", []string{"--health-check", "tcp"}, "health check format: tcp:PORT or http:PORT:/path"},
		{"health-check port words", []string{"--health-check", "tcp:http"}, "health check port must be a number"},
		{"health-check port zero", []string{"--health-check", "tcp:0"}, "health check port must be 1-65535"},
		{"health-check port too large", []string{"--health-check", "http:70000:/"}, "health check port must be 1-65535"},
		{"health-check unknown type", []string{"--health-check", "icmp:80"}, `health check type must be tcp or http, got "icmp"`},
		{"restart unknown", []string{"--restart", "sometimes"}, `restart policy must be never, on-failure, or always, got "sometimes"`},
		{"restart retries words", []string{"--restart", "on-failure:many"}, "restart max-retries must be a number"},
		{"restart retries negative", []string{"--restart", "always:-1"}, "restart max-retries must be >= 0, got -1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := e.run(append(append([]string{"run"}, tc.args...), "web:latest")...)
			afCleanError(t, res.err, "run: ", tc.want)
			require.NotContains(t, res.err.Error(), "EOF", "a VM was requested before the flag was checked")
			require.Empty(t, res.stdout)
		})
	}
}

func TestParseByteSize(t *testing.T) {
	good := map[string]int64{
		"":            0,
		"1":           1,
		"512":         512,
		"4K":          4 << 10,
		"4k":          4 << 10,
		"4KB":         4 << 10,
		"4KiB":        4 << 10,
		"512M":        512 << 20,
		" 512m ":      512 << 20, //nolint:gocritic // surrounding blanks are the case under test
		"10MB":        10 << 20,
		"10mib":       10 << 20,
		"2G":          2 << 30,
		"2GiB":        2 << 30,
		"100B":        100,
		"8589934591G": 8589934591 << 30,
	}
	for in, want := range good {
		got, err := parseByteSize("memory-max", in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}

	bad := []string{
		"M", "B", "iB", "GB", "-1", "-1G", "0", "0M", "1.5G", "1,5G", "1 G", "1T", "1GG", "0x10", "1e3",
		"+", "--1", "１２M", "9223372036854775808", "1\x00M",
	}
	for _, in := range bad {
		got, err := parseByteSize("disk-bps", in)
		require.Error(t, err, "%q parsed as %d", in, got)
		require.Contains(t, err.Error(), "disk-bps", "the error must name the flag")
		require.Zero(t, got)
	}
}

// A size whose byte count does not fit in 64 bits must be refused. Wrapping
// around would turn an absurd limit into a small or negative one that the
// daemon then applies.
func TestParseByteSizeRejectsOverflow(t *testing.T) {
	for _, in := range []string{
		"8589934592G",          // 2^63 exactly
		"17179869185G",         // wraps to exactly 1G
		"9223372036854775807G", // wraps to a negative number
		"9223372036854775807M",
		"9223372036854775807K",
		"8796093022208M",
	} {
		got, err := parseByteSize("memory-max", in)
		require.Error(t, err, "%q parsed as %d", in, got)
		require.Contains(t, err.Error(), "memory-max")
		require.Zero(t, got)
	}
	got, err := parseByteSize("memory-max", "9223372036854775807")
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64), got)
}

func TestBuildEnvFromFile(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		return afWrite(t, filepath.Join(dir, "vars.env"), content)
	}

	t.Run("comments blank lines and CRLF", func(t *testing.T) {
		path := write("# database\r\nDB_HOST=db\r\n\r\n   \r\n  DB_PORT=5432  \r\n#DB_USER=root\r\n")
		env, err := buildEnv(nil, path)
		require.NoError(t, err)
		require.Equal(t, []string{"DB_HOST=db", "DB_PORT=5432"}, env)
	})

	t.Run("a flag overrides the file and keeps the file's position", func(t *testing.T) {
		path := write("A=file\nB=file\n")
		env, err := buildEnv([]string{"B=flag", "C=flag"}, path)
		require.NoError(t, err)
		require.Equal(t, []string{"A=file", "B=flag", "C=flag"}, env)
	})

	t.Run("a later line overrides an earlier one", func(t *testing.T) {
		path := write("A=1\nA=2\nA=\n")
		env, err := buildEnv(nil, path)
		require.NoError(t, err)
		require.Equal(t, []string{"A="}, env)
	})

	t.Run("values keep their equals signs and spaces", func(t *testing.T) {
		path := write("URL=postgres://u:p@h/db?sslmode=require&x=y\nGREETING=hello  world\n")
		env, err := buildEnv(nil, path)
		require.NoError(t, err)
		require.Equal(t, []string{"URL=postgres://u:p@h/db?sslmode=require&x=y", "GREETING=hello  world"}, env)
	})

	t.Run("empty file", func(t *testing.T) {
		env, err := buildEnv([]string{"A=1"}, write(""))
		require.NoError(t, err)
		require.Equal(t, []string{"A=1"}, env)
	})

	t.Run("no trailing newline", func(t *testing.T) {
		env, err := buildEnv(nil, write("A=1\nB=2"))
		require.NoError(t, err)
		require.Equal(t, []string{"A=1", "B=2"}, env)
	})

	// A line longer than the scanner's buffer must fail the run. Dropping the
	// rest of the file would start the VM with half its configuration.
	t.Run("a line too long to read is an error not a truncation", func(t *testing.T) {
		path := write("A=1\nBIG=" + strings.Repeat("x", 128<<10) + "\nAFTER=1\n")
		env, err := buildEnv(nil, path)
		require.Error(t, err)
		require.Contains(t, err.Error(), "read env-file "+path)
		require.Nil(t, env)
	})

	t.Run("missing file names the path", func(t *testing.T) {
		missing := filepath.Join(dir, "absent.env")
		_, err := buildEnv([]string{"A=1"}, missing)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.Contains(t, err.Error(), missing)
	})

	t.Run("unreadable file", func(t *testing.T) {
		afSkipIfPermissionsIgnored(t)
		path := write("A=1\n")
		require.NoError(t, os.Chmod(path, 0o000))
		_, err := buildEnv(nil, path)
		require.ErrorIs(t, err, os.ErrPermission)
	})

	t.Run("no file leaves flags alone", func(t *testing.T) {
		env, err := buildEnv([]string{"A=1", "B=2", "A=3"}, "")
		require.NoError(t, err)
		require.Equal(t, []string{"A=3", "B=2"}, env)
		env, err = buildEnv(nil, "")
		require.NoError(t, err)
		require.Empty(t, env)
		require.NotNil(t, env, "an empty environment is sent as [] on the wire, not null")
	})
}

func TestResolveVolumesStopsAtTheFirstBadSpec(t *testing.T) {
	got, err := resolveVolumes([]string{"data:/var/lib", "logs:/var/log:RO"})
	require.NoError(t, err)
	require.Equal(t, []api.VolumeMountSpec{
		{Name: "data", GuestPath: "/var/lib"},
		{Name: "logs", GuestPath: "/var/log", ReadOnly: true},
	}, got)

	got, err = resolveVolumes([]string{"data:/var/lib", "broken", "logs:/var/log"})
	require.ErrorContains(t, err, `volume "broken"`)
	require.Nil(t, got, "a partial list would mount some volumes and not others")

	got, err = resolveVolumes(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

// The image argument is either a path the daemon opens or a reference it
// looks up. Guessing wrong sends a path to the store or a name to the disk.
func TestSplitImageArg(t *testing.T) {
	dir := t.TempDir()
	disk := afWrite(t, filepath.Join(dir, "disk.img"), "raw disk bytes")
	elf := afWrite(t, filepath.Join(dir, "app"), "\x7fELFrest")
	short := afWrite(t, filepath.Join(dir, "tiny"), "\x7fE")
	empty := afWrite(t, filepath.Join(dir, "empty"), "")
	t.Chdir(dir)

	refs := []string{"web", "web:latest", "acme/web:v1", "registry.example.com/acme/web:v1", "localhost:5000/a/b", "web@sha256:abc"}
	for _, in := range refs {
		ref, path, err := splitImageArg(in)
		require.NoError(t, err, in)
		require.Equal(t, in, ref)
		require.Empty(t, path)
	}

	paths := []string{disk, "./disk.img", "../" + filepath.Base(dir) + "/disk.img", "./absent.img", "/nonexistent/disk.img", `C:\images\disk.img`, "C:/images/disk.img", ".hidden.img", short, empty}
	for _, in := range paths {
		ref, path, err := splitImageArg(in)
		require.NoError(t, err, in)
		require.Empty(t, ref, in)
		require.Equal(t, in, path)
	}

	// A bare name that happens to match a file in the current directory is
	// still a reference: only an explicit path prefix selects a file.
	ref, path, err := splitImageArg("disk.img")
	require.NoError(t, err)
	require.Equal(t, "disk.img", ref)
	require.Empty(t, path)

	_, _, err = splitImageArg(elf)
	require.ErrorContains(t, err, "is an ELF binary, not a bootable disk image")
	require.ErrorContains(t, err, "jerboa build --name")
	_, _, err = splitImageArg("./app")
	require.ErrorContains(t, err, "is an ELF binary")

	// A directory or an unreadable file is left for the daemon to report.
	_, path, err = splitImageArg(dir + "/")
	require.NoError(t, err)
	require.Equal(t, dir+"/", path)
}

func TestSplitImageArgUnreadableFile(t *testing.T) {
	afSkipIfPermissionsIgnored(t)
	locked := afWrite(t, filepath.Join(t.TempDir(), "locked.img"), "\x7fELF")
	require.NoError(t, os.Chmod(locked, 0o000))
	_, path, err := splitImageArg(locked)
	require.NoError(t, err, "what cannot be read cannot be called a binary")
	require.Equal(t, locked, path)
}

func TestGatewayAndMaskHelpers(t *testing.T) {
	require.Equal(t, "10.1.2.1", gatewayIP("10.1.2.77"))
	require.Equal(t, "10.1.2.1", gatewayIP("10.1.2.1"))
	for _, bad := range []string{"", "10.1.2", "not-an-ip", "fe80::1", "10.1.2.300"} {
		require.Empty(t, gatewayIP(bad), bad)
	}
	require.Equal(t, "16", extractMask("10.0.0.0/16"))
	require.Equal(t, "24", extractMask("10.0.0.0"))
	require.Equal(t, "24", extractMask(""))
	require.Equal(t, "8/9", extractMask("10.0.0.0/8/9"))

	require.Equal(t, "/home/u/.jerboa/volumes", volumeStorePath("/home/u/.jerboa/images"))
	require.Equal(t, `C:\Users\u\.jerboa\volumes`, volumeStorePath(`C:\Users\u\.jerboa\images`))
	require.Equal(t, "volumes", volumeStorePath("images"))
	require.Equal(t, "volumes", volumeStorePath(""))
}
