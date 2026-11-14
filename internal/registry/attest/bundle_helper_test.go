package attest

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// virtualSigstore returns a private Sigstore instance to sign and verify
// with. It issues no certificate transparency entries, so the verifier is
// told not to ask for them for the length of the test.
func virtualSigstore(t *testing.T) *ca.VirtualSigstore {
	t.Helper()
	saved := verifierOptions
	verifierOptions = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	t.Cleanup(func() { verifierOptions = saved })
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	return vs
}

// signedBundle signs statement as signer (a workflow URL) and returns the
// attestation as the Sigstore bundle a registry stores, with the signature
// logged at the given time.
func signedBundle(t *testing.T, vs *ca.VirtualSigstore, signer, issuer string, statement []byte, at time.Time) []byte {
	t.Helper()
	e, err := vs.AttestAtTime(signer, issuer, statement, at, true)
	require.NoError(t, err)
	return encodeBundle(t, e)
}

// encodeBundle writes a signed entity in the bundle format, field by field.
func encodeBundle(t *testing.T, e *ca.TestEntity) []byte {
	t.Helper()
	vc, err := e.VerificationContent()
	require.NoError(t, err)
	sc, err := e.SignatureContent()
	require.NoError(t, err)
	env := sc.EnvelopeContent().RawEnvelope()
	sig, err := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	require.NoError(t, err)
	payload, err := env.DecodeB64Payload()
	require.NoError(t, err)
	entries, err := e.TlogEntries()
	require.NoError(t, err)
	var tlogs []json.RawMessage
	for _, entry := range entries {
		data, err := protojson.Marshal(entry.TransparencyLogEntry())
		require.NoError(t, err)
		// The test instance leaves out what a bundle read back must state:
		// the kind of the log entry.
		var fields map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		fields["kindVersion"] = map[string]string{"kind": "dsse", "version": "0.0.1"}
		data, err = json.Marshal(fields)
		require.NoError(t, err)
		tlogs = append(tlogs, data)
	}
	stamps, err := e.Timestamps()
	require.NoError(t, err)
	var rfc3161 []map[string][]byte
	for _, s := range stamps {
		rfc3161 = append(rfc3161, map[string][]byte{"signedTimestamp": s})
	}
	data, err := json.Marshal(map[string]any{
		"mediaType": BundleMediaType,
		"verificationMaterial": map[string]any{
			"certificate":               map[string][]byte{"rawBytes": vc.Certificate().Raw},
			"tlogEntries":               tlogs,
			"timestampVerificationData": map[string]any{"rfc3161Timestamps": rfc3161},
		},
		"dsseEnvelope": map[string]any{
			"payload":     payload,
			"payloadType": env.PayloadType,
			"signatures":  []map[string]any{{"sig": sig, "keyid": env.Signatures[0].KeyID}},
		},
	})
	require.NoError(t, err)
	return data
}

func TestBundleHelperRoundTrips(t *testing.T) {
	vs := virtualSigstore(t)
	data := signedBundle(t, vs, workflow, GitHubIssuer, statement(t, subject), time.Now().Add(5*time.Minute))
	b, err := ParseBundle(data)
	require.NoError(t, err)
	id, err := ParseIdentity("acme/app")
	require.NoError(t, err)
	got, err := Verify(vs, subject, id, []verify.SignedEntity{b})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, workflow, got[0].Signer)
}
