package attest

import (
	"context"
	"errors"
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

// Listed is an attestation as the registry lists it: the bundle, and the
// workflow the registry recorded as its uploader (owner/repo/<workflow path>).
type Listed struct {
	Identity string
	Entity   verify.SignedEntity
}

// Check verifies the attestations of subject in the repository ref names:
// against want, or, when want is nil, each against the workflow the
// registry recorded for it. It returns those that verified.
func Check(ctx context.Context, cl *client.Client, cdn string, ref naming.ImageReference, subject string, want *Identity, trusted root.TrustedMaterial) ([]Result, error) {
	refs, err := cl.Attestations(ctx, cdn, ref.Namespace, ref.Repository, subject)
	if err != nil {
		return nil, err
	}
	if len(refs) > maxChecked {
		refs = refs[len(refs)-maxChecked:]
	}
	listed := make([]Listed, 0, len(refs))
	for _, a := range refs {
		data, err := cl.Attestation(ctx, cdn, ref.Namespace, ref.Repository, subject, a.Digest)
		if err != nil {
			return nil, err
		}
		b, err := ParseBundle(data)
		if err != nil {
			// A malformed upload cannot vouch for anything.
			continue
		}
		listed = append(listed, Listed{Identity: a.Identity, Entity: b})
	}
	return VerifyListed(trusted, subject, want, listed)
}

// VerifyListed verifies attestations of subject against want, or, when want
// is nil, each against the workflow the registry recorded for it, so its
// certificate must name the workflow whose OIDC token uploaded it. It fails
// unless at least one verifies; results are in the order listed, oldest
// first.
func VerifyListed(trusted root.TrustedMaterial, subject string, want *Identity, listed []Listed) ([]Result, error) {
	if want != nil {
		entities := make([]verify.SignedEntity, len(listed))
		for i, l := range listed {
			entities[i] = l.Entity
		}
		return Verify(trusted, subject, *want, entities)
	}
	if len(listed) == 0 {
		return nil, fmt.Errorf("%s %w", subject, ErrUnsigned)
	}
	var (
		out  []Result
		errs []error
	)
	for _, l := range listed {
		id, err := ParseIdentity(l.Identity)
		if err == nil && id.Workflow == "" {
			err = fmt.Errorf("identity %q names no workflow", l.Identity)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		res, err := Verify(trusted, subject, id, []verify.SignedEntity{l.Entity})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, res...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no attestation of %s is signed by the workflow that uploaded it: %w", subject, errors.Join(errs...))
	}
	return out, nil
}
