package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Sandbox confines the build steps that run project code on the host: the
// [build] run commands and the language tools (npm, pip, cargo, go build),
// whose build scripts and plugins run arbitrary code. Confined, a step can
// read the host but write only to the project directory, caches of its own
// and a temporary directory, cannot read the usual credential directories,
// cannot connect to the Jerboa daemon or its VMs' control sockets, nor to
// the host services known to act for whoever asks (see brokerSockets), and,
// unless allowed, cannot reach the network.
//
// The mechanism is bubblewrap (bwrap) on Linux and sandbox-exec on macOS.
// Where neither is available the step runs directly, with Jerboa's
// credentials removed from its environment and a private temporary
// directory, and a warning says so.
type Sandbox struct {
	// Off runs build steps directly on the host ([build] sandbox = false,
	// which the CLI honors only with --allow-unsandboxed).
	Off bool
	// Sockets lists further Unix sockets, or directories of them, a step
	// may not connect to: the endpoint the CLI reaches the daemon at.
	Sockets []string
	// ReadOnly lists files in the project a step may read but not change:
	// the build's own policy (unikernel.toml, .unignore), so a confined
	// step cannot relax the confinement or the build context of later
	// builds.
	ReadOnly []string
}

// Step is one build step to confine.
type Step struct {
	// Dir is the step's working directory, the project; it stays writable.
	Dir string
	// Writable lists further directories the step writes to. They are
	// created when missing.
	Writable []string
	// Network lets the step reach the network.
	Network bool
	// Env is the step's environment; Env() when nil.
	Env []string
	// ReadOnly lists files under Dir or Writable the step may not change.
	ReadOnly []string
}

// Command returns the command that runs name with args as st, confined
// unless the sandbox is off, and a function that releases what it set up.
func (s Sandbox) Command(ctx context.Context, st Step, name string, args ...string) (*exec.Cmd, func(), error) {
	env := st.Env
	if env == nil {
		env = Env()
	}
	dir, err := filepath.Abs(st.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	st.Dir = dir
	for _, w := range st.Writable {
		if err := os.MkdirAll(w, 0o755); err != nil {
			return nil, nil, fmt.Errorf("sandbox: %w", err)
		}
	}
	if s.Off {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the build tool the project asks for
		cmd.Dir = st.Dir
		cmd.Env = env
		return cmd, func() {}, nil
	}

	caches, cacheEnv, err := toolCaches(st.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	st.Writable = append(caches, st.Writable...)
	st.ReadOnly = append(append([]string{}, st.ReadOnly...), s.ReadOnly...)
	env = append(env, cacheEnv...)

	// Resolve the tool here: inside the launcher a missing tool would only
	// show up as the launcher's own failure.
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	name = path

	// A private temporary directory, so the step's scratch files neither
	// collide with nor outlive other processes', and the only temporary
	// directory a confined step may write to.
	tmp, err := os.MkdirTemp("", "jerboa-build-*")
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	done := func() { _ = os.RemoveAll(tmp) }
	env = append(env, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	sockets := append(controlSockets(s.Sockets), brokerMasks(st, tmp)...)
	argv, ok := confine(st, tmp, sockets, name, args)
	if ok {
		env = withoutBrokerAddresses(env)
	} else {
		warnUnconfined.Do(func() {
			slog.Warn("no build sandbox is available on this host (bubblewrap on Linux, sandbox-exec on macOS); build steps run directly, without Jerboa's credentials in their environment")
		})
		argv = append([]string{name}, args...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the sandbox launcher and the build tool
	cmd.Dir = st.Dir
	cmd.Env = env
	return cmd, done, nil
}

var warnUnconfined sync.Once

// Describe says how build steps are confined on this host.
func (s Sandbox) Describe() string {
	if s.Off {
		return "build steps run directly on the host, unconfined ([build] sandbox = false, allowed by --allow-unsandboxed)"
	}
	if name := sandboxName(); name != "" {
		return "build steps run in a sandbox (" + name + "): they write only to the project and tool caches"
	}
	return "no build sandbox is available on this host; build steps run directly"
}

// credentialPaths lists, relative to the home directory, the credential
// stores a build step has no reason to read: Jerboa's own and cloud
// providers'. Those package managers use to fetch private dependencies
// (~/.ssh, ~/.netrc, ~/.npmrc, git credentials) stay readable.
var credentialPaths = []string{
	".jerboa", ".aws", ".azure", ".config/gcloud", ".kube", ".docker", ".gnupg",
}

// hiddenPaths returns the credential stores under home that exist and that
// neither the project nor a writable directory lives in.
func hiddenPaths(home string, st Step) []string {
	if home == "" {
		return nil
	}
	var out []string
	for _, rel := range credentialPaths {
		p := filepath.Join(home, rel)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if within(st.Dir, p) || within(p, st.Dir) {
			continue
		}
		clash := false
		for _, w := range st.Writable {
			if within(w, p) {
				clash = true
				break
			}
		}
		if !clash {
			out = append(out, p)
		}
	}
	return out
}

// within reports whether path is dir or lies under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// toolCaches returns the cache directories confined steps write to, and the
// environment that points the language tools at them. They are Jerboa's
// own: the tools' usual homes hold what later runs outside the sandbox
// (~/.cargo/bin and the sources of downloaded crates, toolchains, the Go
// build cache, wheels pip installs from), so a step that could write there
// could leave code behind for the user's next build. The user's cargo
// configuration and registry credentials are copied in, so cargo still sees
// them.
//
// Each project gets caches of its own, keyed by its real path: a step writes
// to the extracted modules and crates it reads back, so a cache shared by
// every project would let one project's build scripts alter the sources a
// later build of another project compiles.
func toolCaches(project string) ([]string, []string, error) {
	base, err := projectCacheDir(project)
	if err != nil {
		return nil, nil, err
	}
	vars := []struct{ name, dir string }{
		{"npm_config_cache", "npm"},
		{"npm_config_devdir", "node-gyp"},
		{"PIP_CACHE_DIR", "pip"},
		{"GOCACHE", "go-build"},
		{"GOMODCACHE", "go-mod"},
		{"CARGO_HOME", "cargo"},
		{"XDG_CACHE_HOME", "xdg"},
	}
	var dirs, env []string
	for _, v := range vars {
		d := filepath.Join(base, v.dir)
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, fmt.Errorf("cache directory: %w", err)
		}
		dirs = append(dirs, d)
		env = append(env, v.name+"="+d)
	}
	copyCargoConfig(filepath.Join(base, "cargo"))
	return dirs, env, nil
}

// projectCacheDir returns the directory that holds the tool caches of the
// project at dir: a name derived from the project's path with symlinks
// resolved, so two paths to one project share it and two projects never do.
func projectCacheDir(dir string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache directory: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("cache directory: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("cache directory: resolve project %s: %w", abs, err)
	}
	sum := sha256.Sum256([]byte(real))
	return filepath.Join(base, "jerboa", "build-sandbox", hex.EncodeToString(sum[:16])), nil
}

// copyCargoConfig copies the user's cargo configuration and registry
// credentials into the confined steps' cargo home, replacing what an earlier
// step may have left there.
func copyCargoConfig(dst string) {
	src := os.Getenv("CARGO_HOME")
	if src == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		src = filepath.Join(home, ".cargo")
	}
	for _, name := range []string{"config.toml", "config", "credentials.toml", "credentials"} {
		_ = os.Remove(filepath.Join(dst, name))
		data, err := os.ReadFile(filepath.Join(src, name)) //nolint:gosec // the user's own cargo home
		if err == nil {
			_ = os.WriteFile(filepath.Join(dst, name), data, 0o600)
		}
	}
}

// controlSockets returns the Unix sockets, and directories of them, a
// confined step may not connect to: the daemon's API at its default
// locations and extra, and the control sockets of the VMs it runs, any of
// which would let a step act outside the sandbox through the daemon.
func controlSockets(extra []string) []string {
	out := []string{"/var/run/jerboad.sock", "/run/jerboad.sock",
		filepath.Join(os.TempDir(), fmt.Sprintf("jerboa-run-%d", os.Getuid()))}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".jerboa"))
	}
	for _, p := range extra {
		if abs, err := filepath.Abs(p); err == nil && p != "" {
			out = append(out, abs)
		}
	}
	return out
}

// brokerSockets returns the well-known Unix sockets, and directories of
// them, of host services that run commands or act for the user on request:
// the D-Bus buses (systemd, portals, polkit-backed services), the systemd
// user manager, container and VM engines, X11 (keystrokes into a terminal),
// tmux and screen (keystrokes into a shell), and the GnuPG agent. A confined
// step that reached one could have it act outside the sandbox, as with the
// daemon. Paths that do not exist are left out by the launcher. The SSH
// agent stays reachable: package managers fetch private dependencies over
// SSH. This is a list of what is known, not a guarantee: other sockets on
// the host stay reachable.
func brokerSockets() []string {
	uid := os.Getuid()
	runtimeDirs := []string{fmt.Sprintf("/run/user/%d", uid)}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && filepath.IsAbs(xdg) && xdg != runtimeDirs[0] {
		runtimeDirs = append(runtimeDirs, xdg)
	}
	var out []string
	for _, rt := range runtimeDirs {
		for _, name := range []string{
			"bus", "dbus-1", "at-spi", "systemd",
			"docker.sock", "docker", "podman", "containerd-rootless", "buildkit", "libvirt",
			"gnupg", "snapd-session-agent.socket",
		} {
			out = append(out, filepath.Join(rt, name))
		}
	}
	out = append(out,
		"/run/dbus/system_bus_socket", "/var/run/dbus/system_bus_socket",
		"/run/docker.sock", "/var/run/docker.sock", "/run/docker",
		"/run/podman", "/run/containerd", "/run/buildkit",
		"/run/libvirt", "/var/run/libvirt",
		"/var/snap/lxd/common/lxd/unix.socket", "/var/lib/lxd/unix.socket",
		"/var/lib/incus/unix.socket", "/run/incus",
		"/run/snapd.socket", "/run/snapd-snap.socket",
		"/tmp/.X11-unix",
		fmt.Sprintf("/tmp/tmux-%d", uid), filepath.Join(os.TempDir(), fmt.Sprintf("tmux-%d", uid)),
	)
	if u, err := user.Current(); err == nil && u.Username != "" {
		out = append(out, filepath.Join("/run/screen", "S-"+u.Username), filepath.Join("/var/run/screen", "S-"+u.Username))
	}
	if home, err := os.UserHomeDir(); err == nil {
		// Docker Desktop, Colima, OrbStack, Rancher Desktop, Lima and Podman
		// machines on macOS; the GnuPG agent's sockets live in its home.
		for _, rel := range []string{
			".docker/run", ".colima", ".orbstack/run", ".rd/docker.sock", ".lima",
			".local/share/containers/podman/machine", ".gnupg",
		} {
			out = append(out, filepath.Join(home, rel))
		}
	}
	return out
}

// brokerMasks returns the brokerSockets to mask for st, leaving out any
// that holds the directories the step works in: masking it would hide them.
func brokerMasks(st Step, tmp string) []string {
	var out []string
	for _, p := range brokerSockets() {
		if !within(st.Dir, p) && !within(tmp, p) && !slices.ContainsFunc(st.Writable, func(w string) bool { return within(w, p) }) {
			out = append(out, p)
		}
	}
	return out
}

// brokerAddresses are the environment variables that point clients at the
// services brokerSockets hides, some of them at addresses no path mask
// covers (abstract sockets, TCP).
var brokerAddresses = []string{"DBUS_SESSION_BUS_ADDRESS", "DBUS_SYSTEM_BUS_ADDRESS", "DOCKER_HOST", "CONTAINER_HOST"}

// withoutBrokerAddresses returns env without brokerAddresses.
func withoutBrokerAddresses(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(brokerAddresses, name) {
			out = append(out, kv)
		}
	}
	return out
}

// bwrapArgs returns the bubblewrap command line that runs name with args as
// st: the host root read-only, fresh /dev and /proc, the project, the
// writable directories and the step's temporary directory bound read-write
// except for the read-only files in them, the hidden paths and the control
// sockets masked, and no network unless allowed.
func bwrapArgs(bwrap string, st Step, hidden, tmp, sockets []string, name string, args []string) []string {
	argv := []string{bwrap,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
	}
	if !st.Network {
		argv = append(argv, "--unshare-net")
	}
	for _, p := range hidden {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			argv = append(argv, "--tmpfs", p)
		} else {
			argv = append(argv, "--ro-bind", "/dev/null", p)
		}
	}
	for _, w := range append(append(append([]string{}, tmp...), st.Writable...), st.Dir) {
		argv = append(argv, "--bind", w, w)
	}
	// Bound over the read-write directories above. A file that does not
	// exist cannot be bound, so a step could still create it.
	for _, p := range st.ReadOnly {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			r := realPath(p)
			argv = append(argv, "--ro-bind", r, r)
		}
	}
	// Mount over the control sockets last, so no bind above uncovers them.
	for _, p := range sockets {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
		case fi.IsDir():
			argv = append(argv, "--tmpfs", realPath(p))
		default:
			argv = append(argv, "--ro-bind", "/dev/null", realPath(p))
		}
	}
	argv = append(argv, "--chdir", st.Dir, "--", name)
	return append(argv, args...)
}

// seatbeltProfile returns the sandbox-exec profile that confines st: every
// write outside the project, the writable directories, /dev and the
// temporary directories is denied, so are writes to the read-only files
// (creating them included), reads of the hidden paths and
// connections to the control sockets and Apple events, and so is the
// network unless allowed
// (other local connections stay open).
func seatbeltProfile(st Step, hidden, tmp, sockets []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	b.WriteString("(allow file-write* (subpath \"/dev\")")
	for _, p := range append(append(append([]string{}, tmp...), st.Writable...), st.Dir) {
		fmt.Fprintf(&b, " (subpath %s)", sbplString(realPath(p)))
	}
	b.WriteString(")\n")
	for _, p := range st.ReadOnly {
		fmt.Fprintf(&b, "(deny file-write* (literal %s))\n", sbplString(realFilePath(p)))
	}
	for _, p := range hidden {
		fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", sbplString(realPath(p)))
	}
	if !st.Network {
		// Only remote addresses: a (local ip "localhost:*") rule would
		// match every outbound connection.
		b.WriteString("(deny network*)\n")
		b.WriteString("(allow network* (remote ip \"localhost:*\"))\n")
		b.WriteString("(allow network* (remote unix-socket))\n")
	}
	// Last, so they win over the allows above.
	for _, p := range sockets {
		fmt.Fprintf(&b, "(deny network-outbound (remote unix-socket (subpath %s)))\n", sbplString(realPath(p)))
	}
	// Apple events drive other applications (a Terminal window runs what it
	// is sent), outside the profile.
	b.WriteString("(deny appleevent-send)\n")
	return b.String()
}

// sbplString quotes s as a sandbox profile string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// realFilePath is realPath for a file that may not exist yet: its
// directory is resolved instead.
func realFilePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Join(realPath(filepath.Dir(p)), filepath.Base(p))
}

// realPath resolves symlinks (macOS's /tmp and /var), which sandbox-exec
// matches against.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
