package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/api"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/spf13/cobra"
)

func newPkgCmd(endpoint *string) *cobra.Command {
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
		newPkgLoadCmd(endpoint),
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
		return filepath.Join(".jerboa", "packages")
	}
	return filepath.Join(home, ".jerboa", "packages")
}

func opsStorePath() string {
	if opsPkgStoreDir != "" {
		return opsPkgStoreDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".jerboa", "packages-ops")
	}
	return filepath.Join(home, ".jerboa", "packages-ops")
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
			// With no explicit --source, list BOTH sources so packages created
			// locally with `pkg create` (jerboa source) are visible without
			// knowing to pass --source jerboa.
			if !cmd.Flags().Changed("source") {
				if outputJSON {
					return pkgListBothJSON(cmd)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "OPS PACKAGES")
				if err := pkgListOps(cmd, false); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "\nJERBOA PACKAGES")
				return pkgListJerboa(cmd, false)
			}
			if source == "ops" {
				return pkgListOps(cmd, outputJSON)
			}
			return pkgListJerboa(cmd, outputJSON)
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "output-json", false, "output as JSON")
	cmd.Flags().StringVar(&source, "source", "ops", "package source: \"ops\" (default) or \"jerboa\"; omit to list both")
	return cmd
}

// pkgListJerboa lists locally created (jerboa-source) packages.
func pkgListJerboa(cmd *cobra.Command, outputJSON bool) error {
	store, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return fmt.Errorf("pkg list --source jerboa: %w", err)
	}
	pkgs, err := store.List()
	if err != nil {
		return fmt.Errorf("pkg list jerboa: %w", err)
	}
	if len(pkgs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No jerboa packages installed. Create one with 'jerboa pkg create <name> <binary>'.")
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
	if err := w.Flush(); err != nil {
		return fmt.Errorf("pkg list jerboa: %w", err)
	}
	return nil
}

// pkgListBothJSON emits both package sources as a single JSON object so the
// default `pkg list --output-json` stays valid JSON (not two concatenated arrays).
func pkgListBothJSON(cmd *cobra.Command) error {
	opsStore, err := openOpsStore()
	if err != nil {
		return fmt.Errorf("pkg list ops: %w", err)
	}
	opsPkgs, err := opsStore.List()
	if err != nil {
		return fmt.Errorf("pkg list ops: %w", err)
	}
	jStore, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return fmt.Errorf("pkg list jerboa: %w", err)
	}
	jPkgs, err := jStore.List()
	if err != nil {
		return fmt.Errorf("pkg list jerboa: %w", err)
	}
	return printJSON(cmd.OutOrStdout(), map[string]any{"ops": opsPkgs, "jerboa": jPkgs})
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
		fmt.Fprintln(cmd.OutOrStdout(), "No ops packages installed. Use 'jerboa pkg get <namespace>/<name>:<version> --source ops' to download.")
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
			pkgStore, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg search: %w", err)
			}
			idx, err := pkgStore.FetchIndexCached()
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
	cmd.Flags().StringVar(&source, "source", "ops", "package source: \"ops\" (default) or \"jerboa\"")
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

			pkgStore, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg get: %w", err)
			}
			idx, err := pkgStore.FetchIndexCached()
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

			if pkgStore.IsDownloaded(target.Name, target.Version) {
				fmt.Fprintf(cmd.OutOrStdout(), "Package %s %s already downloaded.\n", target.Name, target.Version)
				return nil
			}

			if err := pkgStore.Download(*target); err != nil {
				return fmt.Errorf("pkg get: %w", err)
			}
			if err := pkgStore.SaveMeta(*target); err != nil {
				return fmt.Errorf("pkg get: save meta: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Package %s %s installed.\n", target.Name, target.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "ops", "package source: \"ops\" (default) or \"jerboa\"")
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

	target := manifest.LookupArch(id.Namespace, id.Name, id.Version, pkg.ArchSlug())
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
	cmd.Flags().StringVar(&source, "source", "ops", "package source: \"ops\" (default) or \"jerboa\"")
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
		sysroot      string
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
			if sysroot != "" {
				sysroot, err = filepath.Abs(sysroot)
				if err != nil {
					return fmt.Errorf("pkg create: resolving sysroot: %w", err)
				}
				info, statErr := os.Stat(sysroot)
				if statErr != nil {
					return fmt.Errorf("pkg create: sysroot not found: %s", sysroot)
				}
				if !info.IsDir() {
					return fmt.Errorf("pkg create: sysroot not a directory: %s", sysroot)
				}
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

			// Warn on version/unresolved mismatches before building. Without a
			// sysroot, ldd resolves against the host, whose library versions may
			// not match the binary's — the resulting package would boot broken.
			if mismatches, mErr := pkg.LibMismatches(binaryPath, sysroot); mErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not check library compatibility: %v\n", mErr)
			} else if len(mismatches) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: shared library mismatch — the bundled libraries do not satisfy %s:\n", filepath.Base(binaryPath))
				for _, m := range mismatches {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", m)
				}
				if sysroot == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "Resolve against the binary's own rootfs with --sysroot <dir>, use a static binary, or build from Docker (pkg from-docker).\n")
				}
			}

			allLibs := libs
			resolved, err := resolveLibs(binaryPath, sysroot)
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
	cmd.Flags().StringVar(&sysroot, "sysroot", "", "Resolve shared libraries against this rootfs instead of the host (avoids version mismatch for foreign binaries)")
	return cmd
}

// resolveLibs auto-resolves the binary's shared libraries. When sysroot is set,
// libraries are picked from that rootfs (the distro the binary was built for);
// otherwise they are resolved against the host filesystem.
func resolveLibs(binaryPath, sysroot string) ([]string, error) {
	if sysroot != "" {
		return pkg.LddSysroot(binaryPath, sysroot)
	}
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
		Long: `Extract a binary and the shared library closure it needs from a Docker
image and create a local package. Each file is packaged at its real absolute
path inside the container (its interpreter under /lib64, its libraries under
/lib, …) so the built image satisfies the binary's dynamic-linking needs.

The image's exported filesystem is read directly, so this works on scratch and
distroless images with no shell or coreutils, and needs nothing installed in
the image itself.

Without --file, the binary is derived from the image's own Entrypoint/Cmd and
resolved on the container's PATH. Images whose entrypoint is a shell script
(the common docker-entrypoint.sh pattern) cannot be imported automatically —
a unikernel runs exactly one program with no shell — so those need an explicit
--file pointing at the real binary the script eventually launches.

Examples:
  jerboa pkg from-docker node:20 node:20 --runtime node
  jerboa pkg from-docker redis:7.2 redis:7.2 --file /usr/local/bin/redis-server`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, version := parsePkgRef(args[0])
			if version == "" {
				version = "1.0.0"
			}
			dockerImage := args[1]

			filePath, _ := cmd.Flags().GetString("file")
			warnMangledImagePath(cmd.ErrOrStderr(), "--file", filePath)
			if filePath == "" {
				resolved, err := deriveDockerProgram(cmd, dockerImage)
				if err != nil {
					return fmt.Errorf("pkg from-docker: %w", err)
				}
				filePath = resolved
			}

			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg from-docker: %w", err)
			}

			if store.IsDownloaded(name, version) {
				return fmt.Errorf("pkg from-docker: package %s:%s already exists (remove it first)", name, version)
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "Extracting %s and its library closure from Docker image %s...\n", filePath, dockerImage)
			files, cleanup, err := pkg.FromDocker(dockerImage, filePath, libs)
			if err != nil {
				return fmt.Errorf("pkg from-docker: %w", err)
			}
			defer cleanup()
			if len(files) == 0 {
				return fmt.Errorf("pkg from-docker: no files extracted from image")
			}

			if err := store.CreateFromFiles(name, version, files, description, runtimeName); err != nil {
				return fmt.Errorf("pkg from-docker: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Package %s:%s created from Docker image %s (%d files).\n",
				name, version, dockerImage, len(files))
			return nil
		},
	}
	cmd.Flags().String("file", "", "Path to the binary inside the Docker image (default: derived from the image's Entrypoint/Cmd)")
	cmd.Flags().StringArrayVar(&libs, "libs", nil, "Additional library paths inside the container to include (repeatable)")
	cmd.Flags().StringVar(&description, "description", "", "Package description")
	cmd.Flags().StringVar(&runtimeName, "runtime", "", "Runtime family (e.g. node, python)")
	return cmd
}

// deriveDockerProgram determines the binary to extract from a Docker image when
// --file is not given: it reads the image's Entrypoint/Cmd, rejects shell
// launchers (a unikernel cannot run them), and resolves bare names on the
// container's PATH. It also surfaces the image's declared environment so the
// user can bake relevant variables into unikernel.toml [env].
func deriveDockerProgram(cmd *cobra.Command, dockerImage string) (string, error) {
	cfg, err := pkg.InspectDockerImage(dockerImage)
	if err != nil {
		return "", err
	}
	candidate := cfg.ProgramCandidate()
	if candidate == "" {
		return "", fmt.Errorf("image %s declares no Entrypoint or Cmd; pass --file with the binary to extract", dockerImage)
	}
	if pkg.IsShellLauncher(candidate) {
		return "", fmt.Errorf(
			"image %s starts through a shell launcher (%s), which cannot run in a single-process unikernel;\n"+
				"pass --file with the real binary the script launches (e.g. --file /usr/local/bin/redis-server)",
			dockerImage, candidate)
	}
	resolved, err := pkg.ResolveDockerProgramPath(dockerImage, candidate)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Derived program from image config: %s", resolved)
	if args := cfg.ProgramArgs(); len(args) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), " (image args: %v — pass them via [program] args at build time)", args)
	}
	fmt.Fprintln(cmd.ErrOrStderr())
	// The image env is not stored in the package (jerboa package meta has no
	// env field yet); print it so the user can carry over what matters.
	if len(cfg.Env) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Image declares environment variables (bake needed ones into unikernel.toml [env]):\n")
		for _, e := range cfg.Env {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", e)
		}
	}
	return resolved, nil
}

func newPkgPushCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "push <name>[:<version>] <index-url>",
		Short: "Push a locally cached package to a remote package index",
		Long: `Push a locally cached package archive and metadata to a remote package index.
The index server must support POST /packages with multipart form data.

Example:
  jerboa pkg push node:20 https://packages.example.com`,
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

func newPkgLoadCmd(endpoint *string) *cobra.Command {
	var source string
	var detach bool
	cmd := &cobra.Command{
		Use:   "load <package>",
		Short: "Download, build, and run a package in one step",
		Long: `Download a package, build a unikernel image from it, and run the image.
For ops packages, this replicates the 'ops pkg load' workflow.

Examples:
  jerboa pkg load eyberg/node:v16.5.0 --source ops
  jerboa pkg load myruntime:1.0.0`,
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

			// Stream the package to the daemon, which assembles the image with
			// mkfs on its own filesystem and stores it.
			client, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("pkg load: connect to daemon: %w", err)
			}
			defer func() { _ = client.Close() }()

			pr := buildContextReader(binaryPath, pkgFiles)
			defer func() { _ = pr.Close() }()
			res, err := client.ImageBuild(cmd.Context(), api.BuildParams{
				Name:    "pkg-load",
				Program: buildProgramPath,
				Memory:  "256M",
			}, pr)
			if err != nil {
				return fmt.Errorf("pkg load build: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Built image %s:%s (%s)\n", res.Name, res.Tag, res.DiskDigest)

			if detach {
				return nil
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Run with: jerboa run pkg-load:latest")
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "ops", "package source: \"ops\" (default) or \"jerboa\"")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "build only, don't print run instructions")
	return cmd
}
