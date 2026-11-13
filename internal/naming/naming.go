// Package naming validates user-supplied resource names (networks, volumes)
// before they are used to build filesystem paths. Names arrive over the daemon
// RPC surface and are joined onto a store root, so an unvalidated name like
// "../x" would let a client read, write, or delete outside the store.
package naming

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxResourceNameLen bounds a resource name. It is generous enough for
// descriptive names while keeping derived artifacts (directories, labels) sane.
const MaxResourceNameLen = 64

// Sentinel errors so callers and tests can classify a rejection with
// errors.Is instead of matching on message text.
var (
	// ErrInvalidResourceName wraps every rejection from ValidateResourceName.
	// The wrapped message still explains the specific reason for humans.
	ErrInvalidResourceName = errors.New("invalid resource name")
	// ErrPathEscape is returned by SafeJoin when a name would resolve outside
	// the store root.
	ErrPathEscape = errors.New("path escapes store root")
)

// resourceNameRe requires the name to start with an alphanumeric character and
// contain only alphanumerics, underscore, dot, and hyphen. Because the first
// character must be alphanumeric, the reserved traversal names "." and ".." are
// rejected, and there is no way to embed a path separator. The character class
// also excludes control characters and NUL.
var resourceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// windowsReserved is the set of Windows reserved device names. A file or
// directory named after one of these (optionally with an extension, e.g.
// "CON.txt") cannot be created reliably on Windows, so a name whose leading
// component matches must be refused even on non-Windows hosts: stores are
// portable and a name accepted on Linux must not become unusable on Windows.
var windowsReserved = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// ValidateResourceName reports whether name is safe to use as a single path
// component under a store root. kind is used only for the error message
// (e.g. "network", "volume"). Every returned error wraps ErrInvalidResourceName.
func ValidateResourceName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name must not be empty: %w", kind, ErrInvalidResourceName)
	}
	if len(name) > MaxResourceNameLen {
		return fmt.Errorf("%s name %q is too long (max %d characters): %w", kind, name, MaxResourceNameLen, ErrInvalidResourceName)
	}
	// Reject separators explicitly for a clearer message than the regex miss.
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("%s name %q must not contain path separators or %q: %w", kind, name, "..", ErrInvalidResourceName)
	}
	if !resourceNameRe.MatchString(name) {
		return fmt.Errorf("%s name %q is invalid (allowed: letters, digits, and _.- ; must start with a letter or digit): %w", kind, name, ErrInvalidResourceName)
	}
	// A trailing dot is accepted by the regex but Windows silently strips it,
	// so "app." and "app" would collide (and could be used to shadow another
	// resource's directory). Refuse it.
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("%s name %q must not end with a dot: %w", kind, name, ErrInvalidResourceName)
	}
	// Refuse Windows reserved device names, matching on the component before any
	// extension and case-insensitively, exactly as the Windows filesystem does.
	base := name
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	if _, ok := windowsReserved[strings.ToUpper(base)]; ok {
		return fmt.Errorf("%s name %q is a reserved device name: %w", kind, name, ErrInvalidResourceName)
	}
	return nil
}

// SafeJoin joins name onto root and verifies the result stays inside root, as a
// defense-in-depth guard against traversal even if the name slipped validation.
// It returns an error (wrapping ErrPathEscape) rather than a path when the join
// would escape.
func SafeJoin(root, name string) (string, error) {
	joined := filepath.Join(root, name)
	rel, err := filepath.Rel(root, joined)
	if err != nil {
		return "", fmt.Errorf("resolve path for %q: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path for %q escapes the store root: %w", name, ErrPathEscape)
	}
	return joined, nil
}

// composeScopeLen is the length of the hex project digest in a scoped name.
const composeScopeLen = 12

// ComposeScopedPrefix is what a compose project puts before the names of the
// networks and volumes it owns; digest identifies the project.
func ComposeScopedPrefix(digest []byte) string {
	return fmt.Sprintf("compose-%x-", digest[:composeScopeLen/2])
}

// ComposeLocalName is the name a compose file gives a resource whose scoped
// name is given: "app" for "compose-0123456789ab-app". Any other name is
// returned unchanged.
func ComposeLocalName(scoped string) string {
	rest, ok := strings.CutPrefix(scoped, "compose-")
	if !ok || len(rest) <= composeScopeLen+1 || rest[composeScopeLen] != '-' {
		return scoped
	}
	for _, c := range rest[:composeScopeLen] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return scoped
		}
	}
	return rest[composeScopeLen+1:]
}
