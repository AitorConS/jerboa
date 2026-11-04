// Package attest signs and verifies attestations of registry images: in-toto
// statements about a recipe or image index, signed keyless with Sigstore
// from a GitHub Actions job, and stored next to the image in the registry.
//
// An attestation says who published an image and how (SLSA provenance), or
// what it contains (its CycloneDX SBOM). The signature binds the statement to
// the identity GitHub vouched for: the repository, workflow and ref of the
// job, recorded in a short-lived Fulcio certificate and the public Rekor log.
// Verification checks that identity against the one the puller expects.
package attest

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// StatementType is the in-toto statement version this package writes.
	StatementType = "https://in-toto.io/Statement/v1"
	// PayloadType is the DSSE payload type of an in-toto statement.
	PayloadType = "application/vnd.in-toto+json"
	// ProvenanceType is the SLSA provenance predicate.
	ProvenanceType = "https://slsa.dev/provenance/v1"
	// SBOMType is the CycloneDX SBOM predicate.
	SBOMType = "https://cyclonedx.org/bom"
	// BundleMediaType is the Sigstore bundle format attestations travel in.
	BundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	// GitHubIssuer issues the OIDC tokens of GitHub Actions jobs.
	GitHubIssuer = "https://token.actions.githubusercontent.com"
	// MaxBundle bounds one attestation.
	MaxBundle = 256 << 10
)

// Subject names the artifact a statement is about.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Statement is an in-toto statement.
type Statement struct {
	Type          string          `json:"_type"`
	Subject       []Subject       `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     json.RawMessage `json:"predicate"`
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// NewStatement returns the statement that predicate, of predicateType, holds
// for the image name (registry/namespace/repository) with digest, a recipe
// or image index digest.
func NewStatement(name, digest, predicateType string, predicate []byte) ([]byte, error) {
	if !digestRE.MatchString(digest) {
		return nil, fmt.Errorf("attest: invalid subject digest %q", digest)
	}
	if !json.Valid(predicate) {
		return nil, errors.New("attest: the predicate is not JSON")
	}
	data, err := json.Marshal(Statement{
		Type:          StatementType,
		Subject:       []Subject{{Name: name, Digest: map[string]string{"sha256": strings.TrimPrefix(digest, "sha256:")}}},
		PredicateType: predicateType,
		Predicate:     predicate,
	})
	if err != nil {
		return nil, fmt.Errorf("attest: encode statement: %w", err)
	}
	return data, nil
}
