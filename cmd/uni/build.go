package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		pkgSource string
		lang      string
		platform  string
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

			var pkgFiles []pkg.File
			if len(pkgs) > 0 {
				var resolved []pkg.File
				var err error
				if pkgSource == "ops" {
					resolved, err = resolveOpsPackages(cmd.Context(), pkgs)
				} else {
					resolved, err = resolvePackages(cmd.Context(), pkgs)
				}
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

				if cfg != nil && cfg.HasStages() {
					binaryPath, pkgFiles, err = buildStages(cmd, cfg, srcPath, pkgFiles, platform, lang, pkgSource)
					if err != nil {
						return err
					}
					defer func() { _ = os.Remove(binaryPath) }()
				} else {
					binaryPath, err = buildSingle(cmd, srcPath, cfg, lang, platform, &pkgFiles, pkgSource)
					if err != nil {
						return err
					}
					if binaryPath != "" {
						defer func() { _ = os.Remove(binaryPath) }()
					}
				}
			} else {
				binaryPath = srcPath
			}

			if name == "" {
				name = filepath.Base(filepath.Clean(args[0]))
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
	cmd.Flags().StringVar(&pkgSource, "pkg-source", "uni", "package source: \"uni\" (default) or \"ops\" (nanovms/ops ecosystem)")
	cmd.Flags().StringVar(&lang, "lang", "", "build from source directory with language driver (go, node, python, rust)")
	cmd.Flags().StringVar(&platform, "platform", "", "target platform for cross-compilation (e.g. linux/amd64, linux/arm64)")
	return cmd
}

// buildSingle handles a single-language build (no stages).
func buildSingle(cmd *cobra.Command, srcPath string, cfg *builder.Config, langFlag string, platformFlag string, pkgFiles *[]pkg.File, pkgSource string) (string, error) {
	var langHint builder.Lang
	var err error
	switch {
	case langFlag != "":
		langHint, err = builder.ParseLang(langFlag)
		if err != nil {
			return "", fmt.Errorf("build: %w", err)
		}
	case cfg != nil && cfg.LangHint() != builder.LangUnknown:
		langHint = cfg.LangHint()
		fmt.Fprintf(cmd.ErrOrStderr(), "using language from %s: %s\n", builder.ConfigFileName, langHint)
	}

	var buildPlatform builder.Platform
	if platformFlag != "" {
		buildPlatform, err = builder.ParsePlatform(platformFlag)
		if err != nil {
			return "", fmt.Errorf("build: %w", err)
		}
	}

	detected, err := builder.DetectLanguage(srcPath, langHint)
	if err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	driver, err := builder.GetDriver(detected)
	if err != nil {
		return "", fmt.Errorf("build: %w", err)
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
		Platform:   buildPlatform,
	})
	if err != nil {
		return "", fmt.Errorf("build %s: %w", detected, err)
	}

	switch {
	case result.BinaryPath != "":
		return result.BinaryPath, nil
	case result.SourceDir != "":
		resolvedPkgs, err := resolveAutoPackages(cmd.Context(), result.Packages, pkgSource)
		if err != nil {
			return "", fmt.Errorf("build: resolve packages: %w", err)
		}
		*pkgFiles = append(*pkgFiles, resolvedPkgs...)

		runtimeBinary, err := findRuntimeBinary(resolvedPkgs, detected)
		if err != nil {
			return "", fmt.Errorf("build: %w", err)
		}

		srcFiles, err := sourceFiles(result.SourceDir)
		if err != nil {
			return "", fmt.Errorf("build: collect source files: %w", err)
		}
		*pkgFiles = append(*pkgFiles, srcFiles...)
		return runtimeBinary, nil
	default:
		return "", fmt.Errorf("build %s: driver returned empty result", detected)
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
func buildStages(cmd *cobra.Command, cfg *builder.Config, srcPath string, pkgFiles []pkg.File, platformFlag, langFlag, pkgSource string) (string, []pkg.File, error) {
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
		fmt.Fprintf(cmd.ErrOrStderr(), "[stage %d/%d] Building %q (%s)...\n", i+1, len(cfg.Stages), stage.Name, stage.Lang)

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

// resolvePackages downloads and extracts packages, returning the list of
// package files that should be included in the manifest.
func resolvePackages(ctx context.Context, pkgRefs []string) ([]pkg.File, error) {
	pkgStore, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return nil, fmt.Errorf("open package store: %w", err)
	}

	idx, err := pkg.FetchIndex()
	if err != nil {
		return nil, fmt.Errorf("fetch package index: %w", err)
	}

	var files []pkg.File
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
		paths, err := pkgStore.ExtractedFiles(target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list package files %s: %w", target.Name, err)
		}
		for _, p := range paths {
			files = append(files, pkg.File{HostPath: p, GuestPath: filepath.Base(p)})
		}
	}
	return files, nil
}

// resolveOpsPackages downloads and extracts ops packages, returning the list
// of package files with proper guest paths (preserving sysroot/ hierarchy).
func resolveOpsPackages(ctx context.Context, pkgRefs []string) ([]pkg.File, error) {
	opsStore, err := openOpsStore()
	if err != nil {
		return nil, fmt.Errorf("open ops package store: %w", err)
	}

	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return nil, fmt.Errorf("fetch ops manifest: %w", err)
	}

	var files []pkg.File
	for _, ref := range pkgRefs {
		id, err := pkg.ParseOpsIdentifier(ref)
		if err != nil {
			return nil, fmt.Errorf("parse ops package %q: %w", ref, err)
		}

		target := manifest.Lookup(id.Namespace, id.Name, id.Version)
		if target == nil {
			return nil, fmt.Errorf("ops package %q not found in manifest", ref)
		}

		if !opsStore.IsDownloaded(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Download(target.Namespace, target.Name, target.Version, target.SHA256); err != nil {
				return nil, fmt.Errorf("download ops package %s: %w", target.Name, err)
			}
		}
		if !opsStore.IsExtracted(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Extract(target.Namespace, target.Name, target.Version); err != nil {
				return nil, fmt.Errorf("extract ops package %s: %w", target.Name, err)
			}
		}

		pkgFiles, err := opsStore.ExtractedFiles(target.Namespace, target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list ops package files %s: %w", target.Name, err)
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
// and returns the list of extracted package files.
func resolveAutoPackages(ctx context.Context, autoPkgs []string, pkgSource string) ([]pkg.File, error) {
	if len(autoPkgs) == 0 {
		return nil, nil
	}

	if pkgSource == "ops" {
		return resolveOpsAutoPackages(ctx, autoPkgs)
	}

	pkgStore, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return nil, fmt.Errorf("open package store: %w", err)
	}

	idx, err := pkg.FetchIndex()
	if err != nil {
		return nil, fmt.Errorf("fetch package index: %w", err)
	}

	var files []pkg.File
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
		paths, err := pkgStore.ExtractedFiles(target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list package files %s: %w", target.Name, err)
		}
		for _, p := range paths {
			files = append(files, pkg.File{HostPath: p, GuestPath: filepath.Base(p)})
		}
	}
	return files, nil
}

func lookupOpsPackage(manifest *pkg.OpsPackageList, name, version string) *pkg.OpsPackage {
	namespaces := []string{"eyberg", "nanovms", "myuniverse"}
	for _, ns := range namespaces {
		if t := manifest.Lookup(ns, name, version); t != nil {
			return t
		}
	}
	if version == "" || version == "latest" {
		return nil
	}
	for _, ns := range namespaces {
		for i := range manifest.Packages {
			p := &manifest.Packages[i]
			if p.Namespace != ns || p.Name != name {
				continue
			}
			pv := strings.TrimPrefix(p.Version, "v")
			if strings.HasPrefix(pv, version+".") || strings.HasPrefix(pv, version+"-") || pv == version {
				return p
			}
		}
	}
	return nil
}

func resolveOpsAutoPackages(ctx context.Context, autoPkgs []string) ([]pkg.File, error) {
	opsStore, err := openOpsStore()
	if err != nil {
		return nil, fmt.Errorf("open ops package store: %w", err)
	}

	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return nil, fmt.Errorf("fetch ops manifest: %w", err)
	}

	var files []pkg.File
	for _, ref := range autoPkgs {
		pkgName, pkgVer := parsePkgRef(ref)

		var target *pkg.OpsPackage
		if strings.Contains(pkgName, "/") {
			id, parseErr := pkg.ParseOpsIdentifier(pkgName)
			if parseErr != nil {
				return nil, fmt.Errorf("parse ops package %q: %w", pkgName, parseErr)
			}
			if pkgVer != "" && pkgVer != "latest" {
				id.Version = pkgVer
			}
			target = manifest.Lookup(id.Namespace, id.Name, id.Version)
		} else {
			target = lookupOpsPackage(manifest, pkgName, pkgVer)
		}
		if target == nil {
			return nil, fmt.Errorf("ops package %q not found in manifest (try --pkg eyberg/%s)", ref, pkgName)
		}

		if !opsStore.IsDownloaded(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Download(target.Namespace, target.Name, target.Version, target.SHA256); err != nil {
				return nil, fmt.Errorf("download ops package %s: %w", target.Name, err)
			}
		}
		if !opsStore.IsExtracted(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Extract(target.Namespace, target.Name, target.Version); err != nil {
				return nil, fmt.Errorf("extract ops package %s: %w", target.Name, err)
			}
		}

		pkgFiles, err := opsStore.ExtractedFiles(target.Namespace, target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list ops package files %s: %w", target.Name, err)
		}
		files = append(files, pkgFiles...)
	}
	return files, nil
}

// runtimeBinaryNames maps a language to the expected binary name within its package.
var runtimeBinaryNames = map[builder.Lang]string{
	builder.LangNode:   "node",
	builder.LangPython: "python3",
}

// findRuntimeBinary searches the resolved package files for the runtime binary
// of the given language.
func findRuntimeBinary(pkgFiles []pkg.File, lang builder.Lang) (string, error) {
	binaryName, ok := runtimeBinaryNames[lang]
	if !ok {
		return "", fmt.Errorf("language %s does not have a runtime binary", lang)
	}

	for _, f := range pkgFiles {
		base := filepath.Base(f.HostPath)
		if base == binaryName {
			return f.HostPath, nil
		}
	}

	for _, f := range pkgFiles {
		dir := filepath.Dir(f.HostPath)
		prefix := binaryName
		matches, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
		if len(matches) > 0 {
			return matches[0], nil
		}
	}

	return "", fmt.Errorf("runtime binary %q not found in package files", binaryName)
}

// sourceFiles collects application source files from dir for inclusion in the image.
// It reads .unignore patterns and excludes matching files and directories.
func sourceFiles(dir string) ([]pkg.File, error) {
	ignore, err := builder.LoadIgnoreFile(dir)
	if err != nil {
		return nil, fmt.Errorf("load ignore file: %w", err)
	}

	var files []pkg.File
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return fmt.Errorf("source file rel path: %w", rerr)
		}
		rel = filepath.ToSlash(rel)

		if ignore.Match(rel, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			files = append(files, pkg.File{HostPath: path, GuestPath: rel})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk source dir: %w", err)
	}
	return files, nil
}
