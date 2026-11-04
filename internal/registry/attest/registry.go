package attest

import (
	"context"
	"fmt"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Predicate is a claim to sign about an image.
type Predicate struct {
	Type string
	Body []byte
}

// Published is an attestation uploaded to the registry.
type Published struct {
	PredicateType string
	Digest        string
}

// SubjectName is the name statements give an image: its repository.
func SubjectName(ref naming.ImageReference) string {
	return ref.Registry + "/" + ref.Namespace + "/" + ref.Repository
}

// Publish signs each predicate as a statement about subject, a recipe or
// image index of the repository ref names, and uploads the attestations.
func Publish(ctx context.Context, cl *client.Client, token string, ref naming.ImageReference, subject string, predicates []Predicate, signer Signer) ([]Published, error) {
	out := make([]Published, 0, len(predicates))
	for _, p := range predicates {
		statement, err := NewStatement(SubjectName(ref), subject, p.Type, p.Body)
		if err != nil {
			return out, err
		}
		bundle, err := signer.Sign(ctx, statement)
		if err != nil {
			return out, err
		}
		digest, err := cl.PutAttestation(ctx, token, ref.Namespace, ref.Repository, subject, bundle)
		if err != nil {
			return out, fmt.Errorf("upload the %s attestation: %w", p.Type, err)
		}
		out = append(out, Published{PredicateType: p.Type, Digest: digest})
	}
	return out, nil
}

// maxChecked bounds the attestations read for one subject.
const maxChecked = 32

// Check verifies the attestations of subject in the repository ref names
// against identity, and returns those signed by it.
func Check(ctx context.Context, cl *client.Client, cdn string, ref naming.ImageReference, subject string, id Identity, trusted root.TrustedMaterial) ([]Result, error) {
	refs, err := cl.Attestations(ctx, cdn, ref.Namespace, ref.Repository, subject)
	if err != nil {
		return nil, err
	}
	if len(refs) > maxChecked {
		refs = refs[len(refs)-maxChecked:]
	}
	var entities []verify.SignedEntity
	for _, a := range refs {
		data, err := cl.Attestation(ctx, cdn, ref.Namespace, ref.Repository, subject, a.Digest)
		if err != nil {
			return nil, err
		}
		b, err := ParseBundle(data)
		if err != nil {
			// Someone else's malformed upload; it cannot vouch for anything.
			continue
		}
		entities = append(entities, b)
	}
	return Verify(trusted, subject, id, entities)
}
