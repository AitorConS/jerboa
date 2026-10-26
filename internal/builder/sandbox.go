package builder

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Sandbox confines the build steps that run project code on the host: the
// [build] run commands and the language tools (npm, pip, cargo, go build),
// whose build scripts and plugins run arbitrary code. Confined, a step can
// read the host but write only to the project directory, caches of its own
// and a temporary directory, cannot read the usual credential directories,
// cannot connect to the Jerboa daemon or its VMs' control sockets, and,
// unless allowed, cannot reach the network.
//
// The mechanism is bubblewrap (bwrap) on Linux and sandbox-exec on macOS.
// Where neither is available the step runs directly, with Jerboa's
// credentials removed from its environment and a private temporary
// directory, and a warning says so.
type Sandbox struct {
	// Off runs build steps directly on the host ([build] sandbox = false).
	Off bool
	// Sockets lists further Unix sockets, or directories of them, a step
	// may not connect to: the endpoint the CLI reaches the daemon at.
	Sockets []string
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

	caches, cacheEnv, err := toolCaches()
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	st.Writable = append(caches, st.Writable...)
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
	argv, ok := confine(st, tmp, controlSockets(s.Sockets), name, args)
	if !ok {
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
		return "build steps run directly on the host ([build] sandbox = false)"
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
func toolCaches() ([]string, []string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, nil, fmt.Errorf("cache directory: %w", err)
	}
	base = filepath.Join(base, "jerboa", "build-sandbox")
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

// bwrapArgs returns the bubblewrap command line that runs name with args as
// st: the host root read-only, fresh /dev and /proc, the project, the
// writable directories and the step's temporary directory bound read-write,
// the hidden paths and the control sockets masked, and no network unless
// allowed.
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
// temporary directories is denied, so are reads of the hidden paths and
// connections to the control sockets, and so is the network unless allowed
// (other local connections stay open).
func seatbeltProfile(st Step, hidden, tmp, sockets []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	b.WriteString("(allow file-write* (subpath \"/dev\")")
	for _, p := range append(append(append([]string{}, tmp...), st.Writable...), st.Dir) {
		fmt.Fprintf(&b, " (subpath %s)", sbplString(realPath(p)))
	}
	b.WriteString(")\n")
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
	return b.String()
}

// sbplString quotes s as a sandbox profile string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// realPath resolves symlinks (macOS's /tmp and /var), which sandbox-exec
// matches against.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
