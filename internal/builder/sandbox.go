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
// read the host but write only to the project directory, the tools' caches
// and a temporary directory, cannot read the usual credential directories,
// and, unless allowed, cannot reach the network.
//
// The mechanism is bubblewrap (bwrap) on Linux and sandbox-exec on macOS.
// Where neither is available the step runs directly, with Jerboa's
// credentials removed from its environment and a private temporary
// directory, and a warning says so.
type Sandbox struct {
	// Off runs build steps directly on the host ([build] sandbox = false).
	Off bool
}

// Step is one build step to confine.
type Step struct {
	// Dir is the step's working directory, the project; it stays writable.
	Dir string
	// Writable lists further directories the step writes to (tool caches).
	// They are created when missing.
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
	st.Writable = append(existingDirs(defaultWritable()), st.Writable...)

	if s.Off {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the build tool the project asks for
		cmd.Dir = st.Dir
		cmd.Env = env
		return cmd, func() {}, nil
	}

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
	argv, ok := confine(st, tmp, name, args)
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

// defaultWritable lists the caches any build step may write to: the user
// cache directory and the language tools' homes.
func defaultWritable() []string {
	var out []string
	if d, err := os.UserCacheDir(); err == nil {
		out = append(out, d)
	}
	home, _ := os.UserHomeDir()
	for _, kv := range []struct{ env, def string }{
		{"npm_config_cache", ".npm"},
		{"CARGO_HOME", ".cargo"},
		{"RUSTUP_HOME", ".rustup"},
		{"GOPATH", "go"},
		{"GOCACHE", ""},
		{"GOMODCACHE", ""},
		{"PIP_CACHE_DIR", ""},
		{"CARGO_TARGET_DIR", ""},
	} {
		switch v := os.Getenv(kv.env); {
		case v != "":
			out = append(out, filepath.SplitList(v)...)
		case kv.def != "" && home != "":
			out = append(out, filepath.Join(home, kv.def))
		}
	}
	return out
}

func existingDirs(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// bwrapArgs returns the bubblewrap command line that runs name with args as
// st: the host root read-only, fresh /dev and /proc, the project, the
// writable directories and the step's temporary directory bound read-write,
// the hidden paths masked, and no network unless allowed.
func bwrapArgs(bwrap string, st Step, hidden, tmp []string, name string, args []string) []string {
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
	argv = append(argv, "--chdir", st.Dir, "--", name)
	return append(argv, args...)
}

// seatbeltProfile returns the sandbox-exec profile that confines st: every
// write outside the project, the writable directories, /dev and the
// temporary directories is denied, so are reads of the hidden paths, and
// so is the network unless allowed (local connections stay open).
func seatbeltProfile(st Step, hidden, tmp []string) string {
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

// npmCache returns npm's cache directory.
func npmCache() string {
	if d := os.Getenv("npm_config_cache"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".npm")
}

// cargoDirs returns the directories cargo writes to outside the project.
func cargoDirs() []string {
	if d := os.Getenv("CARGO_HOME"); d != "" {
		return []string{d}
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, ".cargo")}
}

// goDirs returns the build and module caches go writes to, as go reports
// them for dir and env. go itself may download a toolchain into the module
// cache, so it must be writable.
func goDirs(ctx context.Context, dir string, env []string) []string {
	cmd := exec.CommandContext(ctx, "go", "env", "GOCACHE", "GOMODCACHE")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if d := strings.TrimSpace(line); filepath.IsAbs(d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}
