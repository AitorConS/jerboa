package attest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Identity is the GitHub Actions workflow an image must be signed from.
type Identity struct {
	// Repository is owner/repo.
	Repository string
	// Workflow is the workflow's path in the repository, such as
	// .github/workflows/publish.yml; empty allows any.
	Workflow string
	// Ref is the git ref the workflow ran on, such as refs/heads/main;
	// empty allows any.
	Ref string
}

var (
	repoRE     = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	workflowRE = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9._/-]+\.ya?ml$`)
	refRE      = regexp.MustCompile(`^refs/(heads|tags)/[A-Za-z0-9._/-]+$`)
)

// ParseIdentity reads owner/repo, owner/repo/.github/workflows/<file>, or
// owner/repo/.github/workflows/<file>@<ref>.
func ParseIdentity(s string) (Identity, error) {
	rest, ref, hasRef := strings.Cut(s, "@")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return Identity{}, fmt.Errorf("identity %q: want owner/repo[/.github/workflows/<file>[@<ref>]]", s)
	}
	id := Identity{Repository: parts[0] + "/" + parts[1]}
	if len(parts) == 3 {
		id.Workflow = parts[2]
	}
	if hasRef {
		id.Ref = ref
	}
	switch {
	case !repoRE.MatchString(id.Repository):
		return Identity{}, fmt.Errorf("identity %q: invalid repository %q", s, id.Repository)
	case id.Workflow != "" && !workflowRE.MatchString(id.Workflow):
		return Identity{}, fmt.Errorf("identity %q: the workflow is a path such as .github/workflows/publish.yml", s)
	case hasRef && (id.Workflow == "" || !refRE.MatchString(id.Ref)):
		return Identity{}, fmt.Errorf("identity %q: a ref follows a workflow, and is refs/heads/<branch> or refs/tags/<tag>", s)
	}
	return id, nil
}

// String renders the identity as ParseIdentity reads it.
func (id Identity) String() string {
	s := id.Repository
	if id.Workflow != "" {
		s += "/" + id.Workflow
	}
	if id.Ref != "" {
		s += "@" + id.Ref
	}
	return s
}

// certificate is the identity as Fulcio certifies it: the certificate's SAN
// is the workflow's URL, https://github.com/<repo>/<workflow>@<ref>.
func (id Identity) certificate() (verify.CertificateIdentity, error) {
	prefix := "https://github.com/" + id.Repository + "/"
	var san string
	switch {
	case id.Ref != "":
		san = "^" + regexp.QuoteMeta(prefix+id.Workflow+"@"+id.Ref) + "$"
	case id.Workflow != "":
		san = "^" + regexp.QuoteMeta(prefix+id.Workflow+"@") + "refs/.+$"
	default:
		san = "^" + regexp.QuoteMeta(prefix) + `\.github/workflows/[^@]+@refs/.+$`
	}
	c, err := verify.NewShortCertificateIdentity(GitHubIssuer, "", "", san)
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("attest: %w", err)
	}
	return c, nil
}

// ParseBundle reads a Sigstore bundle.
func ParseBundle(data []byte) (*bundle.Bundle, error) {
	if len(data) > MaxBundle {
		return nil, fmt.Errorf("attest: a bundle of %d bytes is over the %d byte limit", len(data), MaxBundle)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("attest: parse bundle: %w", err)
	}
	return &b, nil
}

// verifierOptions require what public-good Sigstore gives every signature:
// a certificate transparency entry for its certificate, a Rekor entry, and a
// trusted time it was made at.
var verifierOptions = []verify.VerifierOption{
	verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1),
}

// ErrUnsigned is the error of a subject without attestations.
var ErrUnsigned = errors.New("has no attestations")

// Result is one attestation that verified.
type Result struct {
	PredicateType string
	// Identity is the identity it verified against.
	Identity Identity
	// Signer is the workflow the certificate names (its SAN).
	Signer string
	// Commit, Ref and Run describe the job, from the certificate.
	Commit string
	Ref    string
	Run    string
	// Statement is the verified in-toto statement.
	Statement []byte
}

// Verify checks attestations of subject (a sha256 digest) against trusted
// material and identity, and returns those that verified. It fails unless at
// least one did: the others may be from someone else, so they only add to
// the error.
func Verify(trusted root.TrustedMaterial, subject string, id Identity, entities []verify.SignedEntity) ([]Result, error) {
	if !digestRE.MatchString(subject) {
		return nil, fmt.Errorf("attest: invalid subject digest %q", subject)
	}
	sum, _ := hex.DecodeString(strings.TrimPrefix(subject, "sha256:"))
	cert, err := id.certificate()
	if err != nil {
		return nil, err
	}
	v, err := verify.NewVerifier(trusted, verifierOptions...)
	if err != nil {
		return nil, fmt.Errorf("attest: %w", err)
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithCertificateIdentity(cert))
	var (
		out  []Result
		errs []error
	)
	for _, e := range entities {
		res, err := v.Verify(e, policy)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r := Result{}
		if res.Statement != nil {
			r.PredicateType = res.Statement.GetPredicateType()
		}
		if sig, err := e.SignatureContent(); err == nil && sig.EnvelopeContent() != nil {
			r.Statement, _ = sig.EnvelopeContent().RawEnvelope().DecodeB64Payload()
		}
		if res.Signature != nil && res.Signature.Certificate != nil {
			c := res.Signature.Certificate
			r.Signer, r.Commit, r.Ref, r.Run = c.SubjectAlternativeName, c.SourceRepositoryDigest, c.SourceRepositoryRef, c.RunInvocationURI
		}
		r.Identity = id
		out = append(out, r)
	}
	if len(out) == 0 {
		if len(entities) == 0 {
			return nil, fmt.Errorf("%s %w", subject, ErrUnsigned)
		}
		return nil, fmt.Errorf("no attestation of %s is signed by %s: %w", subject, id, errors.Join(errs...))
	}
	return out, nil
}
