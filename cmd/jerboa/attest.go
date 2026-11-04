package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/attest"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/AitorConS/jerboa/internal/registry/imageindex"
	"github.com/spf13/cobra"
)

// Images pushed from GitHub Actions are signed with Sigstore (attestations
// of their provenance and SBOM); pulls verify the images a policy rule, or
// --identity, names a signer for. See internal/registry/attest.

func sigstoreCacheDir() string { return filepath.Join(signingStorePath(), "sigstore") }

func policyPath() string { return filepath.Join(signingStorePath(), "policy.json") }

// predicateNames are the short names of the predicates jerboa signs.
var predicateNames = map[string]string{attest.ProvenanceType: "provenance", attest.SBOMType: "sbom"}

func predicateName(t string) string {
	if n, ok := predicateNames[t]; ok {
		return n
	}
	return t
}

// shouldSign decides whether a push signs: --sign when given, otherwise
// whenever the push runs in a GitHub Actions job that may request OIDC
// tokens and the registry stores attestations.
func shouldSign(cmd *cobra.Command, sign bool, d client.Discovery, host string) (bool, error) {
	explicit := cmd.Flags().Changed("sign")
	if explicit && !sign {
		return false, nil
	}
	if !credentials.InGitHubActions() {
		if explicit {
			return false, errors.New("--sign signs with the OIDC token of a GitHub Actions job: run it in a job with permissions: id-token: write")
		}
		return false, nil
	}
	if !d.Supports(client.FeatureAttestations) {
		if explicit {
			return false, fmt.Errorf("%s does not store attestations, so it cannot keep signatures", host)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s does not store attestations; pushing without signing\n", host)
		return false, nil
	}
	return true, nil
}

// signPushed signs the provenance of subject, and sbom when there is one,
// and uploads both attestations.
func signPushed(ctx context.Context, out io.Writer, cl *client.Client, token string, ref naming.ImageReference, subject string, sbom []byte) error {
	gh, err := attest.GitHubFromEnv()
	if err != nil {
		return err
	}
	provenance, err := gh.Provenance()
	if err != nil {
		return err
	}
	predicates := []attest.Predicate{{Type: attest.ProvenanceType, Body: provenance}}
	if len(sbom) > 0 {
		predicates = append(predicates, attest.Predicate{Type: attest.SBOMType, Body: sbom})
	}
	trust, err := attest.LoadTrust(ctx, sigstoreCacheDir())
	if err != nil {
		return err
	}
	signer := attest.Sigstore{Trust: trust, IDToken: func(ctx context.Context) (string, error) {
		return cl.ActionsIDToken(ctx, attest.SigstoreAudience)
	}}
	published, err := attest.Publish(ctx, cl, token, ref, subject, predicates, signer)
	if err != nil {
		return err
	}
	names := make([]string, len(published))
	for i, p := range published {
		names[i] = predicateName(p.PredicateType)
	}
	fmt.Fprintf(out, "Signed %s: %s, as %s\n", subject, strings.Join(names, " and "), gh.ServerURL+"/"+gh.WorkflowRef)
	return nil
}

// requiredIdentity returns who must have signed ref: --identity, or the
// policy's rule for its namespace. verify asks for a check even without one,
// which is then an error.
func requiredIdentity(ref naming.ImageReference, identity string, verify bool) (attest.Identity, bool, error) {
	if identity != "" {
		id, err := attest.ParseIdentity(identity)
		return id, err == nil, err
	}
	policy, err := attest.LoadPolicy(policyPath())
	if err != nil {
		return attest.Identity{}, false, err
	}
	id, ok, err := policy.For(ref.Registry, ref.Namespace)
	if err != nil {
		return attest.Identity{}, false, err
	}
	if !ok && verify {
		return attest.Identity{}, false, fmt.Errorf("no identity to verify %s/%s against: pass --identity, or add a rule to %s", ref.Registry, ref.Namespace, policyPath())
	}
	return id, ok, nil
}

// verifySubject checks that subject, a recipe or index of the repository ref
// names, has an attestation signed by id.
func verifySubject(ctx context.Context, ref naming.ImageReference, subject string, id attest.Identity) ([]attest.Result, error) {
	cl := client.New(ref.Registry)
	d, err := cl.Discover(ctx)
	if err != nil {
		return nil, err
	}
	if !d.Supports(client.FeatureAttestations) {
		return nil, fmt.Errorf("%s does not store attestations, so %s cannot be verified", ref.Registry, ref)
	}
	trust, err := attest.LoadTrust(ctx, sigstoreCacheDir())
	if err != nil {
		return nil, err
	}
	results, err := attest.Check(ctx, cl, d.CDN, ref, subject, id, trust.Root)
	if err != nil {
		return nil, fmt.Errorf("%s is not signed by %s: %w", ref, id, err)
	}
	return results, nil
}

// printVerified describes the attestations that verified.
func printVerified(out io.Writer, subject string, results []attest.Result) {
	fmt.Fprintf(out, "Verified %s\n", subject)
	for _, r := range results {
		fmt.Fprintf(out, "  %-10s signed by %s\n", predicateName(r.PredicateType), r.Signer)
		if r.Commit != "" || r.Run != "" {
			fmt.Fprintf(out, "  %-10s commit %s, run %s\n", "", r.Commit, r.Run)
		}
	}
}

func newRegistryVerifyCmd() *cobra.Command {
	var identity, platform string
	cmd := &cobra.Command{
		Use:   "verify <[registry/]namespace/repository[:tag|@digest]>",
		Short: "Check that an image in a registry is signed by the identity it should be",
		Long: `Check that an image in a registry is signed by the identity it should be.

The identity is --identity, or the rule of the policy file
(~/.jerboa/policy.json) for the image's namespace. It is a GitHub
repository, optionally narrowed to one workflow and one ref:

  owner/repo
  owner/repo/.github/workflows/publish.yml
  owner/repo/.github/workflows/publish.yml@refs/heads/main

A tag published for several platforms is checked for this host's platform,
or the one --platform names.`,
		Example: `  jerboa registry verify acme/web:v1 --identity acme/web/.github/workflows/release.yml@refs/heads/main
  jerboa registry verify jerboa/postgresql:16`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			id, _, err := requiredIdentity(ref, identity, true)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			subject, err := resolveSubject(cmd.Context(), ref, platform)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			results, err := verifySubject(cmd.Context(), ref, subject, id)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			printVerified(cmd.OutOrStdout(), subject, results)
			return nil
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "the GitHub repository, workflow and ref the image must be signed from")
	cmd.Flags().StringVar(&platform, "platform", "", "the platform to check, for a tag published for several (default: this host's)")
	return cmd
}

// resolveSubject returns the recipe ref names: its digest, or what its tag
// names, choosing the platform's recipe from an index.
func resolveSubject(ctx context.Context, ref naming.ImageReference, platform string) (string, error) {
	if ref.Digest != "" {
		return ref.Digest, nil
	}
	cl := client.New(ref.Registry)
	d, err := cl.Discover(ctx)
	if err != nil {
		return "", err
	}
	tag, err := cl.Tag(ctx, d.CDN, ref.Namespace, ref.Repository, ref.Tag)
	if err != nil {
		return "", err
	}
	if tag.Index == "" {
		return tag.Recipe, nil
	}
	data, err := cl.Index(ctx, d.CDN, tag.Index)
	if err != nil {
		return "", err
	}
	ix, err := imageindex.Parse(data)
	if err != nil {
		return "", err
	}
	recipe, _, err := ix.Choose(platform)
	return recipe, err
}
