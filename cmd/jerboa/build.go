package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/builder"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/spf13/cobra"
)

// buildProgramPath is the reserved tar path under which the compiled program
// binary is streamed in a build context. The daemon splits it out and places
// it at /program in the image, so it must not collide with real guest paths.
const buildProgramPath = ".jerboa-program"

// absPath resolves p to an absolute path, returning p unchanged on error.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func newBuildCmd(endpoint *string, verbose *bool) *cobra.Command {
	var (
		name       string
		tag        string
		memory     string
		cpus       int
		port       int
		pkgs       []string
		pkgSource  string
		lang       string
		platform   string
		entrypoint string
	)
	cmd := &cobra.Command{
		Use:   "build <path>",
		Short: "Build a unikernel image from a binary or source directory",
		Long: `Build a unikernel image from a static ELF binary or a source directory.

If <path> is a file, it is used directly as the binary (legacy mode).
If <path> is a directory and --lang is specified, the appropriate language
driver compiles the project first, then packages the result.
If --lang is omitted for a directory, the language is auto-detected from
project markers (go.mod, package.json, etc.).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			buildStart := time.Now()
			sp := newSpinner(cmd.ErrOrStderr(), *verbose)

			var pkgFiles []pkg.File
			if len(pkgs) > 0 {
				sp.Start("Resolving packages")
				var resolved []pkg.File
				var err error
				if pkgSource == "ops" {
					resolved, err = resolveOpsPackages(cmd.Context(), pkgs)
				} else {
					resolved, err = resolvePackages(cmd.Context(), pkgs)
				}
				if err != nil {
					sp.Fail("Package resolution failed")
					return fmt.Errorf("build: %w", err)
				}
				pkgFiles = resolved
				sp.Done(fmt.Sprintf("Resolved %s", strings.Join(pkgs, ", ")))
			}

			// Seed image env vars from ops package manifests (e.g. HOME, PYTHONPATH set by eyberg/python).
			pkgEnv := make(map[string]string)
			if pkgSource == "ops" && len(pkgs) > 0 {
				for k, v := range loadOpsPackageEnvs(pkgs) {
					pkgEnv[k] = v
				}
			}

			srcPath := absPath(args[0])

			var binaryPath string
			var programArgs []string
			var cfg *builder.Config
			info, err := os.Stat(srcPath)
			if err != nil {
				return fmt.Errorf("build: stat %s: %w", srcPath, err)
			}

			// Writers for build output: info messages and subprocess output.
			infoW := io.Writer(io.Discard)
			if *verbose {
				infoW = cmd.ErrOrStderr()
			}
			subW := sp.SubWriter()

			if info.IsDir() {
				cfg, err = builder.LoadConfig(srcPath)
				if err != nil {
					return fmt.Errorf("build: %w", err)
				}

				if cfg != nil && cfg.HasStages() {
					sp.Start("Building project")
					binaryPath, pkgFiles, err = buildStages(cmd, cfg, srcPath, pkgFiles, platform, pkgSource, infoW, subW)
					if err != nil {
						sp.Fail("Build failed")
						return err
					}
					sp.Done("Build complete")
					defer func() { _ = os.Remove(binaryPath) }()
				} else {
					sp.Start("Building project")
					var buildEntrypoint string
					var driverEnv map[string]string
					binaryPath, buildEntrypoint, programArgs, driverEnv, err = buildSingle(cmd, srcPath, cfg, lang, platform, &pkgFiles, pkgSource, pkgs, infoW, subW)
					if err != nil {
						sp.Fail("Build failed")
						return err
					}
					sp.Done("Build complete")
					if binaryPath != "" {
						defer func() { _ = os.Remove(binaryPath) }()
					}
					if buildEntrypoint != "" {
						entrypoint = buildEntrypoint
					}
					// Driver env (e.g. PYTHONPATH) overrides package manifest env for same keys.
					for k, v := range driverEnv {
						pkgEnv[k] = v
					}
				}
			} else {
				binaryPath = srcPath
			}

			if name == "" {
				name = filepath.Base(filepath.Clean(args[0]))
			}

			var diskSize string
			if cfg != nil {
				diskSize = cfg.Build.DiskSize
			}

			sp.Start("Assembling image on daemon")
			client, err := api.Dial(*endpoint)
			if err != nil {
				sp.Fail("Image assembly failed")
				return fmt.Errorf("build: connect to daemon: %w", err)
			}
			defer func() { _ = client.Close() }()

			pr := buildContextReader(binaryPath, pkgFiles)
			defer func() { _ = pr.Close() }()
			res, err := client.ImageBuild(cmd.Context(), api.BuildParams{
				Name:       name,
				Tag:        tag,
				Program:    buildProgramPath,
				Memory:     memory,
				CPUs:       cpus,
				Entrypoint: entrypoint,
				Args:       programArgs,
				Env:        pkgEnv,
				Port:       port,
				DiskSize:   diskSize,
			}, pr)
			if err != nil {
				sp.Fail("Image assembly failed")
				return fmt.Errorf("build: %w", err)
			}

			elapsed := time.Since(buildStart)
			sizeStr := formatSize(res.DiskSize)
			sp.Done(fmt.Sprintf("%s:%s  ·  %s  ·  built in %s", res.Name, res.Tag, sizeStr, formatDuration(elapsed)))

			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s:%s  ·  %s  ·  built in %s\n",
				res.DiskDigest, res.Name, res.Tag, sizeStr, formatDuration(elapsed))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name (default: binary filename)")
	cmd.Flags().StringVar(&tag, "tag", "latest", "image tag")
	cmd.Flags().StringVar(&memory, "memory", "256M", "default VM memory")
	cmd.Flags().IntVar(&cpus, "cpus", 1, "default VM CPU count")
	cmd.Flags().StringArrayVar(&pkgs, "pkg", nil, "include package in image (e.g. node:20, python:3.12) (repeatable)")
	cmd.Flags().StringVar(&pkgSource, "pkg-source", "jerboa", "package source: \"jerboa\" (default) or \"ops\" (nanovms/ops ecosystem)")
	cmd.Flags().StringVar(&lang, "lang", "", "build from source directory with language driver (go, node, python, rust, raw)")
	cmd.Flags().StringVar(&platform, "platform", "", "target platform for cross-compilation (e.g. linux/amd64, linux/arm64)")
	cmd.Flags().IntVar(&port, "port", 0, "declared service port; enables network in the image manifest (required for HTTP servers)")
	return cmd
}

// runBuildCommand executes a single shell command string in dir, writing output to w.
// Uses sh -c on Unix and cmd /c on Windows so arbitrary shell syntax works.
func runBuildCommand(ctx context.Context, dir, command string, w io.Writer) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run command: %w", err)
	}
	return nil
}

// buildSingle handles a single-language build (no stages).
// Returns (binaryPath, entrypoint, args, env, error). entrypoint is non-empty
// for interpreted languages (e.g. "hi.js" for Node). args holds additional
// argv elements for lang="raw" builds (from [program].args). env holds
// runtime environment variables the driver requires (e.g. PYTHONPATH when
// pip installed packages).
func buildSingle(cmd *cobra.Command, srcPath string, cfg *builder.Config, langFlag string, platformFlag string, pkgFiles *[]pkg.File, pkgSource string, userPkgs []string, infoW io.Writer, subW io.Writer) (string, string, []string, map[string]string, error) {
	var langHint builder.Lang
	var err error
	switch {
	case langFlag != "":
		langHint, err = builder.ParseLang(langFlag)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("build: %w", err)
		}
	case cfg != nil && cfg.LangHint() != builder.LangUnknown:
		langHint = cfg.LangHint()
		fmt.Fprintf(infoW, "using language from %s: %s\n", builder.ConfigFileName, langHint)
	}

	var buildPlatform builder.Platform
	if platformFlag != "" {
		buildPlatform, err = builder.ParsePlatform(platformFlag)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("build: %w", err)
		}
	}

	detected, err := builder.DetectLanguage(srcPath, langHint)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("build: %w", err)
	}
	driver, err := builder.GetDriver(detected)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("build: %w", err)
	}
	fmt.Fprintf(infoW, "detected language: %s\n", detected)

	var cfgEntrypoint string
	var buildArgs []string
	var buildRun []string
	if cfg != nil {
		cfgEntrypoint = cfg.Build.Entrypoint
		buildArgs = cfg.Build.Args
		buildRun = cfg.Build.Run
	}

	// Execute user-defined build commands (unikernel.toml [build] run = [...]).
	// These run before the driver packages the project — equivalent to RUN in a Dockerfile.
	for _, command := range buildRun {
		fmt.Fprintf(infoW, "$ %s\n", command)
		if err := runBuildCommand(cmd.Context(), srcPath, command, subW); err != nil {
			return "", "", nil, nil, fmt.Errorf("build: run %q: %w", command, err)
		}
	}

	result, err := driver.Build(cmd.Context(), srcPath, builder.Options{
		Entrypoint: cfgEntrypoint,
		BuildArgs:  buildArgs,
		Platform:   buildPlatform,
		Output:     subW,
	})
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("build %s: %w", detected, err)
	}

	switch {
	case result.BinaryPath != "":
		return result.BinaryPath, "", nil, nil, nil
	case result.SourceDir != "":
		var runtimeBinary string
		var programArgs []string
		env := make(map[string]string)

		if detected == builder.LangRaw {
			if cfg == nil || cfg.Program.Path == "" {
				return "", "", nil, nil, fmt.Errorf("build: lang %q requires %s with [program] path = \"...\"", "raw", builder.ConfigFileName)
			}
			runtimeBinary, err = findProgramBinary(*pkgFiles, cfg.Program.Path)
			if err != nil {
				return "", "", nil, nil, fmt.Errorf("build: %w", err)
			}
			programArgs = cfg.Program.Args
		} else {
			autoPkgs := filterCoveredAutoPkgs(result.Packages, userPkgs)
			resolvedPkgs, err := resolveAutoPackages(cmd.Context(), autoPkgs, pkgSource)
			if err != nil {
				return "", "", nil, nil, fmt.Errorf("build: resolve packages: %w", err)
			}
			*pkgFiles = append(*pkgFiles, resolvedPkgs...)

			runtimeBinary, err = findRuntimeBinary(append(resolvedPkgs, *pkgFiles...), detected)
			if err != nil {
				return "", "", nil, nil, fmt.Errorf("build: %w", err)
			}

			// Merge env from auto-resolved ops packages, then driver (driver takes priority).
			if pkgSource == "ops" && len(autoPkgs) > 0 {
				for k, v := range loadOpsPackageEnvs(autoPkgs) {
					env[k] = v
				}
			}
		}

		srcFiles, err := sourceFiles(result.SourceDir)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("build: collect source files: %w", err)
		}
		*pkgFiles = append(*pkgFiles, srcFiles...)

		for k, v := range result.Env {
			env[k] = v
		}
		return runtimeBinary, result.Entrypoint, programArgs, env, nil
	default:
		return "", "", nil, nil, fmt.Errorf("build %s: driver returned empty result", detected)
	}
}

// stageResult holds the output of a completed build stage.
type stageResult struct {
	binaryPath string
	sourceDir  string
	pkgFiles   []pkg.File
}

// buildStages processes multi-stage builds from unikernel.toml.
// Each stage is built independently. CopyFrom directives copy artifacts
// from previous stages. The final stage's output is used as the image binary.
func buildStages(cmd *cobra.Command, cfg *builder.Config, srcPath string, pkgFiles []pkg.File, platformFlag, pkgSource string, infoW io.Writer, subW io.Writer) (string, []pkg.File, error) {
	stageOutputs := make(map[string]*stageResult)

	var buildPlatform builder.Platform
	var err error
	if platformFlag != "" {
		buildPlatform, err = builder.ParsePlatform(platformFlag)
		if err != nil {
			return "", nil, fmt.Errorf("build: %w", err)
		}
	}

	for i, stage := range cfg.Stages {
		fmt.Fprintf(infoW, "[stage %d/%d] Building %q (%s)...\n", i+1, len(cfg.Stages), stage.Name, stage.Lang)

		stageLang, err := builder.ParseLang(stage.Lang)
		if err != nil {
			return "", nil, fmt.Errorf("build stage %q: %w", stage.Name, err)
		}

		detected, err := builder.DetectLanguage(srcPath, stageLang)
		if err != nil {
			return "", nil, fmt.Errorf("build stage %q: %w", stage.Name, err)
		}
		driver, err := builder.GetDriver(detected)
		if err != nil {
			return "", nil, fmt.Errorf("build stage %q: %w", stage.Name, err)
		}

		var stagePkgs []pkg.File
		stagePkgs = append(stagePkgs, pkgFiles...)

		for _, cf := range stage.CopyFrom {
			prev, ok := stageOutputs[cf.Stage]
			if !ok {
				return "", nil, fmt.Errorf("build stage %q: copy_from references unknown stage %q", stage.Name, cf.Stage)
			}
			if prev.binaryPath == "" {
				return "", nil, fmt.Errorf("build stage %q: copy_from stage %q has no binary output", stage.Name, cf.Stage)
			}
			dst := cf.Dst
			if dst == "" {
				dst = filepath.Base(cf.Src)
			}
			stagePkgs = append(stagePkgs, pkg.File{HostPath: prev.binaryPath, GuestPath: filepath.Base(prev.binaryPath)})
			_ = dst
		}

		var driverPkgPaths []string
		for _, pf := range stagePkgs {
			driverPkgPaths = append(driverPkgPaths, pf.HostPath)
		}

		result, err := driver.Build(cmd.Context(), srcPath, builder.Options{
			Entrypoint: stage.Entrypoint,
			BuildArgs:  stage.Args,
			PkgFiles:   driverPkgPaths,
			Platform:   buildPlatform,
			Output:     subW,
		})
		if err != nil {
			return "", nil, fmt.Errorf("build stage %q (%s): %w", stage.Name, stage.Lang, err)
		}

		switch {
		case result.BinaryPath != "":
			stageOutputs[stage.Name] = &stageResult{
				binaryPath: result.BinaryPath,
				pkgFiles:   stagePkgs,
			}
		case result.SourceDir != "":
			resolvedPkgs, err := resolveAutoPackages(cmd.Context(), result.Packages, pkgSource)
			if err != nil {
				return "", nil, fmt.Errorf("build stage %q: resolve packages: %w", stage.Name, err)
			}
			stagePkgs = append(stagePkgs, resolvedPkgs...)

			runtimeBinary, err := findRuntimeBinary(resolvedPkgs, detected)
			if err != nil {
				return "", nil, fmt.Errorf("build stage %q: %w", stage.Name, err)
			}

			stageOutputs[stage.Name] = &stageResult{
				binaryPath: runtimeBinary,
				sourceDir:  result.SourceDir,
				pkgFiles:   stagePkgs,
			}
		default:
			return "", nil, fmt.Errorf("build stage %q: driver returned empty result", stage.Name)
		}
	}

	finalStage := cfg.Stages[len(cfg.Stages)-1]
	finalResult := stageOutputs[finalStage.Name]
	if finalResult == nil {
		return "", nil, fmt.Errorf("build: final stage %q has no output", finalStage.Name)
	}

	return finalResult.binaryPath, finalResult.pkgFiles, nil
}

func defaultToolsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".jerboa", "tools")
	}
	return filepath.Join(home, ".jerboa", "tools")
}
