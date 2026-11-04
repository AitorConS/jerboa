package imageindex

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	amd = Entry{Digest: "sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64"}
	arm = Entry{Digest: "sha256:" + strings.Repeat("b", 64), Platform: "linux/arm64"}
)

func TestCanonicalEncoding(t *testing.T) {
	ix, err := New([]Entry{arm, amd})
	require.NoError(t, err)
	digest, data, err := ix.Digest()
	require.NoError(t, err)
	// The same bytes the registry checks (JSON.stringify of the same object).
	require.Equal(t, `{"schemaVersion":1,"mediaType":"application/vnd.jerboa.image.index.v1+json","manifests":[`+
		`{"digest":"`+amd.Digest+`","platform":"linux/amd64"},{"digest":"`+arm.Digest+`","platform":"linux/arm64"}]}`, string(data))
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)

	parsed, err := Parse(data)
	require.NoError(t, err)
	require.Equal(t, ix, parsed)
	require.Equal(t, amd.Digest, parsed.Default())
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, parsed.Platforms())

	_, err = Parse([]byte(strings.Replace(string(data), `"manifests":`, `"manifests": `, 1)))
	require.ErrorContains(t, err, "canonical")
	_, err = Parse([]byte(strings.Replace(string(data), `{"schemaVersion":1,`, `{"schemaVersion":1,"x":1,`, 1)))
	require.ErrorContains(t, err, "unknown field")
}

func TestValidate(t *testing.T) {
	_, err := New(nil)
	require.ErrorContains(t, err, "1 to 8")
	_, err = New([]Entry{amd, amd})
	require.ErrorContains(t, err, "two recipes for linux/amd64")
	_, err = New([]Entry{{Digest: "sha256:x", Platform: "linux/amd64"}})
	require.ErrorContains(t, err, "invalid recipe digest")
	_, err = New([]Entry{{Digest: amd.Digest, Platform: "windows/amd64"}})
	require.ErrorContains(t, err, "unsupported platform")
	ix := &Index{SchemaVersion: SchemaVersion, MediaType: MediaType, Manifests: []Entry{arm, amd}}
	require.ErrorContains(t, ix.Validate(), "not sorted")
}

func TestChoose(t *testing.T) {
	ix, err := New([]Entry{arm})
	require.NoError(t, err)
	require.Equal(t, arm.Digest, ix.Default())
	d, p, err := ix.Choose("linux/arm64")
	require.NoError(t, err)
	require.Equal(t, arm.Digest, d)
	require.Equal(t, "linux/arm64", p)
	_, _, err = ix.Choose("linux/amd64")
	require.EqualError(t, err, "no image for linux/amd64; there is linux/arm64")
	if HostPlatform() != "linux/arm64" {
		_, _, err = ix.Choose("")
		require.ErrorContains(t, err, "choose one with --platform")
	}
}
