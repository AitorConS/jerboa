//go:build sigstore_e2e

package attest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/require"
)

// TestSigstoreRoundTrip signs a statement with public-good Sigstore, as a
// push from this GitHub Actions job would, and verifies the bundle against
// the job's workflow. It needs a job with permissions: id-token: write, and
// records an entry in the public Rekor log.
func TestSigstoreRoundTrip(t *testing.T) {
	if !credentials.InGitHubActions() {
		t.Skip("needs a GitHub Actions job that may request OIDC tokens")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	trust, err := LoadTrust(ctx, t.TempDir())
	require.NoError(t, err)

	gh, err := GitHubFromEnv()
	require.NoError(t, err)
	provenance, err := gh.Provenance()
	require.NoError(t, err)
	digest := "sha256:" + strings.Repeat("0", 63) + "1"
	statement, err := NewStatement("registry.jerboa.dev/jerboa-e2e/sigstore", digest, ProvenanceType, provenance)
	require.NoError(t, err)

	cl := client.New("registry.jerboa.dev")
	signer := Sigstore{Trust: trust, IDToken: func(ctx context.Context) (string, error) {
		return cl.ActionsIDToken(ctx, SigstoreAudience)
	}}
	data, err := signer.Sign(ctx, statement)
	require.NoError(t, err)
	b, err := ParseBundle(data)
	require.NoError(t, err)

	// The job's workflow file and ref, as the certificate names them.
	workflowRef := os.Getenv("GITHUB_WORKFLOW_REF")
	path, ref, _ := strings.Cut(workflowRef, "@")
	id, err := ParseIdentity(path + "@" + ref)
	if err != nil {
		// Pull request refs (refs/pull/N/merge) are not identities; check
		// the workflow on any ref.
		id, err = ParseIdentity(path)
	}
	require.NoError(t, err)
	results, err := Verify(trust.Root, digest, id, []verify.SignedEntity{b})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, ProvenanceType, results[0].PredicateType)
	require.Equal(t, gh.ServerURL+"/"+workflowRef, results[0].Signer)
	require.Equal(t, os.Getenv("GITHUB_SHA"), results[0].Commit)
	t.Logf("verified a bundle signed by %s", results[0].Signer)

	other, err := ParseIdentity("AitorConS/somewhere-else")
	require.NoError(t, err)
	_, err = Verify(trust.Root, digest, other, []verify.SignedEntity{b})
	require.Error(t, err)
	_, err = Verify(trust.Root, "sha256:"+strings.Repeat("0", 64), id, []verify.SignedEntity{b})
	require.Error(t, err)
}
