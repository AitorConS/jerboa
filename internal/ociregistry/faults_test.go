package ociregistry

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A manifest names, by digest and size, every file an image is made of, and
// those digests become file names. These tests feed the parser manifests cut
// short by a dropped download and manifests written to mislead it.

const validDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validManifestBytes(t testing.TB) []byte {
	t.Helper()
	data, err := MarshalManifest(Manifest{
		SchemaVersion: OCIManifestSchemaVersion,
		MediaType:     MediaTypeImageManifest,
		Config:        Descriptor{MediaType: MediaTypeImageConfig, Digest: validDigest, Size: 120},
		Layers: []Descriptor{
			{MediaType: MediaTypeImageLayerTarGzip, Digest: validDigest, Size: 4096, Annotations: map[string]string{"k": "v"}},
			{MediaType: MediaTypeImageLayerTarGzip, Digest: "sha256:" + strings.Repeat("b", 64), Size: 1},
		},
	})
	require.NoError(t, err)
	_, err = ParseManifest(data)
	require.NoError(t, err)
	return data
}

// No prefix of a manifest is a manifest: a download cut anywhere is refused
// rather than read as an image with fewer layers.
func TestFaults_TruncatedManifestIsNeverValid(t *testing.T) {
	data := validManifestBytes(t)
	for n := range len(data) - 1 {
		m, err := ParseManifest(data[:n])
		require.Error(t, err, "the first %d of %d bytes parsed as a manifest", n, len(data))
		require.Equal(t, Manifest{}, m)
	}
}

func TestFaults_HostileManifests(t *testing.T) {
	whole := string(validManifestBytes(t))
	replace := func(old, repl string) []byte {
		require.Contains(t, whole, old)
		return []byte(strings.Replace(whole, old, repl, 1))
	}
	cases := map[string][]byte{
		"empty":                    nil,
		"null":                     []byte("null"),
		"an array":                 []byte("[]"),
		"html":                     []byte("<html>502 Bad Gateway</html>"),
		"zeroed":                   make([]byte, len(whole)),
		"digest as a path":         replace(`"digest": "`+validDigest+`"`, `"digest": "sha256:../../../../etc/passwd"`),
		"digest with a path after": replace(`"digest": "`+validDigest+`"`, `"digest": "`+validDigest+`/../../x"`),
		"digest upper-cased":       replace(`"digest": "`+validDigest+`"`, `"digest": "`+strings.ToUpper(validDigest)+`"`),
		"digest of another kind":   replace(`"digest": "`+validDigest+`"`, `"digest": "md5:`+strings.Repeat("a", 32)+`"`),
		"digest with a NUL":        replace(`"digest": "`+validDigest+`"`, `"digest": "sha256:`+strings.Repeat("a", 63)+`\u0000"`),
		"digest as a number":       replace(`"digest": "`+validDigest+`"`, `"digest": 7`),
		"negative size":            replace(`"size": 120`, `"size": -120`),
		"zero size":                replace(`"size": 120`, `"size": 0`),
		"size as a string":         replace(`"size": 120`, `"size": "120"`),
		"size beyond int64":        replace(`"size": 120`, `"size": 99999999999999999999`),
		"fractional size":          replace(`"size": 120`, `"size": 1.5`),
		"old schema":               replace(`"schemaVersion": 2`, `"schemaVersion": 1`),
		"an image index":           replace(MediaTypeImageManifest, "application/vnd.oci.image.index.v1+json"),
		"no layers":                []byte(`{"schemaVersion":2,"config":{"mediaType":"x","digest":"` + validDigest + `","size":1},"layers":[]}`),
		"layers null":              []byte(`{"schemaVersion":2,"config":{"mediaType":"x","digest":"` + validDigest + `","size":1},"layers":null}`),
		"no config":                []byte(`{"schemaVersion":2,"layers":[{"mediaType":"x","digest":"` + validDigest + `","size":1}]}`),
		"layer without media type": []byte(`{"schemaVersion":2,"config":{"mediaType":"x","digest":"` + validDigest + `","size":1},"layers":[{"digest":"` + validDigest + `","size":1}]}`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseManifest(data)
			require.Error(t, err)
			assert.Equal(t, Manifest{}, m, "a refused manifest was returned beside its error")
		})
	}
}

// A manifest written by this package reads back as itself, so a store that
// saves and reloads one does not change what it describes.
func TestFaults_MarshalRoundTrips(t *testing.T) {
	data := validManifestBytes(t)
	m, err := ParseManifest(data)
	require.NoError(t, err)
	again, err := MarshalManifest(m)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(data, again))
}

func FuzzParseManifest(f *testing.F) {
	whole := validManifestBytes(f)
	f.Add(whole)
	f.Add(whole[:len(whole)/2])
	f.Add([]byte(strings.Replace(string(whole), validDigest, "sha256:../../../../etc/passwd", 1)))
	f.Add([]byte(`{"schemaVersion":2,"config":{},"layers":[{}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseManifest(data)
		if err != nil {
			require.Equal(t, Manifest{}, m)
			return
		}
		// Every digest of a manifest that parses is safe to use as a file
		// name, and every size is a size.
		for _, d := range append([]Descriptor{m.Config}, m.Layers...) {
			require.Regexp(t, `^sha256:[0-9a-f]{64}$`, d.Digest)
			require.Positive(t, d.Size)
			require.NotEmpty(t, d.MediaType)
		}
		require.NotEmpty(t, m.Layers)
		out, err := MarshalManifest(m)
		require.NoError(t, err)
		back, err := ParseManifest(out)
		require.NoError(t, err)
		require.Equal(t, m, back)
	})
}
