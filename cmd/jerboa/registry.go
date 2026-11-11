package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/naming"
	pkg "github.com/AitorConS/jerboa/internal/package"
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
				entry.Token, entry.Login, err = githubLogin(cmd.Context(), cmd.OutOrStdout(), registry, cl, d, defaultClientID)
				if err != nil {
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

// githubLogin signs in to registry with GitHub and returns its refresh token
// and the account's login. A registry whose discovery names
// client.LoginDevice runs the device flow itself, and the GitHub token never
// reaches this client; with an older one, the client runs the flow with the
// app it advertises (githubClientID) and hands the registry the token.
func githubLogin(ctx context.Context, w io.Writer, registry string, cl *client.Client, d client.Discovery, official func(context.Context) (string, error)) (token, login string, err error) {
	if d.Login == client.LoginDevice {
		l, err := cl.StartLogin(ctx)
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(w, "Open %s and enter the code %s to log in to %s\n", l.VerificationURI, l.UserCode, registry)
		return cl.WaitLogin(ctx, l)
	}
	clientID, err := githubClientID(ctx, registry, d, official)
	if err != nil {
		return "", "", err
	}
	flow := client.DeviceFlow{GitHub: os.Getenv("JERBOA_GITHUB_URL")}
	dc, err := flow.Start(ctx, clientID)
	if err != nil {
		return "", "", err
	}
	fmt.Fprintf(w, "Open %s and enter the code %s\n", dc.VerificationURI, dc.UserCode)
	gh, err := flow.Wait(ctx, clientID, dc)
	if err != nil {
		return "", "", err
	}
	return cl.LoginWithGitHub(ctx, gh)
}

// githubClientID returns the GitHub app to log in to registry with. The
// default registry accepts any token GitHub issues for its app, so such a
// token must not be handed to another registry: one that names the default
// registry's app is refused. official returns the default registry's app.
func githubClientID(ctx context.Context, registry string, d client.Discovery, official func(context.Context) (string, error)) (string, error) {
	if d.GitHubClientID == "" {
		return "", fmt.Errorf("%s does not offer GitHub login; use --token", registry)
	}
	if registry == naming.DefaultRegistry {
		return d.GitHubClientID, nil
	}
	id, err := official(ctx)
	if err != nil {
		return "", fmt.Errorf("checking %s's GitHub app against %s: %w; use --token", registry, naming.DefaultRegistry, err)
	}
	if d.GitHubClientID == id {
		return "", fmt.Errorf("%s offers %s's GitHub app, and a token for it would let %s act as you on %s; use --token",
			registry, naming.DefaultRegistry, registry, naming.DefaultRegistry)
	}
	return d.GitHubClientID, nil
}

// defaultClientID returns the GitHub app of the default registry.
func defaultClientID(ctx context.Context) (string, error) {
	d, err := client.New(naming.DefaultRegistry).Discover(ctx)
	return d.GitHubClientID, err
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
	var index, sign bool
	cmd := &cobra.Command{
		Use:   "push <[registry/]namespace/repository[:tag]>",
		Short: "Push an image to a registry as its recipe and the files the registry lacks",
		Long: `Push an image to a registry as its recipe and the files the registry lacks.

With --index, push no image: tag an image index of images already pushed,
one per platform, so the tag pulls the image built for each host.

A trusted publisher's push (a GitHub Actions job with permissions:
id-token: write, pushing with the job's OIDC token) is also signed with
Sigstore: attestations of the image's provenance (the repository, commit,
workflow and run) and of its SBOM are stored with it, for pulls to verify.
--sign=false pushes unsigned.`,
		Example: `  jerboa push aitorcons/web:v1
  jerboa push --index aitorcons/web:v1 aitorcons/web:v1-amd64 aitorcons/web:v1-arm64`,
		Args: func(cmd *cobra.Command, args []string) error {
			if index {
				return cobra.MinimumNArgs(2)(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			if ref.Digest != "" {
				return fmt.Errorf("push: %s: push to a tag, not a digest", args[0])
			}
			cred, err := credentials.For(ref.Registry)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			cl := client.New(ref.Registry)
			token, err := cl.PushToken(cmd.Context(), cred, ref.Namespace, ref.Repository)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			d, err := cl.Discover(cmd.Context())
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			signed, err := shouldSign(cmd, sign, d, ref.Registry, cred)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			if index {
				subject, err := pushIndex(cmd, cl, token, ref, args[1:])
				if err != nil || !signed {
					return err
				}
				if err := signPushed(cmd.Context(), cmd.OutOrStdout(), cl, token, d.CDN, ref, subject, nil); err != nil {
					return fmt.Errorf("push: sign: %w", err)
				}
				return nil
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
			if !signed {
				return nil
			}
			sbom, err := c.ImageSBOM(cmd.Context(), ref.Name()+":"+ref.Tag)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: signing without an SBOM: %v\n", err)
			}
			if err := signPushed(cmd.Context(), cmd.OutOrStdout(), cl, token, d.CDN, ref, res.Recipe, sbom); err != nil {
				return fmt.Errorf("push: sign: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&index, "index", false, "tag an image index of the images named after the target, one per platform")
	cmd.Flags().BoolVar(&sign, "sign", false, "sign the push with Sigstore (default: in GitHub Actions jobs that may request OIDC tokens)")
	return cmd
}

// pushIndex tags ref as an image index of the images sources name, and
// returns the index's digest.
func pushIndex(cmd *cobra.Command, cl *client.Client, token string, ref naming.ImageReference, sources []string) (string, error) {
	refs := make([]naming.ImageReference, len(sources))
	for i, s := range sources {
		r, err := naming.ParseImageReference(s)
		if err != nil {
			return "", fmt.Errorf("push: %w", err)
		}
		refs[i] = r
	}
	res, err := transfer.PushIndex(cmd.Context(), transfer.IndexOptions{Client: cl, Token: token, Ref: ref, Sources: refs})
	if err != nil {
		return "", err
	}
	platforms := make([]string, len(res.Manifests))
	for i, e := range res.Manifests {
		platforms[i] = e.Platform
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Pushed %s: an index for %s\n%s\n", ref, strings.Join(platforms, ", "), res.Index)
	return res.Index, nil
}

func newPullCmd(endpoint *string) *cobra.Command {
	var platform, identity string
	var verify bool
	cmd := &cobra.Command{
		Use:   "pull <[registry/]namespace/repository[:tag|@digest]>",
		Short: "Pull an image: fetch the files the daemon lacks and assemble its disk locally",
		Long: `Pull an image: fetch the files the daemon lacks and assemble its disk locally.

A tag published for several platforms pulls the image for this host, or the
one --platform names.

A signed image is pulled only if its Sigstore signatures verify, signed by:
--identity, else the rule of the policy file (~/.jerboa/policy.json) for its
namespace (official images under jerboa/ always have one), else the signer
remembered for its repository, else the workflow the registry recorded as
their uploader. The first pull of a repository that verifies remembers its
signer (~/.jerboa/signers.json): later pulls refuse another signer, or an
unsigned image. --verify refuses unsigned images too. The check comes first:
a pull that fails it downloads nothing and leaves the image store as it was.
See jerboa registry verify and signers.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if platform != "" {
				if err := pkg.ValidatePlatform(platform); err != nil {
					return fmt.Errorf("pull: %w", err)
				}
			}
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			v, err := verificationFor(ref, identity, verify)
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			c, err := api.Dial(*endpoint)
			if err != nil {
				return fmt.Errorf("pull: connect to daemon: %w", err)
			}
			defer func() { _ = c.Close() }()
			// Verified before the daemon fetches anything, and the daemon pulls
			// the recipe that verified, not what the tag names a moment later:
			// an image that fails the check never reaches the store.
			subject, err := resolveSubject(cmd.Context(), ref, platform)
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			results, err := verifySubject(cmd.Context(), cmd.OutOrStdout(), ref, subject, v)
			if err != nil {
				return fmt.Errorf("pull: %w; nothing was pulled", err)
			}
			if results != nil {
				printVerified(cmd.OutOrStdout(), subject, results)
			}
			res, err := c.ImagePull(cmd.Context(), api.ImagePullParams{Reference: args[0], NoCount: transfer.DoNotTrack(), Platform: platform, Recipe: subject})
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			// A daemon older than this CLI resolves the tag itself.
			if res.Recipe != subject {
				err := fmt.Errorf("the daemon pulled %s, not %s, the recipe that was checked: update the daemon", res.Recipe, subject)
				if rmErr := c.ImageRemove(cmd.Context(), res.Image.Name+":"+res.Image.Tag); rmErr != nil {
					return fmt.Errorf("pull: %w (and removing the pulled image failed: %w)", err, rmErr)
				}
				return fmt.Errorf("pull: %w; the pulled image was removed", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pulled %s:%s (%s): fetched %d of %d files (%s) in %d requests\n%s\n",
				res.Image.Name, res.Image.Tag, res.Image.Platform, res.BlobsFetched, res.Blobs, humanBytes(res.BytesFetched), res.Requests, res.Image.DiskDigest)
			return nil
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "", "the platform to pull, linux/amd64 or linux/arm64 (default: this host's)")
	cmd.Flags().BoolVar(&verify, "verify", false, "pull the image only if it is signed, even the first pull of a repository")
	cmd.Flags().StringVar(&identity, "identity", "", "pull the image only if it is signed from this GitHub repository[/workflow[@ref]] (implies --verify)")
	return cmd
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
			// 0 would be dropped from the request and get the registry's
			// default, not what was asked for.
			if cmd.Flags().Changed("expires-in") && (patOpts.ExpiresInDays < 1 || patOpts.ExpiresInDays > 365) {
				return fmt.Errorf("pat create: --expires-in must be from 1 to 365 days, got %d", patOpts.ExpiresInDays)
			}
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
	cmd.AddCommand(pat, newRepoCmd(&registry), newRegistryVerifyCmd(), newRegistrySignersCmd())
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
	repo.AddCommand(edit, newRepoRmCmd(registry))
	return repo
}

func newRepoRmCmd(registry *string) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <[registry/]namespace/repository[:tag]>",
		Short: "Delete a tag, or a repository with all its tags",
		Long: `Deletes a tag from a registry, or, without a tag, the repository with all its
tags, description and README. It needs the namespace's owner, an organization
admin, or a personal access token created with --delete.`,
		Example: `  jerboa registry repo rm aitorcons/web:v1
  jerboa registry repo rm aitorcons/web --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("repo rm: %w", err)
			}
			if ref.Digest != "" {
				return fmt.Errorf("repo rm: name a tag or a repository, not a digest")
			}
			// ParseImageReference defaults the tag to latest; only a tag the
			// user wrote selects a tag.
			tagged := strings.Contains(args[0][strings.LastIndex(args[0], "/")+1:], ":")
			if ref.Registry == naming.DefaultRegistry {
				ref.Registry = *registry
			}
			name := ref.Namespace + "/" + ref.Repository
			if !tagged && !yes && !confirmPrompt(fmt.Sprintf("Delete %s with all its tags from %s? [y/N] ", name, ref.Registry)) {
				fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
				return nil
			}
			cred, err := credentials.For(ref.Registry)
			if err != nil {
				return fmt.Errorf("repo rm: %w", err)
			}
			c := client.New(ref.Registry)
			token, err := c.DeleteToken(cmd.Context(), cred, ref.Namespace, ref.Repository)
			if err != nil {
				return fmt.Errorf("repo rm: %w", err)
			}
			if tagged {
				if err := c.DeleteTag(cmd.Context(), token, ref.Namespace, ref.Repository, ref.Tag); err != nil {
					return fmt.Errorf("repo rm: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s:%s\n", name, ref.Tag)
				return nil
			}
			if err := c.DeleteRepo(cmd.Context(), token, ref.Namespace, ref.Repository); err != nil {
				return fmt.Errorf("repo rm: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete a whole repository without asking")
	return cmd
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
