//go:build !windows

package builder

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the language drivers against toolchains that fail the ways
// real ones do: the tool is not installed, it exits non-zero, it claims
// success without producing anything, it hangs. A driver must report the
// failure with the tool's own output available to the user, must never
// return a result for a build that did not happen, and must leave the
// project so that the next build starts clean.

// fakeTool puts a script named name first on PATH. The script appends its
// arguments to <dir>/<name>.calls and then runs body.
func fakeTool(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + dir + "/" + name + ".calls'\n" + body + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700)) //nolint:gosec // the test's own script must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	return dir
}

// calls returns the argument lines the fake tool was run with.
func calls(t *testing.T, toolDir, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(toolDir, name+".calls"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// noTools leaves PATH without any toolchain.
func noTools(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// direct runs build steps without the sandbox launcher, so the fake tool is
// what gets executed.
func direct(out *bytes.Buffer) Options {
	return Options{Sandbox: Sandbox{Off: true}, Output: out}
}

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return dir
}

// --- Go ---

func TestFaults_GoToolchainMissing(t *testing.T) {
	noTools(t)
	dir := project(t, map[string]string{"go.mod": "module x\n", "main.go": "package main\nfunc main() {}\n"})
	var out bytes.Buffer
	res, err := (&GoDriver{}).Build(context.Background(), dir, direct(&out))
	require.ErrorContains(t, err, "go build")
	assert.Contains(t, err.Error(), "executable file not found", "the error must say the toolchain is missing")
	assert.Empty(t, res.BinaryPath)
}

func TestFaults_GoCompilerFails(t *testing.T) {
	tool := fakeTool(t, "go", "echo './main.go:3:1: syntax error: unexpected }' >&2\nexit 1")
	dir := project(t, map[string]string{"go.mod": "module x\n"})
	var out bytes.Buffer
	res, err := (&GoDriver{}).Build(context.Background(), dir, direct(&out))
	require.ErrorContains(t, err, "go build")
	assert.Contains(t, err.Error(), "exit status 1")
	assert.Contains(t, out.String(), "syntax error: unexpected }", "the compiler's message must reach the user")
	assert.Empty(t, res.BinaryPath)
	require.Len(t, calls(t, tool, "go"), 1, "a failed build must not be retried silently")
}

// The compiler reports success but there is no binary: a library package, a
// wrapper script, a full disk it did not notice. The driver must not hand
// back the path of a file that is not there, or of a previous build's.
func TestFaults_GoCompilerProducesNothing(t *testing.T) {
	fakeTool(t, "go", "exit 0")
	dir := project(t, map[string]string{"go.mod": "module x\n"})
	var out bytes.Buffer
	res, err := (&GoDriver{}).Build(context.Background(), dir, direct(&out))
	require.Error(t, err, "a build that produced no binary was reported as a success: %+v", res)
	assert.Empty(t, res.BinaryPath)

	// A binary left by an earlier build must not be passed off as this one's.
	stale := filepath.Join(dir, ".jerboa-build-binary")
	require.NoError(t, os.WriteFile(stale, []byte("yesterday's build"), 0o700)) //nolint:gosec // stands in for an executable
	res, err = (&GoDriver{}).Build(context.Background(), dir, direct(&out))
	require.Error(t, err, "a stale binary was returned as the result of a build that produced none")
	assert.Empty(t, res.BinaryPath)
}

// A build that is canceled stops the compiler and returns at once.
func TestFaults_GoBuildCanceled(t *testing.T) {
	fakeTool(t, "go", "exec sleep 60")
	dir := project(t, map[string]string{"go.mod": "module x\n"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		_, err := (&GoDriver{}).Build(ctx, dir, direct(&out))
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the build kept running after it was canceled")
	}
}

// The compiler, which runs code from the project and its dependencies, gets
// the build's settings and none of Jerboa's credentials.
func TestFaults_GoBuildEnvironment(t *testing.T) {
	tool := fakeTool(t, "go", "env > \"$(dirname \"$0\")/env\"\nprintf binary > .jerboa-build-binary")
	t.Setenv("JERBOA_AUTH_TOKEN", "daemon-secret")
	t.Setenv("JERBOA_REGISTRY_TOKEN", "registry-secret")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "oidc-secret")
	dir := project(t, map[string]string{"go.mod": "module x\n"})
	opts := direct(&bytes.Buffer{})
	opts.Platform = Platform{OS: "linux", Arch: "arm64"}
	opts.Env = []string{"MY_FLAG=1"}
	opts.BuildArgs = []string{"-tags", "netgo"}
	opts.Entrypoint = "./cmd/server"
	res, err := (&GoDriver{}).Build(context.Background(), dir, opts)
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(res.BinaryPath))

	env, err := os.ReadFile(filepath.Join(tool, "env"))
	require.NoError(t, err)
	for _, secret := range []string{"daemon-secret", "registry-secret", "oidc-secret"} {
		assert.NotContains(t, string(env), secret)
	}
	for _, want := range []string{"GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "MY_FLAG=1"} {
		assert.Contains(t, string(env), want)
	}
	got := calls(t, tool, "go")
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0], "-tags netgo ./cmd/server"), "arguments: %s", got[0])
}

// --- Rust ---

func TestFaults_RustToolchain(t *testing.T) {
	cargoToml := "[package]\nname = \"svc\"\n"
	t.Run("cargo missing", func(t *testing.T) {
		noTools(t)
		dir := project(t, map[string]string{"Cargo.toml": cargoToml})
		_, err := (&RustDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.ErrorContains(t, err, "rust driver")
		assert.Contains(t, err.Error(), "executable file not found")
	})
	t.Run("cargo fails", func(t *testing.T) {
		fakeTool(t, "cargo", "echo 'error[E0425]: cannot find value' >&2\nexit 101")
		dir := project(t, map[string]string{"Cargo.toml": cargoToml})
		var out bytes.Buffer
		res, err := (&RustDriver{}).Build(context.Background(), dir, direct(&out))
		require.ErrorContains(t, err, "cargo build")
		assert.Contains(t, out.String(), "error[E0425]")
		assert.Empty(t, res.BinaryPath)
	})
	t.Run("target not installed leaves no binary", func(t *testing.T) {
		fakeTool(t, "cargo", "exit 0")
		dir := project(t, map[string]string{"Cargo.toml": cargoToml})
		res, err := (&RustDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.ErrorContains(t, err, "binary not found")
		assert.Contains(t, err.Error(), "svc")
		assert.Empty(t, res.BinaryPath)
	})
	for name, manifest := range map[string]string{"not toml": "[package\nname=", "no package name": "[package]\nversion = \"1\"\n", "name of the wrong type": "[package]\nname = 7\n"} {
		t.Run("manifest/"+name, func(t *testing.T) {
			fakeTool(t, "cargo", "exit 0")
			dir := project(t, map[string]string{"Cargo.toml": manifest})
			res, err := (&RustDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
			require.ErrorContains(t, err, "Cargo.toml")
			assert.Empty(t, res.BinaryPath)
		})
	}
	t.Run("built binary is copied out whole", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		opts := direct(&bytes.Buffer{})
		opts.Platform = Platform{OS: "linux", Arch: "amd64"}
		target := opts.Platform.RustTarget()
		tool := fakeTool(t, "cargo", "mkdir -p target/"+target+"/release && printf 'rust-binary' > target/"+target+"/release/svc")
		dir := project(t, map[string]string{"Cargo.toml": cargoToml})
		res, err := (&RustDriver{}).Build(context.Background(), dir, opts)
		require.NoError(t, err)
		data, err := os.ReadFile(res.BinaryPath)
		require.NoError(t, err)
		assert.Equal(t, "rust-binary", string(data))
		assert.Contains(t, calls(t, tool, "cargo")[0], "--target "+target)
	})
	t.Run("binary cannot be read", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file modes")
		}
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		opts := direct(&bytes.Buffer{})
		opts.Platform = Platform{OS: "linux", Arch: "amd64"}
		target := opts.Platform.RustTarget()
		fakeTool(t, "cargo", "mkdir -p target/"+target+"/release && printf x > target/"+target+"/release/svc && chmod 000 target/"+target+"/release/svc")
		dir := project(t, map[string]string{"Cargo.toml": cargoToml})
		res, err := (&RustDriver{}).Build(context.Background(), dir, opts)
		require.ErrorContains(t, err, "copy binary")
		assert.Empty(t, res.BinaryPath)
		entries, err := os.ReadDir(tmp)
		require.NoError(t, err)
		assert.Empty(t, entries, "a failed copy left its temporary file behind")
	})
}

// --- Node ---

func TestFaults_NodeDependencies(t *testing.T) {
	pkgJSON := `{"name":"svc","main":"server.js","engines":{"node":">=22.1"}}`
	t.Run("npm missing", func(t *testing.T) {
		noTools(t)
		dir := project(t, map[string]string{"package.json": pkgJSON})
		_, err := (&NodeDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.ErrorContains(t, err, "node driver")
		assert.Contains(t, err.Error(), "executable file not found")
	})
	t.Run("npm fails and the next build tries again", func(t *testing.T) {
		tool := fakeTool(t, "npm", "echo 'npm ERR! code E404' >&2\nexit 1")
		dir := project(t, map[string]string{"package.json": pkgJSON})
		var out bytes.Buffer
		res, err := (&NodeDriver{}).Build(context.Background(), dir, direct(&out))
		require.ErrorContains(t, err, "npm install")
		assert.Contains(t, out.String(), "npm ERR! code E404")
		assert.Empty(t, res.Packages)
		_, statErr := os.Stat(filepath.Join(dir, "node_modules", ".jerboa-deps-stamp"))
		require.ErrorIs(t, statErr, os.ErrNotExist, "a failed install was recorded as done")

		_, err = (&NodeDriver{}).Build(context.Background(), dir, direct(&out))
		require.Error(t, err)
		assert.Len(t, calls(t, tool, "npm"), 2, "the install must be attempted again after a failure")
	})
	t.Run("install runs once, without scripts, until the manifest changes", func(t *testing.T) {
		tool := fakeTool(t, "npm", "exit 0")
		dir := project(t, map[string]string{"package.json": pkgJSON})
		res, err := (&NodeDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.NoError(t, err)
		assert.Equal(t, "server.js", res.Entrypoint)
		assert.Equal(t, []string{"node:22"}, res.Packages)
		_, err = (&NodeDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.NoError(t, err)
		got := calls(t, tool, "npm")
		require.Len(t, got, 1, "an unchanged project was reinstalled")
		assert.Equal(t, "install --omit=dev --no-audit --no-fund --ignore-scripts", got[0])

		require.NoError(t, os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o600))
		opts := direct(&bytes.Buffer{})
		opts.InstallScripts = true
		_, err = (&NodeDriver{}).Build(context.Background(), dir, opts)
		require.NoError(t, err)
		got = calls(t, tool, "npm")
		require.Len(t, got, 2, "a new lockfile must trigger an install")
		assert.Equal(t, "ci --omit=dev --no-audit --no-fund", got[1])
	})
	t.Run("entrypoint that does not exist", func(t *testing.T) {
		tool := fakeTool(t, "npm", "exit 0")
		dir := project(t, map[string]string{"package.json": pkgJSON})
		opts := direct(&bytes.Buffer{})
		opts.Entrypoint = "missing.js"
		_, err := (&NodeDriver{}).Build(context.Background(), dir, opts)
		require.ErrorContains(t, err, "entrypoint missing.js not found")
		assert.Empty(t, calls(t, tool, "npm"), "dependencies were installed for a build that cannot succeed")
	})
	t.Run("manifest cannot be read", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file modes")
		}
		tool := fakeTool(t, "npm", "exit 0")
		dir := project(t, map[string]string{"package.json": pkgJSON})
		require.NoError(t, os.Chmod(filepath.Join(dir, "package.json"), 0o000))
		_, err := (&NodeDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.ErrorContains(t, err, "package.json")
		assert.Empty(t, calls(t, tool, "npm"))
	})
	t.Run("stamp cannot be written", func(t *testing.T) {
		fakeTool(t, "npm", "exit 0")
		dir := project(t, map[string]string{"package.json": pkgJSON, "node_modules": "a file where the directory should be"})
		_, err := (&NodeDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.Error(t, err)
	})
}

// Damaged manifests fall back to defaults instead of failing or producing a
// runtime reference no registry has.
func TestFaults_NodeManifestDamaged(t *testing.T) {
	for name, manifest := range map[string]string{
		"not json":          "{name: svc",
		"empty":             "",
		"array":             "[]",
		"engines is text":   `{"engines":"node 20"}`,
		"main is a number":  `{"main":7}`,
		"wildcard engine":   `{"engines":{"node":"*"}}`,
		"empty engine":      `{"engines":{"node":""}}`,
		"only an operator":  `{"engines":{"node":">="}}`,
		"null main":         `{"main":null}`,
		"binary":            "\x00\x01\x02",
		"deeply nested":     strings.Repeat("[", 10000),
		"engines is a list": `{"engines":["node"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := project(t, map[string]string{"package.json": manifest})
			version, err := nodeVersionFromPackageJSON(dir)
			require.NoError(t, err)
			assert.Equal(t, "20", version)
			entry, err := nodeEntrypoint(dir, "")
			require.NoError(t, err)
			assert.Equal(t, "index.js", entry)
		})
	}
}

// --- Python ---

func TestFaults_PythonDependencies(t *testing.T) {
	files := map[string]string{"requirements.txt": "flask==3.0\n", "main.py": "print('hi')\n"}
	t.Run("pip missing", func(t *testing.T) {
		noTools(t)
		dir := project(t, files)
		_, err := (&PythonDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.ErrorContains(t, err, "python driver")
		assert.Contains(t, err.Error(), "executable file not found")
		requireNoStaging(t, dir)
	})
	t.Run("pip fails and the installed packages stay", func(t *testing.T) {
		tool := fakeTool(t, "pip", "echo 'ERROR: No matching distribution found for flask==3.0' >&2\nexit 1")
		dir := project(t, files)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "packages", "flask"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "packages", "flask", "__init__.py"), []byte("old"), 0o600))
		var out bytes.Buffer
		res, err := (&PythonDriver{}).Build(context.Background(), dir, direct(&out))
		require.ErrorContains(t, err, "pip install")
		assert.Contains(t, out.String(), "No matching distribution")
		assert.Empty(t, res.Packages)
		data, err := os.ReadFile(filepath.Join(dir, "packages", "flask", "__init__.py"))
		require.NoError(t, err, "a failed install removed the packages that were there")
		assert.Equal(t, "old", string(data))
		requireNoStaging(t, dir)
		_, statErr := os.Stat(filepath.Join(dir, "packages", ".jerboa-deps-stamp"))
		require.ErrorIs(t, statErr, os.ErrNotExist)
		assert.Len(t, calls(t, tool, "pip"), 1)
	})
	t.Run("an interrupted swap is not built over", func(t *testing.T) {
		fakeTool(t, "pip", "exit 0")
		dir := project(t, files)
		// What a build killed between the two renames leaves behind.
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "packages.previous", "flask"), 0o755))
		_, err := (&PythonDriver{}).Build(context.Background(), dir, direct(&bytes.Buffer{}))
		require.ErrorContains(t, err, "recovery needed")
		assert.Contains(t, err.Error(), "packages.previous")
		_, statErr := os.Stat(filepath.Join(dir, "packages.previous", "flask"))
		require.NoError(t, statErr, "the only copy of the previous packages was removed")
		requireNoStaging(t, dir)
	})
	t.Run("install replaces the packages and runs once", func(t *testing.T) {
		tool := fakeTool(t, "pip", `while [ "$1" != "--target" ]; do shift; done; mkdir -p "$2/flask" && printf new > "$2/flask/__init__.py"`)
		dir := project(t, files)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "packages", "gone"), 0o755))
		opts := direct(&bytes.Buffer{})
		opts.Platform = Platform{OS: "linux", Arch: "arm64"}
		res, err := (&PythonDriver{}).Build(context.Background(), dir, opts)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"PYTHONPATH": "/packages"}, res.Env)
		assert.Equal(t, "main.py", res.Entrypoint)
		data, err := os.ReadFile(filepath.Join(dir, "packages", "flask", "__init__.py"))
		require.NoError(t, err)
		assert.Equal(t, "new", string(data))
		_, statErr := os.Stat(filepath.Join(dir, "packages", "gone"))
		require.ErrorIs(t, statErr, os.ErrNotExist, "packages of the previous requirements were kept")
		_, statErr = os.Stat(filepath.Join(dir, "packages.previous"))
		require.ErrorIs(t, statErr, os.ErrNotExist)
		requireNoStaging(t, dir)

		_, err = (&PythonDriver{}).Build(context.Background(), dir, opts)
		require.NoError(t, err)
		got := calls(t, tool, "pip")
		require.Len(t, got, 1, "unchanged requirements were installed again")
		assert.Contains(t, got[0], "--platform manylinux_2_17_aarch64")
		assert.Contains(t, got[0], "--only-binary :all:", "source distributions would be compiled for the host, not the guest")

		// Another target architecture needs other wheels.
		opts.Platform = Platform{OS: "linux", Arch: "amd64"}
		_, err = (&PythonDriver{}).Build(context.Background(), dir, opts)
		require.NoError(t, err)
		require.Len(t, calls(t, tool, "pip"), 2)
	})
	t.Run("entrypoint that does not exist", func(t *testing.T) {
		tool := fakeTool(t, "pip", "exit 0")
		dir := project(t, files)
		opts := direct(&bytes.Buffer{})
		opts.Entrypoint = "app/missing.py"
		_, err := (&PythonDriver{}).Build(context.Background(), dir, opts)
		require.ErrorContains(t, err, "entrypoint app/missing.py not found")
		assert.Empty(t, calls(t, tool, "pip"))
	})
}

func requireNoStaging(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, ".python-packages-*"))
	require.NoError(t, err)
	require.Empty(t, left, "the staging directory of a failed install was left in the project")
}

// --- language detection and configuration ---

// A project directory that is missing, unreadable or a file is "no language
// detected", an error the caller can report, for every driver.
func TestFaults_DetectInUnusableDirectories(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	for _, dir := range []string{filepath.Join(t.TempDir(), "absent"), file, ""} {
		for _, d := range AvailableDrivers() {
			assert.False(t, d.Detect(dir), "%s detected in %q", d.Lang(), dir)
		}
		_, err := DetectLanguage(dir, LangUnknown)
		require.Error(t, err, "directory %q", dir)
	}
}

// A configuration file that cannot be read is an error naming the file, and
// one that is absent is "no configuration" only when it was not asked for.
func TestFaults_ConfigUnreadable(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	require.NoError(t, err)
	assert.Nil(t, cfg)

	_, err = LoadConfigFile(filepath.Join(dir, "custom.toml"))
	require.ErrorContains(t, err, "custom.toml")

	require.NoError(t, os.Mkdir(filepath.Join(dir, ConfigFileName), 0o700))
	_, err = LoadConfig(dir)
	require.ErrorContains(t, err, ConfigFileName, "a directory in place of the file was taken for no configuration")

	if os.Geteuid() == 0 {
		return
	}
	locked := t.TempDir()
	path := filepath.Join(locked, ConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte("[build]\nlang = \"go\"\n"), 0o000))
	_, err = LoadConfig(locked)
	require.ErrorContains(t, err, path)
}

// FuzzParseConfig feeds arbitrary bytes to the build configuration parser. A file it
// accepts must hold a language the builder knows and settings that pass the
// same validation again: nothing half-validated reaches a build.
func FuzzParseConfig(f *testing.F) {
	for _, seed := range []string{
		"", "[build]\nlang = \"go\"\n", "[build]\nlang = \"cobol\"\n", "[build]\nlang = 7\n",
		"[run]\nmemory = \"512M\"\nports = [\"8080:80\"]\n", "[run]\nmemory = \"-1G\"\n", "[run]\nports = [\"0:0\"]\n",
		"[program]\npath = \"/bin/app\"\nargs = [\"-v\"]\n", "[build]\nlang = \"raw\"\n[program]\npath = \"../../etc/passwd\"\n",
		"[[stage]]\nname = \"a\"\n[[stage]]\nname = \"a\"\n", "[env]\nA = \"1\"\n\"\" = \"x\"\n", "[build\n", "\x00", "a = [[[[[[",
		"[build]\npkgs = [\"node:20\", \"\"]\nexclude = [\"**\"]\ninclude = [\"[\"]\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := parseConfig("unikernel.toml", data)
		if err != nil {
			if cfg != nil {
				t.Fatal("a refused configuration was returned")
			}
			if !strings.Contains(err.Error(), "unikernel.toml") {
				t.Fatalf("the error does not name the file: %v", err)
			}
			return
		}
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("an accepted configuration fails validation the second time: %v", err)
		}
		if cfg.Build.Lang != "" {
			if _, err := ParseLang(cfg.Build.Lang); err != nil {
				t.Fatalf("accepted with unknown language %q", cfg.Build.Lang)
			}
		}
		_ = cfg.LangHint()
		_ = cfg.HasStages()
		_ = cfg.BuildSandbox()
	})
}
