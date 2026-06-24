package main

import (
	"fmt"
	"os"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/AitorConS/unikernel-engine/internal/signing"
	"github.com/spf13/cobra"
)

func newSignCmd(endpoint *string) *cobra.Command {
	return &cobra.Command{
		Use:   "sign <image>",
		Short: "Sign an image (by its disk digest) with the default Ed25519 key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			digest, err := imageDigest(cmd, endpoint, ref)
			if err != nil {
				return fmt.Errorf("sign: %w", err)
			}

			sigStore, err := signing.NewStore(signingStorePath())
			if err != nil {
				return fmt.Errorf("sign: open signing store: %w", err)
			}
			sig, err := sigStore.SignDigest(digest)
			if err != nil {
				return fmt.Errorf("sign: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "signed %s (key %s, digest %s)\n", ref, sig.KeyID, sig.Digest)
			return nil
		},
	}
}

func newVerifyCmd(endpoint *string) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <image>",
		Short: "Verify the signature of an image (by its disk digest)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			digest, err := imageDigest(cmd, endpoint, ref)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}

			sigStore, err := signing.NewStore(signingStorePath())
			if err != nil {
				return fmt.Errorf("verify: open signing store: %w", err)
			}
			sig, err := sigStore.VerifyDigest(digest)
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
}

// imageDigest resolves an image reference to its disk digest via the daemon.
func imageDigest(cmd *cobra.Command, endpoint *string, ref string) (string, error) {
	client, err := api.Dial(*endpoint)
	if err != nil {
		return "", fmt.Errorf("connect to daemon: %w", err)
	}
	defer func() { _ = client.Close() }()
	res, err := client.ImageGet(cmd.Context(), ref)
	if err != nil {
		return "", err
	}
	return res.DiskDigest, nil
}

func signingStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni"
	}
	return home + "/.uni"
}
