package attest

import (
	"context"
	"fmt"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"google.golang.org/protobuf/encoding/protojson"
)

// SigstoreAudience is the audience of the OIDC token Fulcio accepts.
const SigstoreAudience = "sigstore"

// Signer signs an in-toto statement and returns the Sigstore bundle.
type Signer interface {
	Sign(ctx context.Context, statement []byte) ([]byte, error)
}

// Trust is the Sigstore public-good instance as its TUF repository
// describes it: the roots to verify against and the services to sign with.
type Trust struct {
	Root    *root.TrustedRoot
	Signing *root.SigningConfig
}

// LoadTrust reads the trusted root and signing config through TUF, caching
// them in cacheDir. The TUF root jerboa ships with (sigstore-go's) anchors
// the chain; an up to date cache is used without the network.
func LoadTrust(ctx context.Context, cacheDir string) (Trust, error) {
	opts := tuf.DefaultOptions().WithCachePath(cacheDir).WithCacheValidity(1).WithContext(ctx)
	client, err := tuf.New(opts)
	if err != nil {
		return Trust{}, fmt.Errorf("sigstore: update the trusted root: %w", err)
	}
	trusted, err := root.GetTrustedRoot(client)
	if err != nil {
		return Trust{}, fmt.Errorf("sigstore: trusted root: %w", err)
	}
	signing, err := root.GetSigningConfig(client)
	if err != nil {
		return Trust{}, fmt.Errorf("sigstore: signing config: %w", err)
	}
	return Trust{Root: trusted, Signing: signing}, nil
}

// Sigstore signs keyless: an ephemeral key, certified by Fulcio for the
// identity in an OIDC token, and the signature recorded in Rekor (and
// timestamped, where the instance runs a timestamp authority).
type Sigstore struct {
	Trust Trust
	// IDToken returns an OIDC token for SigstoreAudience.
	IDToken func(ctx context.Context) (string, error)
}

// Sign implements Signer.
func (s Sigstore) Sign(ctx context.Context, statement []byte) ([]byte, error) {
	token, err := s.IDToken(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	fulcio, err := root.SelectService(s.Trust.Signing.FulcioCertificateAuthorityURLs(), sign.FulcioAPIVersions, now)
	if err != nil {
		return nil, fmt.Errorf("sigstore: fulcio: %w", err)
	}
	rekors, err := root.SelectServices(s.Trust.Signing.RekorLogURLs(), s.Trust.Signing.RekorLogURLsConfig(), sign.RekorAPIVersions, now)
	if err != nil {
		return nil, fmt.Errorf("sigstore: rekor: %w", err)
	}
	opts := sign.BundleOptions{
		Context:     ctx,
		TrustedRoot: s.Trust.Root,
		CertificateProvider: sign.NewFulcio(&sign.FulcioOptions{
			BaseURL: fulcio.URL, Timeout: 30 * time.Second, Retries: 2,
		}),
		CertificateProviderOptions: &sign.CertificateProviderOptions{IDToken: token},
	}
	for _, r := range rekors {
		opts.TransparencyLogs = append(opts.TransparencyLogs, sign.NewRekor(&sign.RekorOptions{
			BaseURL: r.URL, Timeout: 90 * time.Second, Retries: 2, Version: r.MajorAPIVersion,
		}))
	}
	if tsas := s.Trust.Signing.TimestampAuthorityURLs(); len(tsas) > 0 {
		selected, err := root.SelectServices(tsas, s.Trust.Signing.TimestampAuthorityURLsConfig(), sign.TimestampAuthorityAPIVersions, now)
		if err != nil {
			return nil, fmt.Errorf("sigstore: timestamp authority: %w", err)
		}
		for _, t := range selected {
			opts.TimestampAuthorities = append(opts.TimestampAuthorities, sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{
				URL: t.URL, Timeout: 30 * time.Second, Retries: 2,
			}))
		}
	}
	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, fmt.Errorf("sigstore: %w", err)
	}
	b, err := sign.Bundle(&sign.DSSEData{Data: statement, PayloadType: PayloadType}, keypair, opts)
	if err != nil {
		return nil, fmt.Errorf("sigstore: sign: %w", err)
	}
	data, err := protojson.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("sigstore: encode bundle: %w", err)
	}
	return data, nil
}
