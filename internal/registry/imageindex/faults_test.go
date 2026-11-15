package imageindex

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An index decides which recipe a tag pulls on each platform. One that is
// cut short, altered, or written to mislead must be refused whole: there is
// no such thing as reading the entries that survived.

func validIndex(t testing.TB) []byte {
	t.Helper()
	ix, err := New([]Entry{arm, amd})
	require.NoError(t, err)
	data, err := ix.Encode()
	require.NoError(t, err)
	return data
}

func TestFaults_TruncatedIndexIsNeverValid(t *testing.T) {
	data := validIndex(t)
	for n := range len(data) {
		ix, err := Parse(data[:n])
		require.Error(t, err, "the first %d of %d bytes parsed as an index", n, len(data))
		require.Nil(t, ix)
	}
}

// Any byte changed: the index is refused, or it is still exactly an index in
// canonical form (so its digest, which the client checks, has changed).
func TestFaults_AlteredIndex(t *testing.T) {
	data := validIndex(t)
	for i := range data {
		for _, bit := range []byte{0x01, 0x20} {
			damaged := bytes.Clone(data)
			damaged[i] ^= bit
			ix, err := Parse(damaged)
			if err != nil {
				continue
			}
			again, err := ix.Encode()
			require.NoError(t, err)
			require.Equal(t, damaged, again, "byte %d: an index that is not canonical parsed", i)
			require.NoError(t, ix.Validate())
		}
	}
}

func TestFaults_HostileIndexes(t *testing.T) {
	encode := func(ix Index) []byte {
		data, err := json.Marshal(ix)
		require.NoError(t, err)
		return data
	}
	base := Index{SchemaVersion: SchemaVersion, MediaType: MediaType}
	with := func(entries ...Entry) Index {
		ix := base
		ix.Manifests = entries
		return ix
	}
	many := make([]Entry, 0, MaxPlatforms+1)
	for i := range MaxPlatforms + 1 {
		many = append(many, Entry{Digest: amd.Digest, Platform: "linux/amd64" + strings.Repeat("x", i)})
	}
	whole := validIndex(t)
	cases := map[string][]byte{
		"empty":                    nil,
		"null":                     []byte("null"),
		"an array":                 []byte("[]"),
		"empty object":             []byte("{}"),
		"two indexes":              append(bytes.Clone(whole), whole...),
		"trailing newline":         append(bytes.Clone(whole), '\n'),
		"no entries":               encode(with()),
		"too many entries":         encode(with(many...)),
		"one platform twice":       encode(with(amd, amd)),
		"not sorted":               encode(with(arm, amd)),
		"future schema":            encode(Index{SchemaVersion: 2, MediaType: MediaType, Manifests: []Entry{amd}}),
		"another media type":       encode(Index{SchemaVersion: 1, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []Entry{amd}}),
		"digest as a path":         encode(with(Entry{Digest: "sha256:../../../etc/passwd", Platform: "linux/amd64"})),
		"digest upper-cased":       encode(with(Entry{Digest: strings.ToUpper(amd.Digest), Platform: "linux/amd64"})),
		"digest too short":         encode(with(Entry{Digest: amd.Digest[:70], Platform: "linux/amd64"})),
		"digest with a newline":    encode(with(Entry{Digest: amd.Digest + "\n", Platform: "linux/amd64"})),
		"another digest algorithm": encode(with(Entry{Digest: "sha512:" + strings.Repeat("a", 128), Platform: "linux/amd64"})),
		"unknown platform":         encode(with(Entry{Digest: amd.Digest, Platform: "windows/amd64"})),
		"platform as a path":       encode(with(Entry{Digest: amd.Digest, Platform: "linux/../amd64"})),
		"empty platform":           encode(with(Entry{Digest: amd.Digest})),
		"extra field in an entry":  []byte(strings.Replace(string(whole), `"platform":"linux/amd64"`, `"platform":"linux/amd64","url":"http://evil.example/disk"`, 1)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			ix, err := Parse(data)
			require.Error(t, err)
			assert.Nil(t, ix)
		})
	}
}

// Choosing from an index never falls back to another platform's image: a
// host without one gets an error naming what there is.
func TestFaults_ChooseNeverSubstitutesAPlatform(t *testing.T) {
	ix, err := New([]Entry{amd})
	require.NoError(t, err)
	for _, platform := range []string{"linux/arm64", "linux/riscv64", "linux/amd64 ", "LINUX/AMD64", "amd64", "linux", "/"} {
		d, p, err := ix.Choose(platform)
		require.Error(t, err, platform)
		assert.Empty(t, d)
		assert.Empty(t, p)
		assert.Contains(t, err.Error(), "linux/amd64")
	}
	d, p, err := ix.Choose("linux/amd64")
	require.NoError(t, err)
	assert.Equal(t, amd.Digest, d)
	assert.Equal(t, "linux/amd64", p)
	// The default for old clients is the first entry when there is no amd64.
	only, err := New([]Entry{arm})
	require.NoError(t, err)
	assert.Equal(t, arm.Digest, only.Default())
}

func FuzzParse(f *testing.F) {
	whole := validIndex(f)
	f.Add(whole)
	f.Add(whole[:len(whole)/2])
	f.Add([]byte(strings.Replace(string(whole), "linux/arm64", "linux/amd64", 1)))
	f.Add([]byte(`{"schemaVersion":1,"mediaType":"application/vnd.jerboa.image.index.v1+json","manifests":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		ix, err := Parse(data)
		if err != nil {
			require.Nil(t, ix)
			return
		}
		again, err := ix.Encode()
		require.NoError(t, err)
		require.Equal(t, data, again, "an index that is not in canonical form parsed")
		require.NotEmpty(t, ix.Manifests)
		require.LessOrEqual(t, len(ix.Manifests), MaxPlatforms)
		seen := map[string]bool{}
		for _, e := range ix.Manifests {
			require.Regexp(t, `^sha256:[0-9a-f]{64}$`, e.Digest)
			require.False(t, seen[e.Platform], "two recipes for one platform")
			seen[e.Platform] = true
			d, ok := ix.For(e.Platform)
			require.True(t, ok)
			require.Equal(t, e.Digest, d)
		}
		_, ok := ix.For(ix.Manifests[0].Platform)
		require.True(t, ok)
		require.NotEmpty(t, ix.Default())
	})
}
