package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/attest"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/AitorConS/jerboa/internal/registry/imageindex"
	"github.com/spf13/cobra"
)

// Images trusted publishers push from GitHub Actions are signed with
// Sigstore (attestations of their provenance and SBOM). Pulls verify every
// image that has attestations: against the policy's rule for its namespace,
// else the signer remembered for its repository, else the workflow the
// registry recorded as their uploader, which is then remembered. See
// internal/registry/attest.

func sigstoreCacheDir() string { return filepath.Join(signingStorePath(), "sigstore") }

func policyPath() string { return filepath.Join(signingStorePath(), "policy.json") }

func signersPath() string { return filepath.Join(signingStorePath(), "signers.json") }

// predicateNames are the short names of the predicates jerboa signs.
var predicateNames = map[string]string{attest.ProvenanceType: "provenance", attest.SBOMType: "sbom"}

func predicateName(t string) string {
	if n, ok := predicateNames[t]; ok {
		return n
	}
	return t
}

// shouldSign decides whether a push signs: --sign when given, otherwise
// whenever a trusted publisher pushes (cred is the GitHub Actions OIDC
// token) to a registry that stores attestations. Registries take
// attestations from trusted publishers only: the workflow their OIDC token
// names is the identity pulls expect the signatures to have.
func shouldSign(cmd *cobra.Command, sign bool, d client.Discovery, host, cred string) (bool, error) {
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
	if cred != credentials.GitHubActions {
		const why = "only trusted publishers sign: push with the job's OIDC token (no " + credentials.EnvToken + ") from a workflow the namespace trusts"
		if explicit {
			return false, errors.New(why)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "note: pushing without signing; %s\n", why)
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

// verification is how a pull or verify checks an image's signatures.
type verification struct {
	// id is who must have signed; nil means the workflow the registry
	// recorded for each attestation.
	id *attest.Identity
	// source says where id comes from, for messages.
	source string
	// required fails an image without signatures.
	required bool
	// remember records the signer once it verifies.
	remember bool
}

// verificationFor returns how ref is checked: against --identity, the
// policy's rule for its namespace (official images always have one), or the
// signer remembered for its repository, all required; otherwise against the
// registry's record, required only with verify, and remembered.
func verificationFor(ref naming.ImageReference, identity string, verify bool) (verification, error) {
	if identity != "" {
		id, err := attest.ParseIdentity(identity)
		if err != nil {
			return verification{}, err
		}
		return verification{id: &id, source: "--identity", required: true}, nil
	}
	policy, err := attest.LoadPolicy(policyPath())
	if err != nil {
		return verification{}, err
	}
	id, ok, err := policy.For(ref.Registry, ref.Namespace)
	if err != nil {
		return verification{}, err
	}
	if ok {
		return verification{id: &id, source: "the policy", required: true}, nil
	}
	signers, err := attest.LoadSigners(signersPath())
	if err != nil {
		return verification{}, err
	}
	if id, ok := signers.Get(ref); ok {
		return verification{id: &id, source: "remembered", required: true}, nil
	}
	return verification{required: verify, remember: true}, nil
}

// verifySubject checks the signatures of subject, a recipe or index of the
// repository ref names, as v says. It returns nil results for an unsigned
// image v does not require signed.
func verifySubject(ctx context.Context, out io.Writer, ref naming.ImageReference, subject string, v verification) ([]attest.Result, error) {
	cl := client.New(ref.Registry)
	d, err := cl.Discover(ctx)
	if err != nil {
		return nil, err
	}
	if !d.Supports(client.FeatureAttestations) {
		if v.required {
			return nil, fmt.Errorf("%s does not store attestations, so %s cannot be verified", ref.Registry, ref)
		}
		return nil, nil
	}
	repo := ref.Registry + "/" + ref.Namespace + "/" + ref.Repository
	// Unsigned and not required: nothing to verify, so no trusted root needed.
	if !v.required {
		list, err := cl.Attestations(ctx, d.CDN, ref.Namespace, ref.Repository, subject)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, nil
		}
	}
	trust, err := attest.LoadTrust(ctx, sigstoreCacheDir())
	if err != nil {
		return nil, err
	}
	results, err := attest.Check(ctx, cl, d.CDN, ref, subject, v.id, trust.Root)
	switch {
	case err == nil:
	case v.source == "remembered":
		return nil, fmt.Errorf("%s was signed by %s before, and %s is not: %w\nIf its publisher changed on purpose, run: jerboa registry signers forget %s", repo, v.id, ref, err, repo)
	case v.id != nil:
		return nil, fmt.Errorf("%s is not signed by %s (%s): %w", ref, v.id, v.source, err)
	default:
		return nil, fmt.Errorf("%s fails its signature check: %w", ref, err)
	}
	if v.remember {
		signer := results[len(results)-1].Identity
		signers, err := attest.LoadSigners(signersPath())
		if err != nil {
			return nil, err
		}
		if err := signers.Remember(ref, signer); err != nil {
			return nil, err
		}
		signer.Ref = ""
		fmt.Fprintf(out, "Remembering %s as the signer of %s: later pulls require it\n", signer, repo)
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
		Short: "Check an image's Sigstore signatures, as a pull does",
		Long: `Check an image's Sigstore signatures, as a pull does, without pulling it.

The signer it must have is --identity, else the rule of the policy file
(~/.jerboa/policy.json) for the image's namespace, else the signer a pull
remembered for the repository (~/.jerboa/signers.json), else the workflow the
registry recorded as the uploader of each signature: a trusted publisher of
the namespace. An identity is a GitHub repository, optionally narrowed to one
workflow and one ref:

  owner/repo
  owner/repo/.github/workflows/publish.yml
  owner/repo/.github/workflows/publish.yml@refs/heads/main

A tag published for several platforms is checked for this host's platform,
or the one --platform names.`,
		Example: `  jerboa registry verify acme/web:v1
  jerboa registry verify acme/web:v1 --identity acme/web/.github/workflows/release.yml@refs/heads/main`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			v, err := verificationFor(ref, identity, true)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			// Checking remembers nothing: only pulls do.
			v.remember = false
			subject, err := resolveSubject(cmd.Context(), ref, platform)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			results, err := verifySubject(cmd.Context(), cmd.OutOrStdout(), ref, subject, v)
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

func newRegistrySignersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "signers",
		Short: "List the signers pulls remember for repositories",
		Long: `List the signers pulls remember for repositories.

The first pull of a signed image remembers the workflow that signed it, and
later pulls of the repository require that workflow, so a change of
publisher, or an unsigned image, is refused (~/.jerboa/signers.json).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			signers, err := attest.LoadSigners(signersPath())
			if err != nil {
				return err
			}
			list := signers.List()
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No signers remembered yet.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "REPOSITORY\tSIGNER")
			for _, e := range list {
				fmt.Fprintf(w, "%s\t%s\n", e[0], e[1])
			}
			return w.Flush()
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "forget <[registry/]namespace/repository>",
		Short: "Forget a repository's signer, so the next pull trusts whoever signs it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := naming.ParseImageReference(args[0])
			if err != nil {
				return fmt.Errorf("signers forget: %w", err)
			}
			signers, err := attest.LoadSigners(signersPath())
			if err != nil {
				return err
			}
			key := attest.SignerKey(ref)
			ok, err := signers.Forget(key)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("signers forget: no signer remembered for %s", key)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forgot the signer of %s\n", key)
			return nil
		},
	})
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
