package release

import (
	"os"
	"path/filepath"
)

// PublicKeyB64 is the embedded minisign public key that every Jerboa binary
// verifies release manifests against. The matching private seed is held only as
// a CI secret (JERBOA_MINISIGN_SEED) and is never committed.
//
// To rotate: generate a new keypair, replace this value, ship a release signed
// with BOTH the old and new keys during the overlap, then retire the old key.
const PublicKeyB64 = "RWQUeMEQrLXFcshAMUevjf6nlhsSB1PuZYt5dFhb0za9aypwSAUMorsH"

// Default builds a Client for the production release origin, honoring the
// JERBOA_RELEASE_BASE override (useful for staging or local mirrors). It keeps
// the newest accepted manifest's generation time in ~/.jerboa, so an older
// manifest is refused.
func Default() (*Client, error) {
	base := os.Getenv("JERBOA_RELEASE_BASE")
	c, err := NewClient(base, PublicKeyB64)
	if err != nil {
		return nil, err
	}
	if home, err := os.UserHomeDir(); err == nil {
		c.StateDir = filepath.Join(home, ".jerboa")
	}
	return c, nil
}
