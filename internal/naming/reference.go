package naming

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultRegistry is the registry image references name when they name none.
const DefaultRegistry = "registry.jerboa.dev"

var (
	namespaceRE  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38})$`)
	repositoryRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	tagRE        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// A registry is a host name or an IPv4 address, or an IPv6 address in
	// brackets, with an optional port. Nothing else: "user@host" would be
	// dialled as host while the reference shows the text before the "@".
	registryRE = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)
)

// ImageReference names an image in a registry:
// [registry/]namespace/repository[:tag|@sha256:<hex>].
type ImageReference struct {
	Registry   string
	Namespace  string
	Repository string
	Tag        string
	Digest     string
}

// normalizeRegistry gives the one spelling of a registry host: lowercase,
// without the root's trailing dot and without the HTTPS default port. The
// signing rules and the remembered signers are looked up by this name, so
// "REGISTRY.JERBOA.DEV", "registry.jerboa.dev." and "registry.jerboa.dev:443"
// must not name a registry other than "registry.jerboa.dev".
func normalizeRegistry(host string) string {
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ":443")
	return strings.TrimSuffix(host, ".")
}

// ParseImageReference parses s. The first component is a registry when it
// contains a dot or a colon or is "localhost", as with Docker; otherwise the
// registry is DefaultRegistry. Without a tag or digest the tag is "latest".
// The registry is returned in its normalized spelling.
func ParseImageReference(s string) (ImageReference, error) {
	var r ImageReference
	rest := s
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		r.Digest = rest[i+1:]
		rest = rest[:i]
		if !digestRE.MatchString(r.Digest) {
			return r, fmt.Errorf("image reference %q: invalid digest", s)
		}
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 3 || (len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost")) {
		r.Registry, parts = normalizeRegistry(parts[0]), parts[1:]
		if len(r.Registry) > 253 || !registryRE.MatchString(r.Registry) {
			return r, fmt.Errorf("image reference %q: invalid registry (want host[:port])", s)
		}
	} else {
		r.Registry = DefaultRegistry
	}
	if len(parts) != 2 {
		return r, fmt.Errorf("image reference %q: want [registry/]namespace/repository[:tag]", s)
	}
	r.Namespace, r.Repository = parts[0], parts[1]
	if i := strings.LastIndex(r.Repository, ":"); i >= 0 {
		r.Tag, r.Repository = r.Repository[i+1:], r.Repository[:i]
		if !tagRE.MatchString(r.Tag) {
			return r, fmt.Errorf("image reference %q: invalid tag", s)
		}
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	if !namespaceRE.MatchString(r.Namespace) {
		return r, fmt.Errorf("image reference %q: invalid namespace (lowercase GitHub login or organization)", s)
	}
	if len(r.Repository) > 128 || !repositoryRE.MatchString(r.Repository) {
		return r, fmt.Errorf("image reference %q: invalid repository", s)
	}
	return r, nil
}

// Name is the image name in the local store: namespace/repository, prefixed
// by the registry unless it is the default one.
func (r ImageReference) Name() string {
	name := r.Namespace + "/" + r.Repository
	if r.Registry != DefaultRegistry {
		name = r.Registry + "/" + name
	}
	return name
}

// String renders the reference in its shortest form.
func (r ImageReference) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}
