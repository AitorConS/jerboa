package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	pkg "github.com/AitorConS/unikernel-engine/internal/package"
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
	)
	return cmd
}

// pkgStoreDir can be overridden in tests to use a temp directory.
var pkgStoreDir string

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

func newPkgListCmd() *cobra.Command {
	var outputJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List locally cached packages",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return cmd
}

func newPkgSearchCmd() *cobra.Command {
	var outputJSON bool
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the remote package index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return cmd
}

func newPkgGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>[:<version>]",
		Short: "Download a package from the remote index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return cmd
}

func newPkgRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove <name>[:<version>]",
		Short:   "Remove a locally cached package",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return cmd
}

func newPkgCreateCmd() *cobra.Command {
	var (
		libs        []string
		description string
		runtimeName string
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

			store, err := pkg.NewStore(pkgStorePath())
			if err != nil {
				return fmt.Errorf("pkg create: %w", err)
			}

			if err := store.Create(name, version, binaryPath, libs, description, runtimeName); err != nil {
				return fmt.Errorf("pkg create: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Package %s:%s created from %s.\n", name, version, filepath.Base(binaryPath))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&libs, "libs", nil, "Additional files to include (repeatable)")
	cmd.Flags().StringVar(&description, "description", "", "Package description")
	cmd.Flags().StringVar(&runtimeName, "runtime", "", "Runtime family (e.g. node, python)")
	return cmd
}

// parsePkgRef parses "name" or "name:version" into its components.
func parsePkgRef(ref string) (name, version string) {
	if idx := lastIndexByte(ref, ':'); idx > 0 {
		return ref[:idx], ref[idx+1:]
	}
	return ref, ""
}

// lastIndexByte returns the index of the last instance of sep in s,
// or -1 if sep is not present.
func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
