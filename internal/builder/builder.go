package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Lang represents a programming language supported by the build system.
type Lang int

const (
	// LangUnknown indicates the language could not be determined.
	LangUnknown Lang = iota
	// LangGo indicates a Go project.
	LangGo
	// LangNode indicates a Node.js project.
	LangNode
	// LangPython indicates a Python project.
	LangPython
	// LangRust indicates a Rust project.
	LangRust
	// LangRaw indicates a generic, driver-agnostic build (see RawDriver).
	LangRaw
)

// String returns the human-readable name of the language.
func (l Lang) String() string {
	switch l {
	case LangGo:
		return "go"
	case LangNode:
		return "node"
	case LangPython:
		return "python"
	case LangRust:
		return "rust"
	case LangRaw:
		return "raw"
	default:
		return "unknown"
	}
}

// ParseLang parses a language string (case-insensitive) into a Lang.
func ParseLang(s string) (Lang, error) {
	switch strings.ToLower(s) {
	case "go":
		return LangGo, nil
	case "node", "nodejs":
		return LangNode, nil
	case "python", "py":
		return LangPython, nil
	case "rust":
		return LangRust, nil
	case "raw":
		return LangRaw, nil
	default:
		return LangUnknown, fmt.Errorf("unsupported language %q: use go, node, python, rust, or raw", s)
	}
}

// BuildResult holds the output of a successful language build.
type BuildResult struct {
	// BinaryPath is the path to the compiled ELF binary.
	// For interpreted languages (Node, Python), this may be empty and
	// SourceDir should be used instead.
	BinaryPath string
	// SourceDir is the directory containing the application source files
	// to include in the image (used for interpreted languages).
	SourceDir string
	// Entrypoint is the command or script that should be used as the program entrypoint.
	Entrypoint string
	// Packages lists language runtime packages that should be included in the image
	// (e.g. "node:20" for Node.js projects).
	Packages []string
	// Env holds runtime environment variables required by this build (e.g. PYTHONPATH
	// when pip installed packages into a non-default directory).
	Env map[string]string
}

// Driver is the interface that each language builder must implement.
type Driver interface {
	// Detect checks whether the given directory contains a project of this language.
	// Returns true if the language markers are found.
	Detect(dir string) bool

	// Build compiles the project in dir and returns the path to the resulting binary.
	Build(ctx context.Context, dir string, opts Options) (BuildResult, error)

	// Lang returns the language this driver builds.
	Lang() Lang
}

// Options contains language-independent build options passed to every driver.
type Options struct {
	// Entrypoint overrides the default entrypoint for the language (e.g. "cmd/server/main.go" for Go).
	Entrypoint string
	// BuildArgs are extra arguments passed to the language build tool.
	BuildArgs []string
	// Env is additional environment variables for the build process.
	Env []string
	// PkgFiles are pre-resolved package file paths to include in the image.
	PkgFiles []string
	// Platform is the target platform for cross-compilation. Defaults to the current platform.
	Platform Platform
}

// DetectLanguage inspects dir and returns the language detected.
// If multiple markers exist and langHint is non-zero, langHint takes precedence.
// Returns LangUnknown and an error if detection is ambiguous and no hint is given.
func DetectLanguage(dir string, langHint Lang) (Lang, error) {
	if langHint != LangUnknown {
		return langHint, nil
	}

	var detected []Lang
	drivers := AvailableDrivers()
	for _, d := range drivers {
		if d.Detect(dir) {
			detected = append(detected, d.Lang())
		}
	}

	switch len(detected) {
	case 0:
		return LangUnknown, fmt.Errorf("no language detected in %s: specify --lang explicitly", dir)
	case 1:
		return detected[0], nil
	default:
		names := make([]string, len(detected))
		for i, l := range detected {
			names[i] = l.String()
		}
		return LangUnknown, fmt.Errorf("ambiguous language detected (%s): specify --lang explicitly", strings.Join(names, ", "))
	}
}

// GetDriver returns the Driver for the given language, or an error if unavailable.
func GetDriver(lang Lang) (Driver, error) {
	for _, d := range AvailableDrivers() {
		if d.Lang() == lang {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no build driver for language %q", lang)
}

// AvailableDrivers returns all registered build drivers.
func AvailableDrivers() []Driver {
	return []Driver{
		&GoDriver{},
		&NodeDriver{},
		&PythonDriver{},
		&RustDriver{},
		&RawDriver{},
	}
}

// RawDriver is a language-agnostic build mode for runtimes without a
// dedicated driver (Java, .NET, Ruby, PHP, ...). It performs no compilation
// itself — [build] run handles build steps, and [program] in unikernel.toml
// names the runtime binary (resolved from --pkg files) and its arguments.
// Never auto-detected; opt-in via lang = "raw".
type RawDriver struct{}

// Lang returns LangRaw.
func (r *RawDriver) Lang() Lang { return LangRaw }

// Detect always returns false — raw mode is opt-in only.
func (r *RawDriver) Detect(dir string) bool { return false }

// Build returns dir as the source directory, deferring all program/argument
// resolution to the caller's [program] handling.
func (r *RawDriver) Build(ctx context.Context, dir string, opts Options) (BuildResult, error) {
	return BuildResult{SourceDir: dir}, nil
}

// NodeDriver builds Node.js projects into unikernel images.
type NodeDriver struct{}

// Lang returns LangNode.
func (n *NodeDriver) Lang() Lang { return LangNode }

// Detect checks for package.json in dir.
func (n *NodeDriver) Detect(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "package.json"))
	return err == nil
}

// Build runs npm install and returns the source directory with node entrypoint.
// The result includes Packages=["node:20"] so the caller can resolve the Node runtime.
func (n *NodeDriver) Build(ctx context.Context, dir string, opts Options) (BuildResult, error) {
	entrypoint, err := nodeEntrypoint(dir, opts.Entrypoint)
	if err != nil {
		return BuildResult{}, err
	}

	nodeVersion, err := nodeVersionFromPackageJSON(dir)
	if err != nil {
		return BuildResult{}, err
	}

	if _, err := os.Stat(filepath.Join(dir, "node_modules")); os.IsNotExist(err) {
		installCmd := exec.CommandContext(ctx, "npm", "install", "--production")
		installCmd.Dir = dir
		installCmd.Stdout = os.Stderr
		installCmd.Stderr = os.Stderr
		if err := installCmd.Run(); err != nil {
			return BuildResult{}, fmt.Errorf("node driver: npm install: %w", err)
		}
	}

	pkgRef := "node:" + nodeVersion
	return BuildResult{
		SourceDir:  dir,
		Entrypoint: entrypoint,
		Packages:   []string{pkgRef},
	}, nil
}

// nodeEntrypoint determines the entrypoint script for the Node.js project.
// Priority: opts override > package.json "main" field > "index.js" default.
func nodeEntrypoint(dir string, override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(filepath.Join(dir, override)); err != nil {
			return "", fmt.Errorf("node driver: entrypoint %s not found: %w", override, err)
		}
		return override, nil
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "index.js", nil
	}

	var pkg struct {
		Main string `json:"main"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "index.js", nil
	}
	if pkg.Main != "" {
		return pkg.Main, nil
	}
	return "index.js", nil
}

// nodeVersionFromPackageJSON reads the "engines.node" field from package.json
// and returns the major version. Defaults to "20" if not specified.
func nodeVersionFromPackageJSON(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "20", nil
	}

	var pkg struct {
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "20", nil
	}
	if pkg.Engines.Node == "" {
		return "20", nil
	}

	v := strings.TrimPrefix(pkg.Engines.Node, ">=")
	v = strings.TrimPrefix(v, "^")
	v = strings.TrimPrefix(v, "~")
	if idx := strings.Index(v, "."); idx > 0 {
		v = v[:idx]
	}
	if v == "" || v == "*" {
		return "20", nil
	}
	return v, nil
}

// PythonDriver builds Python projects into unikernel images.
type PythonDriver struct{}

// Lang returns LangPython.
func (p *PythonDriver) Lang() Lang { return LangPython }

// Detect checks for pyproject.toml or requirements.txt in dir.
func (p *PythonDriver) Detect(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		return true
	}
	return false
}

// Build installs Python dependencies and returns the source directory with
// the python runtime package. The result includes Packages=["python:3.12"]
// so the caller can resolve the Python runtime.
func (p *PythonDriver) Build(ctx context.Context, dir string, opts Options) (BuildResult, error) {
	entrypoint, err := pythonEntrypoint(dir, opts.Entrypoint)
	if err != nil {
		return BuildResult{}, err
	}

	pythonVersion, err := pythonVersionFromConfig(dir)
	if err != nil {
		return BuildResult{}, err
	}

	var env map[string]string
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		pipCmd := exec.CommandContext(ctx, "pip", "install", "-r", "requirements.txt", "--target", "packages")
		pipCmd.Dir = dir
		pipCmd.Stdout = os.Stderr
		pipCmd.Stderr = os.Stderr
		if err := pipCmd.Run(); err != nil {
			return BuildResult{}, fmt.Errorf("python driver: pip install: %w", err)
		}
		// pip installs to packages/; Python's default sys.path doesn't include it
		env = map[string]string{"PYTHONPATH": "/packages"}
	}

	pkgRef := "python:" + pythonVersion
	return BuildResult{
		SourceDir:  dir,
		Entrypoint: entrypoint,
		Packages:   []string{pkgRef},
		Env:        env,
	}, nil
}

// pythonEntrypoint determines the entrypoint script for the Python project.
// Priority: opts override > pyproject.toml [project] scripts > "main.py" default.
func pythonEntrypoint(dir string, override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(filepath.Join(dir, override)); err != nil {
			return "", fmt.Errorf("python driver: entrypoint %s not found: %w", override, err)
		}
		return override, nil
	}

	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		return "main.py", nil
	}

	var cfg struct {
		Project struct {
			Scripts map[string]string `toml:"scripts"`
		} `toml:"project"`
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return "main.py", nil
	}
	if len(cfg.Project.Scripts) > 0 {
		for _, script := range cfg.Project.Scripts {
			return script, nil
		}
	}
	return "main.py", nil
}

// pythonVersionFromConfig reads requires-python from pyproject.toml
// and returns the major.minor version. Defaults to "3.12" if not specified.
func pythonVersionFromConfig(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		return "3.12", nil
	}

	var cfg struct {
		Project struct {
			RequiresPython string `toml:"requires-python"`
		} `toml:"project"`
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return "3.12", nil
	}
	if cfg.Project.RequiresPython == "" {
		return "3.12", nil
	}

	v := strings.TrimPrefix(cfg.Project.RequiresPython, ">=")
	v = strings.TrimPrefix(v, "^")
	v = strings.TrimPrefix(v, "~")
	v = strings.TrimPrefix(v, ">")
	v = strings.TrimPrefix(v, "=")

	parts := strings.SplitN(v, ".", 3)
	switch len(parts) {
	case 1:
		return parts[0], nil
	case 2, 3:
		return parts[0] + "." + parts[1], nil
	default:
		return "3.12", nil
	}
}

// RustDriver builds Rust projects into static ELF binaries via cross-compilation.
type RustDriver struct{}

// Lang returns LangRust.
func (r *RustDriver) Lang() Lang { return LangRust }

// Detect checks for Cargo.toml in dir.
func (r *RustDriver) Detect(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "Cargo.toml"))
	return err == nil
}

// Build compiles a Rust project into a static ELF binary using
// `cargo build --release --target x86_64-unknown-linux-musl`.
// Requires the musl target to be installed: `rustup target add x86_64-unknown-linux-musl`.
func (r *RustDriver) Build(ctx context.Context, dir string, opts Options) (BuildResult, error) {
	platform := opts.Platform
	if platform.OS == "" {
		platform = Platform{OS: "linux", Arch: "amd64"}
	}
	target := platform.RustTarget()

	args := []string{"build", "--release", "--target", target}
	if len(opts.BuildArgs) > 0 {
		args = append(args, opts.BuildArgs...)
	}

	cmd := exec.CommandContext(ctx, "cargo", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CARGO_BUILD_TARGET="+target)

	if err := cmd.Run(); err != nil {
		return BuildResult{}, fmt.Errorf("rust driver: cargo build: %w", err)
	}

	binaryName, err := rustBinaryName(dir)
	if err != nil {
		return BuildResult{}, err
	}

	binPath := filepath.Join(dir, "target", target, "release", binaryName)
	if _, err := os.Stat(binPath); err != nil {
		return BuildResult{}, fmt.Errorf("rust driver: binary not found at %s: %w", binPath, err)
	}

	tmpBin, err := os.CreateTemp("", "uni-rust-build-*")
	if err != nil {
		return BuildResult{}, fmt.Errorf("rust driver: create temp: %w", err)
	}
	tmpPath := tmpBin.Name()
	tmpBin.Close()

	if err := copyFile(binPath, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return BuildResult{}, fmt.Errorf("rust driver: copy binary: %w", err)
	}

	return BuildResult{BinaryPath: tmpPath}, nil
}

// rustBinaryName reads the package name from Cargo.toml.
func rustBinaryName(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return "", fmt.Errorf("rust driver: read Cargo.toml: %w", err)
	}

	var cargo struct {
		Package struct {
			Name string `toml:"name"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(data, &cargo); err != nil {
		return "", fmt.Errorf("rust driver: parse Cargo.toml: %w", err)
	}
	if cargo.Package.Name == "" {
		return "", fmt.Errorf("rust driver: Cargo.toml missing package.name")
	}
	return cargo.Package.Name, nil
}

// copyFile copies src to dst, preserving permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copy open src: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("copy create dst: %w", err)
	}
	defer out.Close()

	if _, err := out.ReadFrom(in); err != nil {
		return fmt.Errorf("copy data: %w", err)
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("copy stat src: %w", err)
	}
	if err := os.Chmod(dst, info.Mode()); err != nil {
		return fmt.Errorf("copy chmod: %w", err)
	}
	return nil
}

// GoDriver builds Go projects into static ELF binaries.
type GoDriver struct{}

// Lang returns LangGo.
func (g *GoDriver) Lang() Lang { return LangGo }

// Detect checks for go.mod in dir.
func (g *GoDriver) Detect(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// Build compiles a Go project with CGO_ENABLED=0 and returns the binary path.
func (g *GoDriver) Build(ctx context.Context, dir string, opts Options) (BuildResult, error) {
	platform := opts.Platform
	if platform.OS == "" {
		platform = DefaultPlatform()
	}

	output := filepath.Join(dir, ".uni-build-binary")
	if runtime.GOOS == "windows" || platform.OS == "windows" {
		output += ".exe"
	}

	args := []string{"build", "-o", output}
	if len(opts.BuildArgs) > 0 {
		args = append(args, opts.BuildArgs...)
	}
	if opts.Entrypoint != "" {
		args = append(args, opts.Entrypoint)
	}

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), platform.GoCrossCompileEnv()...)
	cmd.Env = append(cmd.Env, opts.Env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return BuildResult{}, fmt.Errorf("go build: %w", err)
	}

	abs, err := filepath.Abs(output)
	if err != nil {
		return BuildResult{}, fmt.Errorf("go build: resolve output path: %w", err)
	}

	return BuildResult{
		BinaryPath: abs,
		Entrypoint: opts.Entrypoint,
	}, nil
}
