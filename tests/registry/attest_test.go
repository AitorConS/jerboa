//go:build registry_e2e

package registry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/attest"
	"github.com/AitorConS/jerboa/internal/registry/client"
)

// selfSigned issues certificates from a throwaway CA in place of Fulcio, so
// the bundles have Sigstore's form without its services.
type selfSigned struct{}

func (selfSigned) GetCertificate(_ context.Context, kp sign.Keypair, _ *sign.CertificateProviderOptions) ([]byte, error) {
	ca, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	san, _ := url.Parse("https://github.com/acme/web/.github/workflows/publish.yml@refs/heads/main")
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "jerboa e2e"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(10 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{san},
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, kp.GetPublicKey(), ca)
}

// selfSigner signs as jerboa push does, with a certificate no Sigstore root trusts.
type selfSigner struct{}

func (selfSigner) Sign(ctx context.Context, statement []byte) ([]byte, error) {
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, err
	}
	b, err := sign.Bundle(&sign.DSSEData{Data: statement, PayloadType: attest.PayloadType}, kp, sign.BundleOptions{
		Context: ctx, CertificateProvider: selfSigned{},
	})
	if err != nil {
		return nil, err
	}
	return protojson.Marshal(b)
}

// checkAttestations uploads a provenance and an SBOM attestation of subject
// and reads them back through the CDN layout: the registry takes the bundles
// jerboa push makes, and the client parses what the registry serves. Their
// signatures do not verify, as no Sigstore root trusts their certificate.
func checkAttestations(t *testing.T, ctx context.Context, cl *client.Client, d client.Discovery, token string, ref naming.ImageReference, subject string) {
	t.Helper()
	predicates := []attest.Predicate{
		{Type: attest.ProvenanceType, Body: []byte(`{"buildDefinition":{}}`)},
		{Type: attest.SBOMType, Body: []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`)},
	}
	published, err := attest.Publish(ctx, cl, token, ref, subject, predicates, selfSigner{})
	require.NoError(t, err)
	require.Len(t, published, 2)

	refs, err := cl.Attestations(ctx, d.CDN, ref.Namespace, ref.Repository, subject)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	for i, r := range refs {
		require.Equal(t, published[i].Digest, r.Digest)
		require.Equal(t, predicates[i].Type, r.PredicateType)
		data, err := cl.Attestation(ctx, d.CDN, ref.Namespace, ref.Repository, subject, r.Digest)
		require.NoError(t, err)
		_, err = attest.ParseBundle(data)
		require.NoError(t, err)
	}

	// Uploading one again stores nothing new.
	_, err = attest.Publish(ctx, cl, token, ref, subject, predicates[:1], selfSigner{})
	require.NoError(t, err)

	// Another digest is no subject of the repository.
	_, err = cl.PutAttestation(ctx, token, ref.Namespace, ref.Repository, "sha256:"+strings.Repeat("ab", 32), []byte("{}"))
	require.True(t, client.IsCode(err, "SUBJECT_UNKNOWN"), "got %v", err)

	id, err := attest.ParseIdentity("acme/web")
	require.NoError(t, err)
	_, err = attest.Check(ctx, cl, d.CDN, ref, subject, id, &root.TrustedRoot{})
	require.Error(t, err)
}
