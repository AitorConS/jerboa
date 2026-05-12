package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/builder"
	"github.com/AitorConS/unikernel-engine/internal/image"
	pkg "github.com/AitorConS/unikernel-engine/internal/package"
	"github.com/AitorConS/unikernel-engine/internal/tools"
	"github.com/spf13/cobra"
)

// absPath resolves p to an absolute path, returning p unchanged on error.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func newBuildCmd(storePath *string) *cobra.Command {
	var (
		name      string
		tag       string
		memory    string
		cpus      int
		mkfs      string
		updateYes bool
		pkgs      []string
		lang      string
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
			store, err := image.NewStore(*storePath)
			if err != nil {
				return fmt.Errorf("build: open store: %w", err)
			}

			toolsDir := defaultToolsPath()

			if mkfs == "" && os.Getenv("UNI_MKFS") == "" && tools.Exist(toolsDir) {
				if err := checkKernelUpdateForBuild(cmd, toolsDir, updateYes); err != nil {
					return err
				}
			}

			if mkfs == "" {
				mkfs = os.Getenv("UNI_MKFS")
			}
			mkfsRun, err := tools.ResolveMkfs(cmd.Context(), toolsDir, mkfs)
			if err != nil {
				return fmt.Errorf("build: %w", err)
			}

			var pkgFiles []string
			if len(pkgs) > 0 {
				resolved, err := resolvePackages(cmd.Context(), pkgs)
				if err != nil {
					return fmt.Errorf("build: %w", err)
				}
				pkgFiles = resolved
			}

			srcPath := absPath(args[0])

			var binaryPath string
			info, err := os.Stat(srcPath)
			if err != nil {
				return fmt.Errorf("build: stat %s: %w", srcPath, err)
			}

			if info.IsDir() {
				cfg, err := builder.LoadConfig(srcPath)
				if err != nil {
					return fmt.Errorf("build: %w", err)
				}

				var langHint builder.Lang
				switch {
				case lang != "":
					langHint, err = builder.ParseLang(lang)
					if err != nil {
						return fmt.Errorf("build: %w", err)
					}
				case cfg != nil && cfg.LangHint() != builder.LangUnknown:
					langHint = cfg.LangHint()
					fmt.Fprintf(cmd.ErrOrStderr(), "using language from %s: %s\n", builder.ConfigFileName, langHint)
				}

				detected, err := builder.DetectLanguage(srcPath, langHint)
				if err != nil {
					return fmt.Errorf("build: %w", err)
				}
				driver, err := builder.GetDriver(detected)
				if err != nil {
					return fmt.Errorf("build: %w", err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "detected language: %s\n", detected)

				var entrypoint string
				var buildArgs []string
				if cfg != nil {
					entrypoint = cfg.Build.Entrypoint
					buildArgs = cfg.Build.Args
				}

				result, err := driver.Build(cmd.Context(), srcPath, builder.Options{
					Entrypoint: entrypoint,
					BuildArgs:  buildArgs,
					PkgFiles:   pkgFiles,
				})
				if err != nil {
					return fmt.Errorf("build %s: %w", detected, err)
				}

				switch {
				case result.BinaryPath != "":
					binaryPath = result.BinaryPath
					defer func() { _ = os.Remove(binaryPath) }()
				case result.SourceDir != "":
					resolvedPkgs, err := resolveAutoPackages(cmd.Context(), result.Packages)
					if err != nil {
						return fmt.Errorf("build: resolve packages: %w", err)
					}
					pkgFiles = append(pkgFiles, resolvedPkgs...)

					runtimeBinary, err := findRuntimeBinary(resolvedPkgs, detected)
					if err != nil {
						return fmt.Errorf("build: %w", err)
					}
					binaryPath = runtimeBinary

					srcFiles, err := sourceFiles(result.SourceDir)
					if err != nil {
						return fmt.Errorf("build: collect source files: %w", err)
					}
					pkgFiles = append(pkgFiles, srcFiles...)
				default:
					return fmt.Errorf("build %s: driver returned empty result", detected)
				}
			} else {
				binaryPath = srcPath
			}

			if name == "" {
				name = args[0]
			}
			m, err := image.NewBuilder(store).Build(cmd.Context(), image.BuildConfig{
				Name:       name,
				Tag:        tag,
				BinaryPath: binaryPath,
				MkfsRun:    mkfsRun,
				Memory:     memory,
				CPUs:       cpus,
				PkgFiles:   pkgFiles,
			})
			if err != nil {
				return fmt.Errorf("build: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s:%s\n", m.DiskDigest, m.Name, m.Tag)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name (default: binary filename)")
	cmd.Flags().StringVar(&tag, "tag", "latest", "image tag")
	cmd.Flags().StringVar(&memory, "memory", "256M", "default VM memory")
	cmd.Flags().IntVar(&cpus, "cpus", 1, "default VM CPU count")
	cmd.Flags().StringVar(&mkfs, "mkfs", "", "path to mkfs binary — skip auto-download (env: UNI_MKFS)")
	cmd.Flags().BoolVarP(&updateYes, "update-kernel", "U", false, "auto-approve kernel update if one is available")
	cmd.Flags().StringArrayVar(&pkgs, "pkg", nil, "include package in image (e.g. node:20, python:3.12) (repeatable)")
	cmd.Flags().StringVar(&lang, "lang", "", "build from source directory with language driver (go, node, python, rust)")
	return cmd
}

// resolvePackages downloads and extracts packages, returning the list of
// file paths that should be included in the manifest.
func resolvePackages(ctx context.Context, pkgRefs []string) ([]string, error) {
	pkgStore, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return nil, fmt.Errorf("open package store: %w", err)
	}

	idx, err := pkg.FetchIndex()
	if err != nil {
		return nil, fmt.Errorf("fetch package index: %w", err)
	}

	var files []string
	for _, ref := range pkgRefs {
		pkgName, pkgVer := parsePkgRef(ref)
		target := idx.Latest(pkgName)
		if target == nil {
			return nil, fmt.Errorf("package %q not found in index", pkgName)
		}
		if pkgVer != "" {
			found := false
			versions, ok := idx.Packages[pkgName]
			if ok {
				for i := range versions {
					if versions[i].Version == pkgVer {
						target = &versions[i]
						found = true
						break
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("version %q of package %q not found", pkgVer, pkgName)
			}
		}
		if !pkgStore.IsDownloaded(target.Name, target.Version) {
			if err := pkgStore.Download(*target); err != nil {
				return nil, fmt.Errorf("download package %s: %w", target.Name, err)
			}
			if err := pkgStore.SaveMeta(*target); err != nil {
				return nil, fmt.Errorf("save package meta: %w", err)
			}
		}
		if !pkgStore.IsExtracted(target.Name, target.Version) {
			if err := pkgStore.Extract(*target); err != nil {
				return nil, fmt.Errorf("extract package %s: %w", target.Name, err)
			}
		}
		pkgFiles, err := pkgStore.ExtractedFiles(target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list package files %s: %w", target.Name, err)
		}
		files = append(files, pkgFiles...)
	}
	return files, nil
}

// checkKernelUpdateForBuild fetches the remote kernel version and, if it differs
// from the local one, asks the user whether to update before building.
func checkKernelUpdateForBuild(cmd *cobra.Command, toolsDir string, autoYes bool) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Second)
	defer cancel()

	remote, err := tools.RemoteVersion(ctx)
	if err != nil {
		// Network unreachable: silently continue, don't block the build.
		return nil
	}
	local := tools.LocalVersion(toolsDir)
	if !tools.IsNewer(local, remote) {
		return nil
	}

	fmt.Fprintf(cmd.ErrOrStderr(),
		"⚠  New kernel version available: %s (installed: %s)\n", remote, local)

	if !autoYes && !confirmPrompt("Update kernel before building? [y/N] ") {
		return nil
	}

	if err := tools.ClearCachedTools(toolsDir); err != nil {
		return fmt.Errorf("build: clear kernel cache: %w", err)
	}
	dlCtx, dlCancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer dlCancel()
	if _, err := tools.ResolveMkfs(dlCtx, toolsDir, ""); err != nil {
		return fmt.Errorf("build: download new kernel: %w", err)
	}
	if err := tools.SaveLocalVersion(toolsDir, remote); err != nil {
		return fmt.Errorf("build: save kernel version: %w", err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Kernel updated to %s.\n", remote)
	return nil
}

func defaultToolsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".uni", "tools")
	}
	return filepath.Join(home, ".uni", "tools")
}

// resolveAutoPackages resolves language runtime packages (e.g. "node:20")
// and returns the list of extracted file paths.
func resolveAutoPackages(ctx context.Context, autoPkgs []string) ([]string, error) {
	if len(autoPkgs) == 0 {
		return nil, nil
	}

	pkgStore, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return nil, fmt.Errorf("open package store: %w", err)
	}

	idx, err := pkg.FetchIndex()
	if err != nil {
		return nil, fmt.Errorf("fetch package index: %w", err)
	}

	var files []string
	for _, ref := range autoPkgs {
		pkgName, pkgVer := parsePkgRef(ref)
		target := idx.Latest(pkgName)
		if target == nil {
			return nil, fmt.Errorf("package %q not found in index", pkgName)
		}
		if pkgVer != "" {
			found := false
			versions, ok := idx.Packages[pkgName]
			if ok {
				for i := range versions {
					if versions[i].Version == pkgVer {
						target = &versions[i]
						found = true
						break
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("version %q of package %q not found", pkgVer, pkgName)
			}
		}
		if !pkgStore.IsDownloaded(target.Name, target.Version) {
			if err := pkgStore.Download(*target); err != nil {
				return nil, fmt.Errorf("download package %s: %w", target.Name, err)
			}
			if err := pkgStore.SaveMeta(*target); err != nil {
				return nil, fmt.Errorf("save package meta: %w", err)
			}
		}
		if !pkgStore.IsExtracted(target.Name, target.Version) {
			if err := pkgStore.Extract(*target); err != nil {
				return nil, fmt.Errorf("extract package %s: %w", target.Name, err)
			}
		}
		pkgFiles, err := pkgStore.ExtractedFiles(target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list package files %s: %w", target.Name, err)
		}
		files = append(files, pkgFiles...)
	}
	return files, nil
}

// runtimeBinaryNames maps a language to the expected binary name within its package.
var runtimeBinaryNames = map[builder.Lang]string{
	builder.LangGo:     "",
	builder.LangNode:   "node",
	builder.LangPython: "python3",
	builder.LangRust:   "",
}

// findRuntimeBinary searches the resolved package files for the runtime binary
// of the given language.
func findRuntimeBinary(pkgFiles []string, lang builder.Lang) (string, error) {
	binaryName, ok := runtimeBinaryNames[lang]
	if !ok || binaryName == "" {
		return "", fmt.Errorf("language %s does not have a runtime binary", lang)
	}

	for _, f := range pkgFiles {
		if filepath.Base(f) == binaryName {
			return f, nil
		}
	}

	for _, f := range pkgFiles {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(f), binaryName))
		if len(matches) > 0 {
			return matches[0], nil
		}
	}

	return "", fmt.Errorf("runtime binary %q not found in package files", binaryName)
}

// sourceFiles collects application source files from dir for inclusion in the image.
// It walks the directory, excluding node_modules, .git, and other build artifacts.
func sourceFiles(dir string) ([]string, error) {
	var files []string
	skipDirs := map[string]bool{
		"node_modules": true,
		".git":         true,
		".uni-build":   true,
		"__pycache__":  true,
		".tox":         true,
		"venv":         true,
		".venv":        true,
		"dist":         true,
		".next":        true,
	}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && skipDirs[info.Name()] {
			return filepath.SkipDir
		}
		if !info.IsDir() {
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				return fmt.Errorf("source file rel path: %w", rerr)
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk source dir: %w", err)
	}
	return files, nil
}
