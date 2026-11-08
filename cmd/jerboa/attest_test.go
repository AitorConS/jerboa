package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/attest"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func signCmd(t *testing.T, args ...string) (*cobra.Command, *bool, *bytes.Buffer) {
	t.Helper()
	var sign bool
	cmd := &cobra.Command{}
	cmd.Flags().BoolVar(&sign, "sign", false, "")
	require.NoError(t, cmd.ParseFlags(args))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	return cmd, &sign, &stderr
}

func TestShouldSign(t *testing.T) {
	withAttestations := client.Discovery{Features: []string{client.FeatureAttestations}}
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")

	// Outside GitHub Actions: unsigned, unless asked, which fails.
	cmd, sign, _ := signCmd(t)
	ok, err := shouldSign(cmd, *sign, withAttestations, "r", credentials.GitHubActions)
	require.NoError(t, err)
	require.False(t, ok)
	cmd, sign, _ = signCmd(t, "--sign")
	_, err = shouldSign(cmd, *sign, withAttestations, "r", credentials.GitHubActions)
	require.ErrorContains(t, err, "id-token: write")

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "x")
	cmd, sign, _ = signCmd(t)
	ok, err = shouldSign(cmd, *sign, withAttestations, "r", credentials.GitHubActions)
	require.NoError(t, err)
	require.True(t, ok)
	cmd, sign, _ = signCmd(t, "--sign=false")
	ok, err = shouldSign(cmd, *sign, withAttestations, "r", credentials.GitHubActions)
	require.NoError(t, err)
	require.False(t, ok)

	// Another credential than the job's OIDC token is no trusted publisher.
	cmd, sign, stderr := signCmd(t)
	ok, err = shouldSign(cmd, *sign, withAttestations, "r", "jpat_x")
	require.NoError(t, err)
	require.False(t, ok)
	require.Contains(t, stderr.String(), "only trusted publishers sign")
	cmd, sign, _ = signCmd(t, "--sign")
	_, err = shouldSign(cmd, *sign, withAttestations, "r", "jpat_x")
	require.ErrorContains(t, err, "only trusted publishers sign")

	// A registry without attestations: a note by default, an error when asked.
	cmd, sign, stderr = signCmd(t)
	ok, err = shouldSign(cmd, *sign, client.Discovery{}, "old.example", credentials.GitHubActions)
	require.NoError(t, err)
	require.False(t, ok)
	require.Contains(t, stderr.String(), "old.example does not store attestations")
	cmd, sign, _ = signCmd(t, "--sign")
	_, err = shouldSign(cmd, *sign, client.Discovery{}, "old.example", credentials.GitHubActions)
	require.ErrorContains(t, err, "cannot keep signatures")
}

func TestVerificationFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ref := func(s string) naming.ImageReference {
		r, err := naming.ParseImageReference(s)
		require.NoError(t, err)
		return r
	}

	v, err := verificationFor(ref("jerboa/postgresql:16"), "", false)
	require.NoError(t, err)
	require.True(t, v.required, "official images are always checked")
	require.Equal(t, "AitorConS/jerboa-images", v.id.Repository)
	require.False(t, v.remember)

	// Nobody named: the registry's record, required only with --verify, then remembered.
	v, err = verificationFor(ref("acme/web:v1"), "", false)
	require.NoError(t, err)
	require.Equal(t, verification{remember: true}, v)
	v, err = verificationFor(ref("acme/web:v1"), "", true)
	require.NoError(t, err)
	require.Equal(t, verification{required: true, remember: true}, v)

	v, err = verificationFor(ref("acme/web:v1"), "acme/web", false)
	require.NoError(t, err)
	require.True(t, v.required)
	require.Equal(t, "acme/web", v.id.String())
	_, err = verificationFor(ref("acme/web:v1"), "acme", false)
	require.Error(t, err)

	// A remembered signer is required.
	signers, err := attest.LoadSigners(signersPath())
	require.NoError(t, err)
	id, err := attest.ParseIdentity("acme/images/.github/workflows/publish.yml@refs/heads/main")
	require.NoError(t, err)
	require.NoError(t, signers.Remember(ref("acme/web:v1"), id))
	v, err = verificationFor(ref("acme/web@sha256:"+strings.Repeat("ab", 32)), "", false)
	require.NoError(t, err)
	require.Equal(t, "remembered", v.source)
	require.True(t, v.required)
	require.Equal(t, "acme/images/.github/workflows/publish.yml", v.id.String())
	v, err = verificationFor(ref("acme/other:v1"), "", false)
	require.NoError(t, err)
	require.Nil(t, v.id)

	// The policy comes before what is remembered.
	require.NoError(t, os.WriteFile(filepath.Join(home, ".jerboa", "policy.json"),
		[]byte(`{"require":[{"namespace":"acme","identity":"acme/infra/.github/workflows/publish.yml@refs/heads/main"}]}`), 0o600))
	v, err = verificationFor(ref("acme/web:v1"), "", false)
	require.NoError(t, err)
	require.Equal(t, "the policy", v.source)
	require.Equal(t, "acme/infra/.github/workflows/publish.yml@refs/heads/main", v.id.String())
}

func TestUnsigned(t *testing.T) {
	const wf = "acme/images/.github/workflows/publish.yml"
	provenance := attest.Predicate{Type: attest.ProvenanceType}
	sbom := attest.Predicate{Type: attest.SBOMType}
	both := []attest.Predicate{provenance, sbom}

	require.Equal(t, both, unsigned(both, nil, wf))
	require.Equal(t, []attest.Predicate{sbom}, unsigned(both, []client.AttestationRef{{PredicateType: attest.ProvenanceType, Identity: wf}}, wf))
	require.Empty(t, unsigned(both, []client.AttestationRef{
		{PredicateType: attest.SBOMType, Identity: wf},
		{PredicateType: attest.ProvenanceType, Identity: wf},
	}, wf))
	// Another workflow's attestation does not count.
	require.Equal(t, both, unsigned(both, []client.AttestationRef{{PredicateType: attest.ProvenanceType, Identity: "acme/other/.github/workflows/x.yml"}}, wf))
}
