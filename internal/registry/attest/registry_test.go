package attest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/registrytest"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/stretchr/testify/require"
)

// fakeSigner wraps statements in a bundle-shaped document without signing.
type fakeSigner struct{ signed [][]byte }

func (f *fakeSigner) Sign(_ context.Context, statement []byte) ([]byte, error) {
	f.signed = append(f.signed, statement)
	return json.Marshal(map[string]any{"mediaType": BundleMediaType, "dsseEnvelope": map[string]any{"payload": statement, "payloadType": PayloadType}})
}

func TestPublishAndCheck(t *testing.T) {
	reg := registrytest.New()
	defer reg.Close()
	ctx := context.Background()
	cl := client.New(reg.Host())
	d, err := cl.Discover(ctx)
	require.NoError(t, err)
	require.True(t, d.Supports(client.FeatureAttestations))
	ref := naming.ImageReference{Registry: reg.Host(), Namespace: "acme", Repository: "app", Tag: "v1"}

	signer := &fakeSigner{}
	predicates := []Predicate{{Type: ProvenanceType, Body: []byte(`{"buildDefinition":{}}`)}, {Type: SBOMType, Body: []byte(`{"bomFormat":"CycloneDX"}`)}}
	// The subject must be an image of the repository.
	_, err = Publish(ctx, cl, "token", ref, subject, predicates, signer)
	require.ErrorContains(t, err, "SUBJECT_UNKNOWN")

	reg.SetObject("refs/acme/app/"+strings.TrimPrefix(subject, "sha256:"), []byte("{}"))
	published, err := Publish(ctx, cl, "token", ref, subject, predicates, signer)
	require.NoError(t, err)
	require.Len(t, published, 2)
	require.Equal(t, ProvenanceType, published[0].PredicateType)
	var st Statement
	require.NoError(t, json.Unmarshal(signer.signed[1], &st))
	require.Equal(t, reg.Host()+"/acme/app", st.Subject[0].Name)

	list, err := cl.Attestations(ctx, d.CDN, "acme", "app", subject)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, published[1].Digest, list[1].Digest)
	require.Equal(t, SBOMType, list[1].PredicateType)
	require.Equal(t, registrytest.DefaultPublisher, list[1].Identity)
	data, err := cl.Attestation(ctx, d.CDN, "acme", "app", subject, list[0].Digest)
	require.NoError(t, err)
	require.Contains(t, string(data), "dsseEnvelope")
	// Uploading the same attestation again stores it once.
	_, err = Publish(ctx, cl, "token", ref, subject, predicates[:1], signer)
	require.NoError(t, err)
	list, err = cl.Attestations(ctx, d.CDN, "acme", "app", subject)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// A tampered attestation does not match its digest.
	reg.Corrupt("attestations/acme/app/"+strings.TrimPrefix(subject, "sha256:")+"/"+strings.TrimPrefix(list[0].Digest, "sha256:")+".json", 10)
	_, err = cl.Attestation(ctx, d.CDN, "acme", "app", subject, list[0].Digest)
	require.ErrorContains(t, err, "does not match its digest")

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	id, _ := ParseIdentity("acme/app")
	// Unsigned bundles vouch for nothing.
	_, err = Check(ctx, cl, d.CDN, ref, other, &id, vs)
	require.ErrorContains(t, err, "has no attestations")
	reg.SetObject("refs/acme/app/"+strings.TrimPrefix(other, "sha256:"), []byte("{}"))
	_, err = Publish(ctx, cl, "token", ref, other, predicates[1:], signer)
	require.NoError(t, err)
	_, err = Check(ctx, cl, d.CDN, ref, other, &id, vs)
	require.ErrorContains(t, err, "has no attestations")
	_, err = Check(ctx, cl, d.CDN, ref, other, nil, vs)
	require.ErrorContains(t, err, "has no attestations")
	_, err = Check(ctx, cl, d.CDN, naming.ImageReference{Registry: reg.Host(), Namespace: "acme", Repository: "none"}, other, nil, vs)
	require.ErrorIs(t, err, ErrUnsigned)

	// Without a trusted publisher, the registry takes no attestations.
	reg.Publisher = ""
	_, err = Publish(ctx, cl, "token", ref, other, predicates[:1], signer)
	require.ErrorContains(t, err, "DENIED")
}
