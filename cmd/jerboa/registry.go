package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
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
func localName(target string) (string, string) {
	if ref, err := naming.ParseImageReference(target); err == nil && ref.Digest == "" {
		return ref.Name(), ref.Tag
	}
	if i := strings.LastIndex(target, ":"); i > 0 && !strings.Contains(target[i:], "/") {
		return target[:i], target[i+1:]
	}
	return target, "latest"
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
			name, tag := localName(args[1])
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
			res, err := c.ImagePull(cmd.Context(), api.ImagePullParams{Reference: args[0]})
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pulled %s:%s: fetched %d of %d files (%s) in %d requests\n%s\n",
				res.Image.Name, res.Image.Tag, res.BlobsFetched, res.Blobs, humanBytes(res.BytesFetched), res.Requests, res.Image.DiskDigest)
			return nil
		},
	}
}
