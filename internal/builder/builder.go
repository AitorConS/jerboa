package builder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	default:
		return LangUnknown, fmt.Errorf("unsupported language %q: use go, node, python, or rust", s)
	}
}

// BuildResult holds the output of a successful language build.
type BuildResult struct {
	// BinaryPath is the path to the compiled ELF binary.
	BinaryPath string
	// Entrypoint is the command or script that should be used as the program entrypoint.
	Entrypoint string
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
	}
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
	output := filepath.Join(dir, ".uni-build-binary")
	if runtime.GOOS == "windows" {
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
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
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
