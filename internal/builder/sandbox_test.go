package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBwrapArgs(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".aws"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".docker"), nil, 0o600))
	st := Step{Dir: "/src/app", Writable: []string{"/cache/go"}}
	sockDir := filepath.Join(home, "run")
	require.NoError(t, os.MkdirAll(sockDir, 0o700))
	sock := filepath.Join(home, "jerboad.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	argv := bwrapArgs("/usr/bin/bwrap", st, hiddenPaths(home, st), []string{"/tmp/step"},
		[]string{sockDir, sock, filepath.Join(home, "missing.sock")}, "go", []string{"build", "./..."})
	line := strings.Join(argv, " ")

	require.Equal(t, "/usr/bin/bwrap", argv[0])
	require.Contains(t, line, "--ro-bind / /")
	require.NotContains(t, line, "--tmpfs /tmp ")
	require.Contains(t, line, "--bind /tmp/step /tmp/step")
	require.Contains(t, line, "--unshare-net")
	require.Contains(t, line, "--bind /cache/go /cache/go")
	require.Contains(t, line, "--chdir /src/app")
	require.Contains(t, line, "--tmpfs "+filepath.Join(home, ".aws"))
	require.Contains(t, line, "--ro-bind /dev/null "+filepath.Join(home, ".docker"))
	require.Contains(t, line, "--bind /src/app /src/app --tmpfs "+realPath(sockDir)+" --ro-bind /dev/null "+realPath(sock)+" --chdir")
	require.NotContains(t, line, "missing.sock")
	require.True(t, strings.HasSuffix(line, "-- go build ./..."), line)

	st.Network = true
	require.NotContains(t, strings.Join(bwrapArgs("bwrap", st, nil, nil, nil, "go", nil), " "), "--unshare-net")
}

// The build's policy files stay read-only inside the read-write project, so
// a confined step cannot relax the next build's confinement.
func TestBwrapArgsKeepsThePolicyReadOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ConfigFileName)
	require.NoError(t, os.WriteFile(cfg, []byte("[build]\n"), 0o600))
	st := Step{Dir: dir, ReadOnly: []string{cfg, filepath.Join(dir, UnignoreFile)}}
	line := strings.Join(bwrapArgs("bwrap", st, nil, nil, nil, "sh", nil), " ")
	bind := "--bind " + dir + " " + dir
	ro := "--ro-bind " + realPath(cfg) + " " + realPath(cfg)
	require.Contains(t, line, ro)
	require.Greater(t, strings.Index(line, ro), strings.Index(line, bind), "the read-only bind must come after the project's read-write one")
	require.NotContains(t, line, UnignoreFile, "a missing file cannot be bound")
}

func TestSandboxPassesThePolicyFilesToTheStep(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ConfigFileName)
	require.NoError(t, os.WriteFile(cfg, []byte("[build]\n"), 0o600))
	cmd, done, err := Sandbox{ReadOnly: []string{cfg}}.Command(context.Background(), Step{Dir: dir, Env: []string{}}, "true")
	require.NoError(t, err)
	defer done()
	if sandboxName() == "bubblewrap" {
		require.Contains(t, strings.Join(cmd.Args, " "), "--ro-bind "+realPath(cfg)+" "+realPath(cfg))
	}
}

func TestSeatbeltProfile(t *testing.T) {
	st := Step{Dir: "/src/a \"b\"", Writable: []string{"/cache"}}
	p := seatbeltProfile(st, []string{"/home/u/.aws"}, []string{"/tmp/step"}, []string{"/home/u/.jerboa"})
	require.Contains(t, p, "(deny file-write*)")
	require.Contains(t, p, `(subpath "/src/a \"b\"")`)
	require.Contains(t, p, `(subpath "/cache")`)
	require.Contains(t, p, `(subpath "/tmp/step")`)
	require.Contains(t, p, `(deny file-read* (subpath "/home/u/.aws"))`)
	require.Contains(t, p, "(deny network*)")
	require.NotContains(t, p, "local ip")
	deny := `(deny network-outbound (remote unix-socket (subpath "/home/u/.jerboa")))`
	require.Contains(t, p, deny)
	require.Greater(t, strings.Index(p, deny), strings.Index(p, "(allow network* (remote unix-socket))"), "the deny must come after the allow it overrides")

	st.Network = true
	require.NotContains(t, seatbeltProfile(st, nil, nil, nil), "network")
}

func TestToolCachesAreJerboasOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	cargo := filepath.Join(home, ".cargo")
	require.NoError(t, os.MkdirAll(cargo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cargo, "config.toml"), []byte("[net]\n"), 0o644))
	t.Setenv("CARGO_HOME", "")

	dirs, env, err := toolCaches()
	require.NoError(t, err)
	cacheDir, err := os.UserCacheDir()
	require.NoError(t, err)
	base := filepath.Join(cacheDir, "jerboa", "build-sandbox")
	vars := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	for _, k := range []string{"npm_config_cache", "PIP_CACHE_DIR", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "XDG_CACHE_HOME"} {
		require.True(t, within(vars[k], base), "%s=%s", k, vars[k])
	}
	for _, d := range dirs {
		require.DirExists(t, d)
		require.True(t, within(d, base), d)
		require.False(t, within(cargo, d) || within(d, cargo), "the user's cargo home is not writable")
	}
	got, err := os.ReadFile(filepath.Join(vars["CARGO_HOME"], "config.toml"))
	require.NoError(t, err)
	require.Equal(t, "[net]\n", string(got))

	// A config a step left behind is replaced by the user's.
	require.NoError(t, os.WriteFile(filepath.Join(vars["CARGO_HOME"], "credentials.toml"), []byte("planted"), 0o600))
	_, _, err = toolCaches()
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(vars["CARGO_HOME"], "credentials.toml"))
}

func TestControlSocketsCoverTheDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := controlSockets([]string{"/srv/jerboad.sock"})
	require.Contains(t, got, filepath.Join(home, ".jerboa"))
	require.Contains(t, got, "/var/run/jerboad.sock")
	require.Contains(t, got, "/srv/jerboad.sock")
}

func TestHiddenPathsSparesTheProjectAndCaches(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".jerboa", ".kube", ".docker"} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, d), 0o700))
	}
	st := Step{Dir: filepath.Join(home, ".jerboa", "src"), Writable: []string{filepath.Join(home, ".docker")}}
	require.Equal(t, []string{filepath.Join(home, ".kube")}, hiddenPaths(home, st))
}

func TestSandboxOffRunsTheToolDirectly(t *testing.T) {
	cmd, done, err := Sandbox{Off: true}.Command(context.Background(), Step{Dir: t.TempDir(), Env: []string{"A=1"}}, "go", "version")
	require.NoError(t, err)
	defer done()
	require.Equal(t, "go", filepath.Base(cmd.Args[0]))
	require.Equal(t, []string{"A=1"}, cmd.Env)
}

func TestSandboxCreatesWritableDirsAndAPrivateTmp(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "npm")
	cmd, done, err := Sandbox{}.Command(context.Background(), Step{Dir: t.TempDir(), Writable: []string{cache}, Env: []string{}}, "true")
	require.NoError(t, err)
	require.DirExists(t, cache)
	var tmp string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			tmp = v
		}
	}
	require.DirExists(t, tmp)
	done()
	require.NoDirExists(t, tmp)
}

func TestBuildSandboxFromConfig(t *testing.T) {
	var nilCfg *Config
	require.False(t, nilCfg.BuildSandbox().Off)
	for body, off := range map[string]bool{
		"[build]\nlang = \"go\"\n":                   false,
		"[build]\nsandbox = true\n":                  false,
		"[build]\nsandbox = false\nnetwork = true\n": true,
	} {
		cfg, err := parseConfig("unikernel.toml", []byte(body))
		require.NoError(t, err, body)
		require.Equal(t, off, cfg.BuildSandbox().Off, body)
	}
	cfg, err := parseConfig("unikernel.toml", []byte("[build]\nnetwork = true\n"))
	require.NoError(t, err)
	require.True(t, cfg.Build.Network)
}

// Host services that act for whoever connects are masked like the daemon.
func TestBrokerSocketsCoverTheHostBrokers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no build sandbox on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/elsewhere")
	got := brokerSockets()
	uid := os.Getuid()
	for _, want := range []string{
		fmt.Sprintf("/run/user/%d/bus", uid),
		fmt.Sprintf("/run/user/%d/systemd", uid),
		fmt.Sprintf("/run/user/%d/docker.sock", uid),
		fmt.Sprintf("/run/user/%d/podman", uid),
		"/run/user/elsewhere/bus",
		"/run/dbus/system_bus_socket",
		"/var/run/docker.sock",
		"/tmp/.X11-unix",
		fmt.Sprintf("/tmp/tmux-%d", uid),
		filepath.Join(home, ".docker", "run"),
	} {
		require.Contains(t, got, want)
	}
	for _, p := range got {
		require.NotContains(t, p, "ssh", "the SSH agent stays reachable")
	}
}

func TestBrokerMasksSpareTheStepsDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, ".lima", "src")
	got := brokerMasks(Step{Dir: project}, filepath.Join(t.TempDir(), "step"))
	require.NotContains(t, got, filepath.Join(home, ".lima"), "masking it would hide the project")
	require.Contains(t, got, filepath.Join(home, ".colima"))
}

func TestBwrapArgsMaskTheHostBrokers(t *testing.T) {
	rt := t.TempDir()
	bus := filepath.Join(rt, "bus")
	require.NoError(t, os.WriteFile(bus, nil, 0o600))
	systemd := filepath.Join(rt, "systemd")
	require.NoError(t, os.MkdirAll(systemd, 0o700))
	line := strings.Join(bwrapArgs("bwrap", Step{Dir: "/src/app"}, nil, nil, []string{bus, systemd}, "sh", nil), " ")
	require.Contains(t, line, "--ro-bind /dev/null "+realPath(bus))
	require.Contains(t, line, "--tmpfs "+realPath(systemd))
}

func TestSeatbeltProfileDeniesAppleEvents(t *testing.T) {
	require.Contains(t, seatbeltProfile(Step{Dir: "/src"}, nil, nil, nil), "(deny appleevent-send)")
}

func TestConfinedStepsLoseBrokerAddresses(t *testing.T) {
	env := withoutBrokerAddresses([]string{"DBUS_SESSION_BUS_ADDRESS=unix:abstract=/tmp/dbus-x", "DOCKER_HOST=tcp://127.0.0.1:2375", "SSH_AUTH_SOCK=/run/user/1/ssh", "PATH=/bin"})
	require.Equal(t, []string{"SSH_AUTH_SOCK=/run/user/1/ssh", "PATH=/bin"}, env)
	// Unconfined, the step keeps its environment as given.
	cmd, done, err := Sandbox{Off: true}.Command(context.Background(), Step{Dir: t.TempDir(), Env: []string{"DOCKER_HOST=x"}}, "true")
	require.NoError(t, err)
	defer done()
	require.Equal(t, []string{"DOCKER_HOST=x"}, cmd.Env)
}
