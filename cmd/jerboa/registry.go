package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/AitorConS/jerboa/internal/registry/transfer"
	"github.com/spf13/cobra"
)

func registryArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return naming.DefaultRegistry
}

func newLoginCmd() *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "login [registry]",
		Short: "Log in to an image registry with GitHub, or store a personal access token",
		Long: `Logs in to an image registry (default ` + naming.DefaultRegistry + `).

Without --token, opens GitHub's device flow: approve the code shown in a
browser and the registry issues a token for your GitHub account, which may push
to your user and the organizations you belong to. With --token (or ` + credentials.EnvToken + `),
stores a personal access token, as CI does.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			registry := registryArg(args)
			cl := client.New(registry)
			d, err := cl.Discover(cmd.Context())
			if err != nil {
				return fmt.Errorf("login: %w", err)
			}
			entry := credentials.Entry{Token: token}
			if entry.Token == "" {
				flow := client.DeviceFlow{GitHub: os.Getenv("JERBOA_GITHUB_URL")}
				dc, err := flow.Start(cmd.Context(), d.GitHubClientID)
				if err != nil {
					return fmt.Errorf("login: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Open %s and enter the code %s\n", dc.VerificationURI, dc.UserCode)
				gh, err := flow.Wait(cmd.Context(), d.GitHubClientID, dc)
				if err != nil {
					return fmt.Errorf("login: %w", err)
				}
				if entry.Token, entry.Login, err = cl.LoginWithGitHub(cmd.Context(), gh); err != nil {
					return fmt.Errorf("login: %w", err)
				}
			}
			f, err := credentials.Load()
			if err != nil {
				return err
			}
			f.Registries[registry] = entry
			if err := f.Save(); err != nil {
				return err
			}
			if entry.Login != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s as %s\n", registry, entry.Login)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Stored a token for %s\n", registry)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "store this personal access token instead of logging in with GitHub")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout [registry]",
		Short: "Forget the credential stored for an image registry",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			registry := registryArg(args)
			f, err := credentials.Load()
			if err != nil {
				return err
			}
			delete(f.Registries, registry)
			if err := f.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged out of %s\n", registry)
			return nil
		},
	}
}

// localName splits a tag target: a registry reference becomes its store name
// and tag; anything else is name[:tag].
func localName(target string) (string, string, error) {
	// A name with a slash is a registry reference: check it now, not at push.
	if strings.Contains(target, "/") {
		ref, err := naming.ParseImageReference(target)
		if err != nil {
			return "", "", err
		}
		if ref.Digest != "" {
			return "", "", fmt.Errorf("%s: tag with a tag, not a digest", target)
		}
		return ref.Name(), ref.Tag, nil
	}
	if i := strings.LastIndex(target, ":"); i > 0 {
		return target[:i], target[i+1:], nil
	}
	return target, "latest", nil
}

func newTagCmd(endpoint *string) *cobra.Command {
	return &cobra.Command{
		Use:   "tag <image> <target>",
		Short: "Give an image another name, such as the registry reference to push it as",
		Example: `  jerboa tag web:latest aitorcons/web:v1
  jerboa push aitorcons/web:v1`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("tag: connect to daemon: %w", err)
			}
			defer func() { _ = c.Close() }()
			name, tag, err := localName(args[1])
			if err != nil {
				return fmt.Errorf("tag: %w", err)
			}
			if _, err := c.ImageTag(cmd.Context(), api.ImageTagParams{Source: args[0], Name: name, Tag: tag}); err != nil {
				return fmt.Errorf("tag: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s:%s\n", name, tag)
			return nil
		},
	}
}

func newPushCmd(endpoint *string) *cobra.Command {
	return &cobra.Command{
		Use:   "push <[registry/]namespace/repository[:tag]>",
		Short: "Push an image to a registry as its recipe and the files the registry lacks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			cred, err := credentials.For(ref.Registry)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			token, err := client.New(ref.Registry).PushToken(cmd.Context(), cred, ref.Namespace, ref.Repository)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			c, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("push: connect to daemon: %w", err)
			}
			defer func() { _ = c.Close() }()
			res, err := c.ImagePush(cmd.Context(), api.ImagePushParams{Reference: args[0], Token: token})
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pushed %s: uploaded %d of %d files (%s) in %d packs\n%s\n",
				res.Reference, res.BlobsUploaded, res.Blobs, humanBytes(res.BytesUploaded), res.PacksUploaded, res.Recipe)
			return nil
		},
	}
}

func newPullCmd(endpoint *string) *cobra.Command {
	return &cobra.Command{
		Use:   "pull <[registry/]namespace/repository[:tag|@digest]>",
		Short: "Pull an image: fetch the files the daemon lacks and assemble its disk locally",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("pull: connect to daemon: %w", err)
			}
			defer func() { _ = c.Close() }()
			res, err := c.ImagePull(cmd.Context(), api.ImagePullParams{Reference: args[0], NoCount: transfer.DoNotTrack()})
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pulled %s:%s: fetched %d of %d files (%s) in %d requests\n%s\n",
				res.Image.Name, res.Image.Tag, res.BlobsFetched, res.Blobs, humanBytes(res.BytesFetched), res.Requests, res.Image.DiskDigest)
			return nil
		},
	}
}

func newRegistryCmd() *cobra.Command {
	var registry string
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage your account on an image registry",
	}
	cmd.PersistentFlags().StringVar(&registry, "registry", naming.DefaultRegistry, "registry host")
	pat := &cobra.Command{Use: "pat", Short: "Personal access tokens, for CI"}
	var patOpts client.PATOptions
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a personal access token; it is shown once",
		Example: `  jerboa registry pat create ci
  jerboa registry pat create release --namespace acme --expires-in 30`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cred, err := credentials.For(registry)
			if err != nil {
				return err
			}
			opts := patOpts
			opts.Name = args[0]
			id, token, err := client.New(registry).CreatePAT(cmd.Context(), cred, opts)
			if err != nil {
				return fmt.Errorf("pat create: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", token)
			fmt.Fprintf(cmd.ErrOrStderr(), "Created token %s (%s). Store it as %s in CI; it is not shown again.\n", args[0], id, credentials.EnvToken)
			return nil
		},
	}
	create.Flags().IntVar(&patOpts.ExpiresInDays, "expires-in", 0, "days until the token expires, 1 to 365 (default: the registry's, 90)")
	create.Flags().StringArrayVar(&patOpts.Namespaces, "namespace", nil, "namespace the token may push to (repeatable; default: all of yours)")
	create.Flags().BoolVar(&patOpts.Delete, "delete", false, "let the token delete tags and repositories")
	pat.AddCommand(create)
	pat.AddCommand(&cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List personal access tokens",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cred, err := credentials.For(registry)
			if err != nil {
				return err
			}
			pats, err := client.New(registry).ListPATs(cmd.Context(), cred)
			if err != nil {
				return fmt.Errorf("pat ls: %w", err)
			}
			for _, p := range pats {
				expires := "never"
				if p.ExpiresAt != nil {
					expires = time.Unix(*p.ExpiresAt, 0).UTC().Format("2006-01-02")
				}
				scope := "all namespaces"
				if len(p.Namespaces) > 0 {
					scope = strings.Join(p.Namespaces, ",")
				}
				if p.Delete {
					scope += " +delete"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\texpires %s\t%s\n", p.ID, p.Name, expires, scope)
			}
			return nil
		},
	})
	pat.AddCommand(&cobra.Command{
		Use:   "rm <id>",
		Short: "Revoke a personal access token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cred, err := credentials.For(registry)
			if err != nil {
				return err
			}
			if err := client.New(registry).RevokePAT(cmd.Context(), cred, args[0]); err != nil {
				return fmt.Errorf("pat rm: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s\n", args[0])
			return nil
		},
	})
	cmd.AddCommand(pat, newRepoCmd(&registry))
	return cmd
}

func newRepoCmd(registry *string) *cobra.Command {
	repo := &cobra.Command{Use: "repo", Short: "Repository details shown on the hub"}
	var description, readme string
	edit := &cobra.Command{
		Use:   "edit <[registry/]namespace/repository>",
		Short: "Set a repository's description or README",
		Example: `  jerboa registry repo edit aitorcons/web --description "Static web server"
  jerboa registry repo edit aitorcons/web --readme README.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("repo edit: %w", err)
			}
			if ref.Digest != "" || strings.Contains(args[0][strings.LastIndex(args[0], "/")+1:], ":") {
				return fmt.Errorf("repo edit: name a repository, without a tag or digest")
			}
			if ref.Registry == naming.DefaultRegistry {
				ref.Registry = *registry
			}
			var changes client.RepoChanges
			if cmd.Flags().Changed("description") {
				changes.Description = &description
			}
			if cmd.Flags().Changed("readme") {
				data, err := readFileOrStdin(cmd, readme)
				if err != nil {
					return fmt.Errorf("repo edit: %w", err)
				}
				text := string(data)
				changes.Readme = &text
			}
			if changes.Description == nil && changes.Readme == nil {
				return fmt.Errorf("repo edit: set --description, --readme or both")
			}
			cred, err := credentials.For(ref.Registry)
			if err != nil {
				return fmt.Errorf("repo edit: %w", err)
			}
			c := client.New(ref.Registry)
			token, err := c.PushToken(cmd.Context(), cred, ref.Namespace, ref.Repository)
			if err != nil {
				return fmt.Errorf("repo edit: %w", err)
			}
			if err := c.EditRepo(cmd.Context(), token, ref.Namespace, ref.Repository, changes); err != nil {
				return fmt.Errorf("repo edit: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %s/%s\n", ref.Namespace, ref.Repository)
			return nil
		},
	}
	edit.Flags().StringVar(&description, "description", "", "one-line description, at most 200 characters")
	edit.Flags().StringVar(&readme, "readme", "", "Markdown file to show as the README, or - for stdin (at most 64 KiB)")
	repo.AddCommand(edit)
	return repo
}

// readFileOrStdin reads a file, or standard input for "-".
func readFileOrStdin(cmd *cobra.Command, path string) ([]byte, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	} else {
		data, err = os.ReadFile(path) //nolint:gosec // a file the user names
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}
