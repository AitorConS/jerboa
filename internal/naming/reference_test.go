package naming

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseImageReference(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	cases := map[string]ImageReference{
		"aitorcons/web":                          {Registry: DefaultRegistry, Namespace: "aitorcons", Repository: "web", Tag: "latest"},
		"aitorcons/web:v1.2":                     {Registry: DefaultRegistry, Namespace: "aitorcons", Repository: "web", Tag: "v1.2"},
		"registry.jerboa.dev/acme/api:prod":      {Registry: DefaultRegistry, Namespace: "acme", Repository: "api", Tag: "prod"},
		"localhost:8788/acme/api":                {Registry: "localhost:8788", Namespace: "acme", Repository: "api", Tag: "latest"},
		"localhost/acme/api":                     {Registry: "localhost", Namespace: "acme", Repository: "api", Tag: "latest"},
		"aitorcons/web@" + d:                     {Registry: DefaultRegistry, Namespace: "aitorcons", Repository: "web", Digest: d},
		"staging.registry.jerboa.dev/a/b:t@" + d: {Registry: "staging.registry.jerboa.dev", Namespace: "a", Repository: "b", Tag: "t", Digest: d},
	}
	for in, want := range cases {
		got, err := ParseImageReference(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"web", "Aitor/web", "a/b/c/d", "a/Web", "a/b:", "a/b@sha256:xyz", "-a/b", "a/b:-x", "a/.b"} {
		_, err := ParseImageReference(bad)
		require.Error(t, err, bad)
	}
}

func TestImageReferenceNames(t *testing.T) {
	r, err := ParseImageReference("aitorcons/web:v1")
	require.NoError(t, err)
	require.Equal(t, "aitorcons/web", r.Name())
	require.Equal(t, "aitorcons/web:v1", r.String())
	r, err = ParseImageReference("localhost:8788/acme/api")
	require.NoError(t, err)
	require.Equal(t, "localhost:8788/acme/api", r.Name())
}
