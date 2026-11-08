package attest

import (
	"encoding/json"
	"github.com/AitorConS/jerboa/internal/naming"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/require"
)

const (
	subject  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	other    = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	workflow = "https://github.com/acme/app/.github/workflows/publish.yml@refs/heads/main"
)

func statement(t *testing.T, digest string) []byte {
	t.Helper()
	data, err := NewStatement("registry.jerboa.dev/acme/app", digest, SBOMType, []byte(`{"bomFormat":"CycloneDX"}`))
	require.NoError(t, err)
	return data
}

func TestStatement(t *testing.T) {
	data := statement(t, subject)
	var st Statement
	require.NoError(t, json.Unmarshal(data, &st))
	require.Equal(t, StatementType, st.Type)
	require.Equal(t, []Subject{{Name: "registry.jerboa.dev/acme/app", Digest: map[string]string{"sha256": strings.Repeat("a", 64)}}}, st.Subject)
	require.Equal(t, SBOMType, st.PredicateType)

	_, err := NewStatement("x", "sha256:nope", SBOMType, []byte(`{}`))
	require.ErrorContains(t, err, "invalid subject digest")
	_, err = NewStatement("x", subject, SBOMType, []byte(`{`))
	require.ErrorContains(t, err, "not JSON")
}

func TestProvenance(t *testing.T) {
	for k, v := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "acme/app",
		"GITHUB_REPOSITORY_ID": "1", "GITHUB_REPOSITORY_OWNER_ID": "2", "GITHUB_SHA": "abc123", "GITHUB_REF": "refs/heads/main",
		"GITHUB_WORKFLOW_REF": "acme/app/.github/workflows/publish.yml@refs/heads/main", "GITHUB_RUN_ID": "99",
		"GITHUB_RUN_ATTEMPT": "2", "GITHUB_EVENT_NAME": "push", "RUNNER_ENVIRONMENT": "github-hosted",
	} {
		t.Setenv(k, v)
	}
	g, err := GitHubFromEnv()
	require.NoError(t, err)
	data, err := g.Provenance()
	require.NoError(t, err)
	var p struct {
		BuildDefinition struct {
			BuildType          string `json:"buildType"`
			ExternalParameters struct {
				Workflow map[string]string `json:"workflow"`
			} `json:"externalParameters"`
			ResolvedDependencies []struct {
				URI    string            `json:"uri"`
				Digest map[string]string `json:"digest"`
			} `json:"resolvedDependencies"`
		} `json:"buildDefinition"`
		RunDetails struct {
			Builder  map[string]string `json:"builder"`
			Metadata map[string]string `json:"metadata"`
		} `json:"runDetails"`
	}
	require.NoError(t, json.Unmarshal(data, &p))
	require.Equal(t, GitHubBuildType, p.BuildDefinition.BuildType)
	require.Equal(t, map[string]string{"ref": "refs/heads/main", "repository": "https://github.com/acme/app", "path": ".github/workflows/publish.yml"},
		p.BuildDefinition.ExternalParameters.Workflow)
	require.Equal(t, "git+https://github.com/acme/app@refs/heads/main", p.BuildDefinition.ResolvedDependencies[0].URI)
	require.Equal(t, "abc123", p.BuildDefinition.ResolvedDependencies[0].Digest["gitCommit"])
	require.Equal(t, workflow, p.RunDetails.Builder["id"])
	require.Equal(t, "https://github.com/acme/app/actions/runs/99/attempts/2", p.RunDetails.Metadata["invocationId"])

	t.Setenv("GITHUB_ACTIONS", "")
	_, err = GitHubFromEnv()
	require.ErrorContains(t, err, "not one")
}

func TestParseIdentity(t *testing.T) {
	for in, want := range map[string]Identity{
		"acme/app":                               {Repository: "acme/app"},
		"acme/app/.github/workflows/publish.yml": {Repository: "acme/app", Workflow: ".github/workflows/publish.yml"},
		"acme/app/.github/workflows/publish.yml@refs/heads/main": {Repository: "acme/app", Workflow: ".github/workflows/publish.yml", Ref: "refs/heads/main"},
	} {
		got, err := ParseIdentity(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got)
		require.Equal(t, in, got.String())
	}
	for _, bad := range []string{"acme", "acme/app@refs/heads/main", "acme/app/main.yml", "acme/app/.github/workflows/x.yml@main", "a b/c"} {
		_, err := ParseIdentity(bad)
		require.Error(t, err, bad)
	}
}

func TestVerify(t *testing.T) {
	// The virtual instance issues no certificate transparency entries.
	saved := verifierOptions
	verifierOptions = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	t.Cleanup(func() { verifierOptions = saved })
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	signed, err := vs.Attest(workflow, GitHubIssuer, statement(t, subject))
	require.NoError(t, err)
	stranger, err := vs.Attest("https://github.com/evil/app/.github/workflows/publish.yml@refs/heads/main", GitHubIssuer, statement(t, subject))
	require.NoError(t, err)

	for _, id := range []string{"acme/app", "acme/app/.github/workflows/publish.yml", "acme/app/.github/workflows/publish.yml@refs/heads/main"} {
		identity, err := ParseIdentity(id)
		require.NoError(t, err)
		got, err := Verify(vs, subject, identity, []verify.SignedEntity{stranger, signed})
		require.NoError(t, err, id)
		require.Len(t, got, 1)
		require.Equal(t, SBOMType, got[0].PredicateType)
		require.Equal(t, workflow, got[0].Signer)
		require.JSONEq(t, string(statement(t, subject)), string(got[0].Statement))
	}

	for _, id := range []string{"acme/application", "acme/app/.github/workflows/other.yml", "acme/app/.github/workflows/publish.yml@refs/heads/dev"} {
		identity, err := ParseIdentity(id)
		require.NoError(t, err)
		_, err = Verify(vs, subject, identity, []verify.SignedEntity{signed})
		require.ErrorContains(t, err, "is signed by "+id, id)
	}

	identity, _ := ParseIdentity("acme/app")
	_, err = Verify(vs, other, identity, []verify.SignedEntity{signed})
	require.ErrorContains(t, err, "no attestation of "+other)
	_, err = Verify(vs, subject, identity, nil)
	require.ErrorContains(t, err, "has no attestations")

	// Another issuer certifying the same workflow URL is not GitHub.
	forged, err := vs.Attest(workflow, "https://accounts.example.com", statement(t, subject))
	require.NoError(t, err)
	_, err = Verify(vs, subject, identity, []verify.SignedEntity{forged})
	require.Error(t, err)
}

func TestParseBundle(t *testing.T) {
	_, err := ParseBundle([]byte(`{}`))
	require.Error(t, err)
	_, err = ParseBundle(make([]byte, MaxBundle+1))
	require.ErrorContains(t, err, "limit")
	data, err := os.ReadFile(filepath.Join("testdata", "bundle.json"))
	require.NoError(t, err)
	b, err := ParseBundle(data)
	require.NoError(t, err)
	require.True(t, b.MinVersion("0.1"))
}

func TestPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	p, err := LoadPolicy(path)
	require.NoError(t, err)
	id, ok, err := p.For("registry.jerboa.dev", "jerboa")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, Identity{Repository: "AitorConS/jerboa-images", Workflow: ".github/workflows/publish.yml", Ref: "refs/heads/main"}, id)
	_, ok, _ = p.For("registry.jerboa.dev", "acme")
	require.False(t, ok)

	require.NoError(t, os.WriteFile(path, []byte(`{"require":[
		{"namespace":"acme","identity":"acme/infra"},
		{"registry":"localhost:8787","namespace":"dev","identity":"acme/dev"},
		{"namespace":"jerboa","identity":"evil/images"}]}`), 0o600))
	p, err = LoadPolicy(path)
	require.NoError(t, err)
	id, ok, _ = p.For("registry.jerboa.dev", "acme")
	require.True(t, ok)
	require.Equal(t, "acme/infra", id.String())
	_, ok, _ = p.For("registry.jerboa.dev", "dev")
	require.False(t, ok)
	id, _, _ = p.For("localhost:8787", "dev")
	require.Equal(t, "acme/dev", id.String())
	// The official rule cannot be replaced.
	id, _, _ = p.For("registry.jerboa.dev", "jerboa")
	require.Equal(t, Official.Identity, id.String())

	require.NoError(t, os.WriteFile(path, []byte(`{"require":[{"namespace":"acme","identity":"nope"}]}`), 0o600))
	_, err = LoadPolicy(path)
	require.ErrorContains(t, err, "identity")
	require.NoError(t, os.WriteFile(path, []byte(`{"rules":[]}`), 0o600))
	_, err = LoadPolicy(path)
	require.ErrorContains(t, err, "unknown field")
}

func TestVerifyListed(t *testing.T) {
	saved := verifierOptions
	verifierOptions = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	t.Cleanup(func() { verifierOptions = saved })
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	signed, err := vs.Attest(workflow, GitHubIssuer, statement(t, subject))
	require.NoError(t, err)
	const recorded = "acme/app/.github/workflows/publish.yml"

	// Each against the workflow the registry recorded for it.
	got, err := VerifyListed(vs, subject, nil, []Listed{{Identity: recorded, Entity: signed}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, recorded, got[0].Identity.String())

	// A certificate for another workflow than the one that uploaded it fails,
	// as does a record that names no workflow.
	for _, r := range []string{"acme/app/.github/workflows/other.yml", "evil/app/.github/workflows/publish.yml", "acme/app", "", "nonsense"} {
		_, err = VerifyListed(vs, subject, nil, []Listed{{Identity: r, Entity: signed}})
		require.ErrorContains(t, err, "signed by the workflow that uploaded it", r)
	}
	// One that verifies among others is enough.
	got, err = VerifyListed(vs, subject, nil, []Listed{{Identity: "evil/app/.github/workflows/x.yml", Entity: signed}, {Identity: recorded, Entity: signed}})
	require.NoError(t, err)
	require.Len(t, got, 1)

	// A wanted identity overrides the records.
	want, _ := ParseIdentity("acme/app/.github/workflows/publish.yml@refs/heads/main")
	got, err = VerifyListed(vs, subject, &want, []Listed{{Identity: "evil/app/.github/workflows/x.yml", Entity: signed}})
	require.NoError(t, err)
	require.Equal(t, want, got[0].Identity)
	want, _ = ParseIdentity("acme/other")
	_, err = VerifyListed(vs, subject, &want, []Listed{{Identity: recorded, Entity: signed}})
	require.Error(t, err)

	_, err = VerifyListed(vs, subject, nil, nil)
	require.ErrorIs(t, err, ErrUnsigned)
	_, err = VerifyListed(vs, subject, &want, nil)
	require.ErrorIs(t, err, ErrUnsigned)
}

func TestSigners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jerboa", "signers.json")
	s, err := LoadSigners(path)
	require.NoError(t, err)
	ref := naming.ImageReference{Registry: "registry.jerboa.dev", Namespace: "acme", Repository: "web", Tag: "v1"}
	_, ok := s.Get(ref)
	require.False(t, ok)

	id, _ := ParseIdentity("acme/images/.github/workflows/publish.yml@refs/tags/v1")
	require.NoError(t, s.Remember(ref, id))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	s, err = LoadSigners(path)
	require.NoError(t, err)
	got, ok := s.Get(naming.ImageReference{Registry: "registry.jerboa.dev", Namespace: "acme", Repository: "web", Digest: subject})
	require.True(t, ok)
	require.Equal(t, "acme/images/.github/workflows/publish.yml", got.String(), "the ref is not remembered")
	require.Equal(t, [][2]string{{"registry.jerboa.dev/acme/web", "acme/images/.github/workflows/publish.yml"}}, s.List())

	ok, err = s.Forget("registry.jerboa.dev/acme/other")
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = s.Forget("registry.jerboa.dev/acme/web")
	require.NoError(t, err)
	require.True(t, ok)
	s, err = LoadSigners(path)
	require.NoError(t, err)
	require.Empty(t, s.List())

	require.NoError(t, os.WriteFile(path, []byte(`{"signers":{"x/a/b":"not an identity"}}`), 0o600))
	_, err = LoadSigners(path)
	require.Error(t, err)
}
