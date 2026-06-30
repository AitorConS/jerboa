package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/api"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/spf13/cobra"
)

func newVolumeCmd(endpoint *string, storePath *string, outputFmt *string, verbose *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage persistent volumes",
	}
	cmd.AddCommand(
		newVolumeCreateCmd(storePath),
		newVolumeLsCmd(storePath, outputFmt),
		newVolumeRmCmd(storePath),
		newVolumeInspectCmd(storePath),
		newVolumeSeedCmd(endpoint, storePath, verbose),
	)
	return cmd
}

// newVolumeSeedCmd populates a volume with an initialized filesystem taken from
// one or more packages — e.g. a database's pre-initialized data directory — so
// the data persists across VM lifecycles. The files are resolved from --pkg,
// optionally narrowed to a subtree with --src (whose contents become the volume
// root), streamed to the daemon, and written into the volume's disk with mkfs.
func newVolumeSeedCmd(endpoint *string, storePath *string, verbose *bool) *cobra.Command {
	var (
		pkgs      []string
		pkgSource string
		src       string
	)
	cmd := &cobra.Command{
		Use:   "seed <name>",
		Short: "Populate a volume with initialized data from a package",
		Long: `Seed an existing volume with files from one or more packages.

Use this to make a database persistent: a package such as eyberg/postgresql
ships a pre-initialized data directory (initdb cannot run inside a unikernel),
which can be written onto a volume once and then mounted, so the cluster
survives recreating the VM.

  jerboa volume create pgdata --size 1G
  jerboa volume seed pgdata --pkg eyberg/postgresql:11.3.0 --pkg-source ops --src /db
  jerboa run postgresql -v pgdata:/db --network pgnet -p 5432:5432`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if len(pkgs) == 0 {
				return fmt.Errorf("volume seed: at least one --pkg is required")
			}

			sp := newSpinner(cmd.ErrOrStderr(), *verbose)

			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume seed: %w", err)
			}
			vol, err := store.Get(name)
			if err != nil {
				return fmt.Errorf("volume seed: volume %q not found (create it with 'jerboa volume create %s'): %w", name, name, err)
			}

			sp.Start("Resolving packages")
			var files []pkg.File
			if pkgSource == "ops" {
				files, err = resolveOpsPackages(cmd.Context(), pkgs)
			} else {
				files, err = resolvePackages(cmd.Context(), pkgs)
			}
			if err != nil {
				sp.Fail("Package resolution failed")
				return fmt.Errorf("volume seed: %w", err)
			}
			sp.Done(fmt.Sprintf("Resolved %s", strings.Join(pkgs, ", ")))

			seedFiles, err := remapSeedFiles(files, src)
			if err != nil {
				return fmt.Errorf("volume seed: %w", err)
			}

			label := vol.Label
			if label == "" {
				label = volume.SanitizeLabel(vol.ID)
			}

			sp.Start("Seeding volume on daemon")
			client, err := api.Dial(*endpoint)
			if err != nil {
				sp.Fail("Volume seeding failed")
				return fmt.Errorf("volume seed: connect to daemon: %w", err)
			}
			defer func() { _ = client.Close() }()

			pr := seedContextReader(seedFiles)
			defer func() { _ = pr.Close() }()
			res, err := client.VolumeSeed(cmd.Context(), api.VolumeSeedParams{
				DiskPath:  hostPathForDaemon(vol.DiskPath),
				Label:     label,
				SizeBytes: vol.SizeBytes,
			}, pr)
			if err != nil {
				sp.Fail("Volume seeding failed")
				return fmt.Errorf("volume seed: %w", err)
			}
			sp.Done(fmt.Sprintf("Seeded %s  ·  %s", name, formatSize(res.SizeBytes)))

			fmt.Fprintf(cmd.OutOrStdout(), "%s seeded from %s (%s)\n", name, strings.Join(pkgs, ", "), formatSize(res.SizeBytes))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&pkgs, "pkg", nil, "package providing the seed files (repeatable)")
	cmd.Flags().StringVar(&pkgSource, "pkg-source", "jerboa", "package source: \"jerboa\" (default) or \"ops\"")
	cmd.Flags().StringVar(&src, "src", "/", "in-package subtree whose contents become the volume root (e.g. /db)")
	return cmd
}

// remapSeedFiles narrows resolved package files to those under src and rebases
// their guest paths so src's contents sit at the volume root. src "/" keeps the
// whole tree. The volume is mounted onto a guest directory at run time, so the
// seeded files must be rooted there (e.g. /db's children become the volume root,
// and mounting at /db restores them).
func remapSeedFiles(files []pkg.File, src string) ([]pkg.File, error) {
	want := strings.Trim(filepath.ToSlash(src), "/")
	out := make([]pkg.File, 0, len(files))
	for _, f := range files {
		gp := strings.TrimPrefix(filepath.ToSlash(f.GuestPath), "/")
		var rel string
		switch {
		case want == "":
			rel = gp
		case gp == want:
			continue // the subtree root dir itself; its children carry the data
		case strings.HasPrefix(gp, want+"/"):
			rel = strings.TrimPrefix(gp, want+"/")
		default:
			continue
		}
		if rel == "" {
			continue
		}
		out = append(out, pkg.File{HostPath: f.HostPath, GuestPath: rel, IsDir: f.IsDir})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no files found under --src %q in the resolved packages", src)
	}
	return out, nil
}

func newVolumeCreateCmd(storePath *string) *cobra.Command {
	var size string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new named volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			sizeBytes, err := volume.ParseSize(size)
			if err != nil {
				return fmt.Errorf("volume create: invalid size: %w", err)
			}
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume create: %w", err)
			}
			v, err := store.Create(name, sizeBytes)
			if err != nil {
				return fmt.Errorf("volume create: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), v.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&size, "size", "1G", "volume size (e.g. 512M, 1G, 2G)")
	return cmd
}

func newVolumeLsCmd(storePath *string, outputFmt *string) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List volumes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume ls: %w", err)
			}
			vols, err := store.List()
			if err != nil {
				return fmt.Errorf("volume ls: %w", err)
			}
			if *outputFmt == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(vols)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSIZE\tCREATED")
			for _, v := range vols {
				fmt.Fprintf(w, "%s\t%s\t%s\n",
					v.ID,
					humanBytes(v.SizeBytes),
					v.CreatedAt.Format("2006-01-02 15:04:05"),
				)
			}
			return w.Flush()
		},
	}
}

func newVolumeRmCmd(storePath *string) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a volume",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume rm: %w", err)
			}
			if err := store.Remove(args[0]); err != nil {
				return fmt.Errorf("volume rm: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), args[0])
			return nil
		},
	}
}

func newVolumeInspectCmd(storePath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name>",
		Short: "Show detailed information about a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := volume.NewStore(volumeStorePath(*storePath))
			if err != nil {
				return fmt.Errorf("volume inspect: %w", err)
			}
			v, err := store.Get(args[0])
			if err != nil {
				return fmt.Errorf("volume inspect: %w", err)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(v)
		},
	}
}

func humanBytes(b int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1fG", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.1fM", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.1fK", float64(b)/KB)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
