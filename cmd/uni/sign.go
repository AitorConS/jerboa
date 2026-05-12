package main

import (
	"fmt"
	"os"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/signing"
	"github.com/spf13/cobra"
)

func newSignCmd(storePath *string) *cobra.Command {
	var keyPath string
	cmd := &cobra.Command{
		Use:   "sign <image>",
		Short: "Sign a local image with the default Ed25519 key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			store, err := image.NewStore(*storePath)
			if err != nil {
				return fmt.Errorf("sign: open store: %w", err)
			}
			m, diskPath, err := store.Get(ref)
			if err != nil {
				return fmt.Errorf("sign: %w", err)
			}
			_ = diskPath

			home, err := signingStorePath()
			if err != nil {
				return fmt.Errorf("sign: %w", err)
			}
			sigStore, err := signing.NewStore(home)
			if err != nil {
				return fmt.Errorf("sign: open signing store: %w", err)
			}

			manifestData, err := image.Marshal(m)
			if err != nil {
				return fmt.Errorf("sign: marshal manifest: %w", err)
			}
			digest := image.DigestSHA256(manifestData)

			imageDir := imageDirFromDigest(home, m.DiskDigest)
			if keyPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: --key flag is informational; using default key pair\n")
			}

			sig, err := sigStore.SignManifest(digest, imageDir)
			if err != nil {
				return fmt.Errorf("sign: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "signed %s (key %s, digest %s)\n", ref, sig.KeyID, sig.Digest)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyPath, "key", "", "path to signing key (uses default key pair)")
	return cmd
}

func newVerifyCmd(storePath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify <image>",
		Short: "Verify the signature of a local image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			store, err := image.NewStore(*storePath)
			if err != nil {
				return fmt.Errorf("verify: open store: %w", err)
			}
			m, _, err := store.Get(ref)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}

			home, err := signingStorePath()
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			sigStore, err := signing.NewStore(home)
			if err != nil {
				return fmt.Errorf("verify: open signing store: %w", err)
			}

			imageDir := imageDirFromDigest(home, m.DiskDigest)
			sig, err := sigStore.VerifyManifest(imageDir)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			if sig == nil {
				return fmt.Errorf("verify: no signature found for %s", ref)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "verified %s (key %s, digest %s)\n", ref, sig.KeyID, sig.Digest)
			return nil
		},
	}
	return cmd
}

func signingStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni", nil
	}
	return home + "/.uni", nil
}

func imageDirFromDigest(uniHome, diskDigest string) string {
	return uniHome + "/images/" + diskDigest[len("sha256:"):]
}
