package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/AitorConS/unikernel-engine/internal/signing"
	"github.com/spf13/cobra"
)

type registryClientConfig struct {
	token    *string
	caCert   *string
	insecure *bool
}

func newRegistryClient(registryURL string, cfg *registryClientConfig) (*registry.Client, error) {
	client := registry.NewClient(registryURL)
	if cfg == nil {
		return client, nil
	}
	if cfg.token != nil {
		client.SetToken(*cfg.token)
	}
	if cfg.caCert != nil {
		if err := client.SetCACertFile(*cfg.caCert); err != nil {
			return nil, fmt.Errorf("configure registry CA cert: %w", err)
		}
	}
	if cfg.insecure != nil {
		client.SetInsecureSkipVerify(*cfg.insecure)
	}
	return client, nil
}

func newPushCmd(storePath *string, regCfg *registryClientConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "push <ref> <registry>",
		Short: "Push a local image to a registry (e.g. push hello:latest http://localhost:5000)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, registryURL := args[0], args[1]
			store, err := image.NewStore(*storePath)
			if err != nil {
				return fmt.Errorf("push: open store: %w", err)
			}
			m, diskPath, err := store.Get(ref)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			client, err := newRegistryClient(registryURL, regCfg)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}
			if err := client.PushOCI(cmd.Context(), m, diskPath); err != nil {
				if errLegacy := client.Push(cmd.Context(), m, diskPath); errLegacy != nil {
					return fmt.Errorf("push: OCI error: %v; legacy error: %w", err, errLegacy)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pushed %s to %s\n", ref, registryURL)
			return nil
		},
	}
}

func newPullCmd(storePath *string, regCfg *registryClientConfig) *cobra.Command {
	var verify string
	cmd := &cobra.Command{
		Use:   "pull <ref> <registry>",
		Short: "Pull an image from a registry (e.g. pull hello:latest http://localhost:5000)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, registryURL := args[0], args[1]
			store, err := image.NewStore(*storePath)
			if err != nil {
				return fmt.Errorf("pull: open store: %w", err)
			}
			client, err := newRegistryClient(registryURL, regCfg)
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			m, err := client.PullOCI(cmd.Context(), ref, store)
			if err != nil {
				mLegacy, errLegacy := client.Pull(cmd.Context(), ref, store)
				if errLegacy != nil {
					return fmt.Errorf("pull: OCI error: %v; legacy error: %w", err, errLegacy)
				}
				m = mLegacy
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s:%s\n", m.DiskDigest, m.Name, m.Tag)

			if err := verifyPulledImage(cmd, m, *storePath, verify); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&verify, "verify", "off", "image signature verification: off, warn, enforce")
	return cmd
}

func verifyPulledImage(cmd *cobra.Command, m image.Manifest, storePath, verifyFlag string) error {
	policy, err := signing.ParseVerifyPolicy(verifyFlag)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	if policy == signing.VerifyOff {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = ".uni"
	} else {
		home += "/.uni"
	}
	sigStore, err := signing.NewStore(home)
	if err != nil {
		if policy == signing.VerifyEnforce {
			return fmt.Errorf("pull: verify: open signing store: %w", err)
		}
		log.Printf("warning: verify: open signing store: %v", err)
		return nil
	}

	imageDir := home + "/images/" + strings.TrimPrefix(m.DiskDigest, "sha256:")
	sig, verifyErr := sigStore.VerifyManifest(imageDir)
	if policy == signing.VerifyWarn {
		if verifyErr != nil {
			log.Printf("warning: verify: %v", verifyErr)
		} else if sig == nil {
			log.Printf("warning: no signature found for %s:%s", m.Name, m.Tag)
		}
		return nil
	}
	if verifyErr != nil {
		return fmt.Errorf("pull: verify: %w", verifyErr)
	}
	if sig == nil {
		return fmt.Errorf("pull: verify: no signature found for %s:%s", m.Name, m.Tag)
	}
	return nil
}
