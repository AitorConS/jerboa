package naming

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateResourceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr error // sentinel expected, or nil for accepted
	}{
		// Accepted names.
		{"simple", "app", nil},
		{"hyphenated", "app-backend", nil},
		{"underscore digit", "db_1", nil},
		{"internal dot", "web.prod", nil},
		{"single char", "a", nil},
		{"leading digit", "N123", nil},
		{"max length", strings.Repeat("x", MaxResourceNameLen), nil},
		{"reserved-like but longer", "console", nil}, // not exactly CON
		{"com but no digit", "com", nil},             // COM without 1-9 is allowed

		// Rejected: structural.
		{"empty", "", ErrInvalidResourceName},
		{"dotdot", "..", ErrInvalidResourceName},
		{"single dot", ".", ErrInvalidResourceName},
		{"parent prefix", "../x", ErrInvalidResourceName},
		{"forward slash", "a/b", ErrInvalidResourceName},
		{"backslash", `a\b`, ErrInvalidResourceName},
		{"embedded traversal", "foo/../bar", ErrInvalidResourceName},
		{"absolute", "/abs", ErrInvalidResourceName},
		{"double dot no slash", "a..b", ErrInvalidResourceName},
		{"leading dot hidden", ".hidden", ErrInvalidResourceName},
		{"leading dash", "-leading-dash", ErrInvalidResourceName},
		{"space", "name with space", ErrInvalidResourceName},
		{"too long", strings.Repeat("x", MaxResourceNameLen+1), ErrInvalidResourceName},

		// Rejected: hostile bytes.
		{"nul byte", "a\x00b", ErrInvalidResourceName},
		{"newline", "a\nb", ErrInvalidResourceName},
		{"tab", "a\tb", ErrInvalidResourceName},

		// Rejected: Windows portability hazards.
		{"trailing dot", "app.", ErrInvalidResourceName},
		{"reserved CON", "CON", ErrInvalidResourceName},
		{"reserved lowercase nul", "nul", ErrInvalidResourceName},
		{"reserved with ext", "CON.txt", ErrInvalidResourceName},
		{"reserved COM1", "com1", ErrInvalidResourceName},
		{"reserved LPT9", "LPT9", ErrInvalidResourceName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateResourceName("network", tt.input)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateResourceName(%q) = %v, want nil", tt.input, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateResourceName(%q) = %v, want errors.Is(_, %v)", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestSafeJoinStaysInRoot(t *testing.T) {
	t.Parallel()
	root := filepath.FromSlash("/store/root")
	got, err := SafeJoin(root, "app")
	if err != nil {
		t.Fatalf("SafeJoin: %v", err)
	}
	if got != filepath.Join(root, "app") {
		t.Fatalf("SafeJoin = %q", got)
	}
	// The result must be a single component directly under root.
	if dir := filepath.Dir(got); dir != filepath.Clean(root) {
		t.Fatalf("SafeJoin produced a nested path: dir=%q, want %q", dir, filepath.Clean(root))
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	t.Parallel()
	root := filepath.FromSlash("/store/root")
	// These would only reach SafeJoin if validation were bypassed; the guard is
	// defense in depth.
	for _, name := range []string{"../../etc", ".." + string(filepath.Separator) + "x"} {
		_, err := SafeJoin(root, name)
		if !errors.Is(err, ErrPathEscape) {
			t.Errorf("SafeJoin(%q) = %v, want errors.Is(_, ErrPathEscape)", name, err)
		}
	}
}

// FuzzValidateResourceName ties the two guards together as a property: any name
// ValidateResourceName accepts MUST also be accepted by SafeJoin and resolve to
// exactly one component directly under the root. If the validator ever lets a
// traversal or separator through, SafeJoin will escape the root or nest, and
// this property fails — catching a class of bugs no fixed table can enumerate.
func FuzzValidateResourceName(f *testing.F) {
	seeds := []string{
		"app", "web.prod", "N123", "a", "..", "../x", "a/b", `a\b`,
		"foo/../bar", "/abs", ".hidden", "-x", "x y", "app.", "CON",
		"nul", "com1", "a\x00b", strings.Repeat("x", MaxResourceNameLen+1),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	root := filepath.FromSlash("/store/root")
	cleanRoot := filepath.Clean(root)

	f.Fuzz(func(t *testing.T, name string) {
		if err := ValidateResourceName("fuzz", name); err != nil {
			return // rejected names carry no guarantee; nothing to check
		}

		// Property 1: a validated name never contains a separator or traversal.
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			t.Fatalf("validated name %q contains a separator or traversal", name)
		}

		// Property 2: SafeJoin must accept it and keep it inside the root.
		joined, err := SafeJoin(root, name)
		if err != nil {
			t.Fatalf("validated name %q rejected by SafeJoin: %v", name, err)
		}
		if dir := filepath.Dir(joined); dir != cleanRoot {
			t.Fatalf("validated name %q escaped or nested: dir=%q, want %q", name, dir, cleanRoot)
		}
	})
}

func TestComposeLocalName(t *testing.T) {
	t.Parallel()
	prefix := ComposeScopedPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	if prefix != "compose-010203040506-" {
		t.Fatalf("prefix = %q", prefix)
	}
	for scoped, want := range map[string]string{
		prefix + "app":              "app",
		prefix + "back-end":         "back-end",
		"app":                       "app",
		"compose-app":               "compose-app",
		"compose-010203040506-":     "compose-010203040506-",
		"compose-01020304050g-app":  "compose-01020304050g-app",
		"compose-0102030405060-app": "compose-0102030405060-app",
	} {
		if got := ComposeLocalName(scoped); got != want {
			t.Errorf("ComposeLocalName(%q) = %q, want %q", scoped, got, want)
		}
	}
}
