//go:build registry_e2e

package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/attest"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
)

// checkAttestations signs subject as jerboa push does and verifies it as
// jerboa pull does, against the workflow the registry recorded. Only trusted
// publishers upload attestations, so with another credential the registry
// must refuse them; with the job's OIDC token this is a real round trip
// through Fulcio, Rekor and the registry.
func checkAttestations(t *testing.T, ctx context.Context, cl *client.Client, d client.Discovery, credential, token string, ref naming.ImageReference, subject string) {
	t.Helper()
	predicates := []attest.Predicate{{Type: attest.SBOMType, Body: []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`)}}
	if credential != credentials.GitHubActions {
		_, err := cl.PutAttestation(ctx, token, ref.Namespace, ref.Repository, subject, []byte("{}"))
		require.True(t, client.IsCode(err, "DENIED"), "got %v", err)
		return
	}
	trust, err := attest.LoadTrust(ctx, t.TempDir())
	require.NoError(t, err)
	gh, err := attest.GitHubFromEnv()
	require.NoError(t, err)
	provenance, err := gh.Provenance()
	require.NoError(t, err)
	predicates = append(predicates, attest.Predicate{Type: attest.ProvenanceType, Body: provenance})
	signer := attest.Sigstore{Trust: trust, IDToken: func(ctx context.Context) (string, error) {
		return cl.ActionsIDToken(ctx, attest.SigstoreAudience)
	}}
	published, err := attest.Publish(ctx, cl, token, ref, subject, predicates, signer)
	require.NoError(t, err)
	require.Len(t, published, 2)

	results, err := attest.Check(ctx, cl, d.CDN, ref, subject, nil, trust.Root)
	require.NoError(t, err)
	require.Len(t, results, 2)
	want, err := attest.ParseIdentity(gh.Repository + "/" + workflowPath(t, gh))
	require.NoError(t, err)
	require.Equal(t, want, results[0].Identity)
	other, err := attest.ParseIdentity("AitorConS/not-a-publisher")
	require.NoError(t, err)
	_, err = attest.Check(ctx, cl, d.CDN, ref, subject, &other, trust.Root)
	require.Error(t, err)
}

// workflowPath is the path of the job's workflow in its repository.
func workflowPath(t *testing.T, gh attest.GitHub) string {
	t.Helper()
	path, _, _ := strings.Cut(gh.WorkflowRef, "@")
	require.Greater(t, len(path), len(gh.Repository)+1)
	return path[len(gh.Repository)+1:]
}
