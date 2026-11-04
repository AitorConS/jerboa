package attest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/AitorConS/jerboa/internal/naming"
)

// Rule requires the images of a namespace to be signed by an identity.
type Rule struct {
	// Registry is the registry host; empty means naming.DefaultRegistry.
	Registry  string `json:"registry,omitempty"`
	Namespace string `json:"namespace"`
	// Identity is read by ParseIdentity.
	Identity string `json:"identity"`
}

// Policy is the rules a pull enforces: an image a rule matches is pulled
// only if an attestation of it verifies against the rule's identity.
type Policy struct {
	Require []Rule `json:"require"`
}

// Official is the rule jerboa enforces whatever the policy file says: the
// official images, under jerboa/ on the default registry, are published from
// the jerboa-images repository's publish workflow on main.
var Official = Rule{
	Registry:  naming.DefaultRegistry,
	Namespace: "jerboa",
	Identity:  "AitorConS/jerboa-images/.github/workflows/publish.yml@refs/heads/main",
}

// LoadPolicy reads a policy file; a missing file is an empty policy.
func LoadPolicy(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Policy{}, nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("policy: %w", err)
	}
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy %s: %w", path, err)
	}
	for _, r := range p.Require {
		if r.Namespace == "" {
			return Policy{}, fmt.Errorf("policy %s: a rule names no namespace", path)
		}
		if _, err := ParseIdentity(r.Identity); err != nil {
			return Policy{}, fmt.Errorf("policy %s: %w", path, err)
		}
	}
	return p, nil
}

// For returns the identity the images of namespace on registry must be
// signed by, if a rule requires one. Official comes first, so no policy file
// can loosen it.
func (p Policy) For(registry, namespace string) (Identity, bool, error) {
	for _, r := range append([]Rule{Official}, p.Require...) {
		host := r.Registry
		if host == "" {
			host = naming.DefaultRegistry
		}
		if host == registry && r.Namespace == namespace {
			id, err := ParseIdentity(r.Identity)
			return id, err == nil, err
		}
	}
	return Identity{}, false, nil
}
