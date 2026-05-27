package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/AitorConS/unikernel-engine/internal/image"
	pkg "github.com/AitorConS/unikernel-engine/internal/package"
	"github.com/AitorConS/unikernel-engine/internal/tools"
	"github.com/spf13/cobra"
)

func newPkgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pkg",
		Short: "Manage runtime packages for unikernel images",
	}
	cmd.AddCommand(
		newPkgListCmd(),
		newPkgSearchCmd(),
		newPkgGetCmd(),
		newPkgRemoveCmd(),
		newPkgCreateCmd(),
		newPkgFromDockerCmd(),
		newPkgPushCmd(),
		newPkgLoadCmd(),
	)
	return cmd
}

var pkgStoreDir string
var opsPkgStoreDir string

func pkgStorePath() string {
	if pkgStoreDir != "" {
		return pkgStoreDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".uni", "packages")
	}
	return filepath.Join(home, ".uni", "packages")
}

func opsStorePath() string {
	if opsPkgStoreDir != "" {
		return opsPkgStoreDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".uni", "packages-ops")
	}
	return filepath.Join(home, ".uni", "packages-ops")
}

func openOpsStore() (*pkg.OpsStore, error) {
	return pkg.NewOpsStore(opsStorePath())
}

func newPkgListCmd() *cobra.Command {
	var outputJSON bool
	var source string
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List locally cached packages",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "ops" {
				return pkgListOps(cmd, outputJSON)
			}
			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg list: %w", err)
			}
			pkgs, err := store.List()
			if err != nil {
				return fmt.Errorf("pkg list: %w", err)
			}
			if len(pkgs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No packages installed. Use 'uni pkg search <term>' to find packages.")
				return nil
			}
			if outputJSON {
				return printJSON(cmd.OutOrStdout(), pkgs)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVERSION\tRUNTIME\tDESCRIPTION")
			for _, p := range pkgs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.Version, p.Runtime, p.Description)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "output-json", false, "output as JSON")
	cmd.Flags().StringVar(&source, "source", "uni", "package source: \"uni\" (default) or \"ops\"")
	return cmd
}

func pkgListOps(cmd *cobra.Command, outputJSON bool) error {
	opsStore, err := openOpsStore()
	if err != nil {
		return fmt.Errorf("pkg list --source ops: %w", err)
	}
	pkgs, err := opsStore.List()
	if err != nil {
		return fmt.Errorf("pkg list ops: %w", err)
	}
	if len(pkgs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No ops packages installed. Use 'uni pkg get <namespace>/<name>:<version> --source ops' to download.")
		return nil
	}
	if outputJSON {
		return printJSON(cmd.OutOrStdout(), pkgs)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tVERSION\tLANGUAGE\tARCH")
	for _, p := range pkgs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Namespace, p.Name, p.Version, p.Language, p.Arch)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("pkg list ops: %w", err)
	}
	return nil
}

func newPkgSearchCmd() *cobra.Command {
	var outputJSON bool
	var source string
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the remote package index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "ops" {
				return pkgSearchOps(cmd, args[0], outputJSON)
			}
			idx, err := pkg.FetchIndex()
			if err != nil {
				return fmt.Errorf("pkg search: %w", err)
			}
			results := idx.Search(args[0])
			if len(results) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No packages found matching %q.\n", args[0])
				return nil
			}
			if outputJSON {
				return printJSON(cmd.OutOrStdout(), results)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVERSION\tRUNTIME\tDESCRIPTION")
			for _, p := range results {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.Version, p.Runtime, p.Description)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "output-json", false, "output as JSON")
	cmd.Flags().StringVar(&source, "source", "uni", "package source: \"uni\" (default) or \"ops\"")
	return cmd
}

func pkgSearchOps(cmd *cobra.Command, query string, outputJSON bool) error {
	opsStore, err := openOpsStore()
	if err != nil {
		return fmt.Errorf("pkg search --source ops: %w", err)
	}
	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return fmt.Errorf("pkg search ops: %w", err)
	}
	results := manifest.Search(query)
	if len(results) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No ops packages found matching %q.\n", query)
		return nil
	}
	if outputJSON {
		return printJSON(cmd.OutOrStdout(), results)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tVERSION\tLANGUAGE\tARCH\tDESCRIPTION")
	for _, p := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Namespace, p.Name, p.Version, p.Language, p.Arch, p.Description)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("pkg search ops: %w", err)
	}
	return nil
}

func newPkgGetCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "get <name>[:<version>]",
		Short: "Download a package from the remote index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "ops" {
				return pkgGetOps(cmd, args[0])
			}
			name, version := parsePkgRef(args[0])

			idx, err := pkg.FetchIndex()
			if err != nil {
				return fmt.Errorf("pkg get: fetch index: %w", err)
			}

			var target *pkg.Package
			if version != "" {
				versions, ok := idx.Packages[name]
				if !ok {
					return fmt.Errorf("pkg get: package %q not found", name)
				}
				for i := range versions {
					if versions[i].Version == version {
						target = &versions[i]
						break
					}
				}
				if target == nil {
					return fmt.Errorf("pkg get: version %q of package %q not found", version, name)
				}
			} else {
				target = idx.Latest(name)
				if target == nil {
					return fmt.Errorf("pkg get: package %q not found", name)
				}
			}

			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg get: %w", err)
			}

			if store.IsDownloaded(target.Name, target.Version) {
				fmt.Fprintf(cmd.OutOrStdout(), "Package %s %s already downloaded.\n", target.Name, target.Version)
				return nil
			}

			if err := store.Download(*target); err != nil {
				return fmt.Errorf("pkg get: %w", err)
			}
			if err := store.SaveMeta(*target); err != nil {
				return fmt.Errorf("pkg get: save meta: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Package %s %s installed.\n", target.Name, target.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "uni", "package source: \"uni\" (default) or \"ops\"")
	return cmd
}

func pkgGetOps(cmd *cobra.Command, ref string) error {
	id, err := pkg.ParseOpsIdentifier(ref)
	if err != nil {
		return fmt.Errorf("pkg get --source ops: %w", err)
	}

	opsStore, err := openOpsStore()
	if err != nil {
		return fmt.Errorf("pkg get ops: %w", err)
	}

	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return fmt.Errorf("pkg get ops: fetch manifest: %w", err)
	}

	target := manifest.Lookup(id.Namespace, id.Name, id.Version)
	if target == nil {
		return fmt.Errorf("pkg get ops: package %q not found in ops manifest", ref)
	}

	if opsStore.IsDownloaded(target.Namespace, target.Name, target.Version) {
		fmt.Fprintf(cmd.OutOrStdout(), "Ops package %s/%s %s already downloaded.\n", target.Namespace, target.Name, target.Version)
		return nil
	}

	if err := opsStore.Download(target.Namespace, target.Name, target.Version, target.SHA256); err != nil {
		return fmt.Errorf("pkg get ops: %w", err)
	}
	if err := opsStore.Extract(target.Namespace, target.Name, target.Version); err != nil {
		return fmt.Errorf("pkg get ops: extract: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Ops package %s/%s %s installed.\n", target.Namespace, target.Name, target.Version)
	return nil
}

func newPkgRemoveCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:     "remove <name>[:<version>]",
		Short:   "Remove a locally cached package",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "ops" {
				return pkgRemoveOps(cmd, args[0])
			}
			name, version := parsePkgRef(args[0])

			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg remove: %w", err)
			}

			if version == "" {
				if err := store.RemoveAll(name); err != nil {
					return fmt.Errorf("pkg remove: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed all versions of package %s.\n", name)
				return nil
			}
			if err := store.Remove(name, version); err != nil {
				return fmt.Errorf("pkg remove: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed package %s %s.\n", name, version)
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "uni", "package source: \"uni\" (default) or \"ops\"")
	return cmd
}

func pkgRemoveOps(cmd *cobra.Command, ref string) error {
	id, err := pkg.ParseOpsIdentifier(ref)
	if err != nil {
		return fmt.Errorf("pkg remove --source ops: %w", err)
	}

	opsStore, err := openOpsStore()
	if err != nil {
		return fmt.Errorf("pkg remove ops: %w", err)
	}

	if err := opsStore.Remove(id.Namespace, id.Name, id.Version); err != nil {
		return fmt.Errorf("pkg remove ops: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed ops package %s/%s %s.\n", id.Namespace, id.Name, id.Version)
	return nil
}

func newPkgCreateCmd() *cobra.Command {
	var (
		libs         []string
		description  string
		runtimeName  string
		missingFiles bool
	)
	cmd := &cobra.Command{
		Use:   "create <name>[:<version>] <binary>",
		Short: "Create a local package from a binary and optional files",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, version := parsePkgRef(args[0])
			if version == "" {
				version = "1.0.0"
			}
			binaryPath, err := filepath.Abs(args[1])
			if err != nil {
				return fmt.Errorf("pkg create: resolving path: %w", err)
			}
			if _, err := os.Stat(binaryPath); err != nil {
				return fmt.Errorf("pkg create: binary not found: %s", binaryPath)
			}

			if missingFiles {
				missing, lddErr := pkg.MissingFiles(binaryPath)
				switch {
				case lddErr != nil:
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: --missing-files could not run ldd: %v\n", lddErr)
				case len(missing) > 0:
					fmt.Fprintf(cmd.ErrOrStderr(), "Missing shared libraries detected (not on local filesystem):\n")
					for _, m := range missing {
						fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", m)
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "Consider adding these with --libs or re-running with the binary on a Linux system.\n")
				default:
					fmt.Fprintf(cmd.ErrOrStderr(), "All shared library dependencies are present.\n")
				}
			}

			allLibs := libs
			resolved, err := resolveLibsFromLdd(binaryPath)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not auto-resolve shared libs: %v\n", err)
			} else {
				allLibs = append(allLibs, resolved...)
			}

			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg create: %w", err)
			}

			if err := store.Create(name, version, binaryPath, allLibs, description, runtimeName); err != nil {
				return fmt.Errorf("pkg create: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Package %s:%s created from %s.\n", name, version, filepath.Base(binaryPath))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&libs, "libs", nil, "Additional files to include (repeatable)")
	cmd.Flags().StringVar(&description, "description", "", "Package description")
	cmd.Flags().StringVar(&runtimeName, "runtime", "", "Runtime family (e.g. node, python)")
	cmd.Flags().BoolVar(&missingFiles, "missing-files", false, "Report shared library dependencies missing from the local filesystem")
	return cmd
}

func resolveLibsFromLdd(binaryPath string) ([]string, error) {
	libs, err := pkg.Ldd(binaryPath)
	if err != nil {
		return nil, err
	}

	var existing []string
	for _, lib := range libs {
		if _, err := os.Stat(lib); err == nil {
			existing = append(existing, lib)
		}
	}
	return existing, nil
}

func parsePkgRef(ref string) (name, version string) {
	if idx := lastIndexByte(ref, ':'); idx > 0 {
		return ref[:idx], ref[idx+1:]
	}
	return ref, ""
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func newPkgFromDockerCmd() *cobra.Command {
	var (
		libs        []string
		description string
		runtimeName string
	)
	cmd := &cobra.Command{
		Use:   "from-docker <name>[:<version>] <image>",
		Short: "Create a package from a binary inside a Docker image",
		Long: `Extract a binary and its shared library dependencies from a Docker image
and create a local package. Uses 'docker create' + 'docker cp' to extract
the binary, then runs 'ldd' inside the container to discover shared libraries.

Example:
  uni pkg from-docker node:20 node:20 --file /usr/local/bin/node --runtime node`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, version := parsePkgRef(args[0])
			if version == "" {
				version = "1.0.0"
			}
			dockerImage := args[1]

			filePath, _ := cmd.Flags().GetString("file")
			if filePath == "" {
				return fmt.Errorf("pkg from-docker: --file is required")
			}

			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg from-docker: %w", err)
			}

			if store.IsDownloaded(name, version) {
				return fmt.Errorf("pkg from-docker: package %s:%s already exists (remove it first)", name, version)
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "Extracting %s from Docker image %s...\n", filePath, dockerImage)
			files, err := pkg.FromDocker(dockerImage, filePath, libs)
			if err != nil {
				return fmt.Errorf("pkg from-docker: %w", err)
			}
			if len(files) == 0 {
				return fmt.Errorf("pkg from-docker: no files extracted from image")
			}

			binaryPath := files[0]
			extraFiles := files[1:]

			if err := store.Create(name, version, binaryPath, extraFiles, description, runtimeName); err != nil {
				return fmt.Errorf("pkg from-docker: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Package %s:%s created from Docker image %s (%d files).\n",
				name, version, dockerImage, len(files))
			return nil
		},
	}
	cmd.Flags().String("file", "", "Path to the binary inside the Docker image (required)")
	cmd.Flags().StringArrayVar(&libs, "libs", nil, "Additional library paths inside the container to include (repeatable)")
	cmd.Flags().StringVar(&description, "description", "", "Package description")
	cmd.Flags().StringVar(&runtimeName, "runtime", "", "Runtime family (e.g. node, python)")
	return cmd
}

func newPkgPushCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "push <name>[:<version>] <index-url>",
		Short: "Push a locally cached package to a remote package index",
		Long: `Push a locally cached package archive and metadata to a remote package index.
The index server must support POST /packages with multipart form data.

Example:
  uni pkg push node:20 https://packages.example.com`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, version := parsePkgRef(args[0])
			if version == "" {
				return fmt.Errorf("pkg push: version is required (use name:version)")
			}
			indexURL := args[1]

			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg push: %w", err)
			}

			if !store.IsDownloaded(name, version) {
				return fmt.Errorf("pkg push: package %s:%s not found locally (create or download it first)", name, version)
			}

			if err := store.Push(name, version, indexURL); err != nil {
				return fmt.Errorf("pkg push: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Pushed %s:%s to %s\n", name, version, indexURL)
			return nil
		},
	}
	return cmd
}

func newPkgLoadCmd() *cobra.Command {
	var source string
	var detach bool
	cmd := &cobra.Command{
		Use:   "load <package>",
		Short: "Download, build, and run a package in one step",
		Long: `Download a package, build a unikernel image from it, and run the image.
For ops packages, this replicates the 'ops pkg load' workflow.

Examples:
  uni pkg load eyberg/node:v16.5.0 --source ops
  uni pkg load myruntime:1.0.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var pkgFiles []pkg.File
			var binaryPath string
			var err error

			if source == "ops" {
				pkgFiles, err = resolveOpsPackages(cmd.Context(), []string{args[0]})
				if err != nil {
					return fmt.Errorf("pkg load ops: %w", err)
				}
				id, parseErr := pkg.ParseOpsIdentifier(args[0])
				if parseErr != nil {
					return fmt.Errorf("pkg load ops: %w", parseErr)
				}
				opsStore, storeErr := openOpsStore()
				if storeErr != nil {
					return fmt.Errorf("pkg load ops store: %w", storeErr)
				}
				binaryPath, err = opsStore.FindBinary(id.Namespace, id.Name, id.Version)
				if err != nil {
					return fmt.Errorf("pkg load ops: %w", err)
				}
			} else {
				pkgFiles, err = resolvePackages(cmd.Context(), []string{args[0]})
				if err != nil {
					return fmt.Errorf("pkg load: %w", err)
				}
				name, _ := parsePkgRef(args[0])
				pkgStore, storeErr := pkg.NewStore(pkgStorePath())
				if storeErr != nil {
					return fmt.Errorf("pkg load: %w", storeErr)
				}
				files, listErr := pkgStore.ExtractedFiles(name, "")
				if listErr != nil || len(files) == 0 {
					latestErr := fmt.Errorf("no extracted files found for %s", name)
					if listErr != nil {
						latestErr = listErr
					}
					return fmt.Errorf("pkg load: %w", latestErr)
				}
				binaryPath = files[0]
			}

			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return fmt.Errorf("pkg load: home dir: %w", homeErr)
			}
			imgStorePath := filepath.Join(home, ".uni", "images")
			imgStore, err := image.NewStore(imgStorePath)
			if err != nil {
				return fmt.Errorf("pkg load: open image store: %w", err)
			}

			toolsDir := defaultToolsPath()
			mkfsPath := os.Getenv("UNI_MKFS")
			mkfsRun, err := tools.ResolveMkfs(cmd.Context(), toolsDir, mkfsPath)
			if err != nil {
				return fmt.Errorf("pkg load: resolve mkfs: %w", err)
			}

			imageName := "pkg-load"
			m, err := image.NewBuilder(imgStore).Build(cmd.Context(), image.BuildConfig{
				Name:       imageName,
				BinaryPath: binaryPath,
				MkfsRun:    mkfsRun,
				Memory:     "256M",
				PkgFiles:   pkgFiles,
			})
			if err != nil {
				return fmt.Errorf("pkg load build: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Built image %s:%s (%s)\n", m.Name, m.Tag, m.DiskDigest)

			if detach {
				return nil
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Run with: uni run pkg-load:latest")
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "uni", "package source: \"uni\" (default) or \"ops\"")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "build only, don't print run instructions")
	return cmd
}
