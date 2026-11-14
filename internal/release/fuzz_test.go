package release

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fuzz targets feed the parsers of everything a release origin serves.
// None of it is trusted before it parses, so none of it may panic, and what a
// parser accepts must hold the invariants the rest of the client relies on.

// fuzzSigner is a fixed key pair, so corpus entries stay valid across runs.
func fuzzSigner() *testSigner {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	return &testSigner{keyID: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, pub: priv.Public().(ed25519.PublicKey), priv: priv}
}

func FuzzParseManifest(f *testing.F) {
	for _, seed := range []string{
		`{"channel":"stable","components":{"cli":{"version":"v0.4.0","url":"https://x/cli","sha256":"abc","size":10}}}`,
		`{"channel":"stable","generated":"2026-10-20T10:00:00Z","components":{"daemon":{"version":"v1","platforms":{"linux-amd64":{"url":"u","sha256":"ab"}},"min_cli":"v0.3.0","proto":3}}}`,
		`{"channel":"beta","components":{"kernel":{"version":"v1","files":{"mkfs":{"url":"u","sha256":"ab","size":1}}},"distro":{"version":"v2","url":"u","sha256":"ab","kernel":"v1"}}}`,
		`{"channel":"stable","components":{"cli":{"version":"v1"}}}`,
		`{"channel":"stable","components":{"cli":{"version":"v1","url":"u","sha256":"ab","size":-1}}}`,
		`{"channel":"stable","components":{}}`,
		`{"channel":"stable","components":null}`,
		`{"channel":"stable","components":{"cli":null}}`,
		`{"channel":"stable","components":{"cli":{"version":"v1","platforms":{"":{}}}}}`,
		`{"channel":"stable","components":{"cli":{"version":"v1","url":"u","sha256":"ab"}}}{"channel":"beta"}`,
		`[]`, `null`, `{`, ``, "\x00", `{"channel":1e999}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseManifest(data)
		if err != nil {
			require.Nil(t, m)
			return
		}
		require.NotEmpty(t, m.Channel)
		require.NotEmpty(t, m.Components)
		for name, c := range m.Components {
			require.NotEmpty(t, c.Version, "component %q", name)
			// Every asset a caller can reach by platform or by file name
			// carries the hash it will be checked against.
			for _, a := range c.Platforms {
				require.True(t, a.valid())
			}
			for _, a := range c.Files {
				require.True(t, a.valid())
			}
			if len(c.Platforms) == 0 && len(c.Files) == 0 {
				a, err := c.Asset("linux", "amd64")
				require.NoError(t, err)
				require.True(t, a.valid())
			}
		}
		// What was accepted survives being written out and read back.
		out, err := m.Marshal()
		require.NoError(t, err)
		again, err := ParseManifest(out)
		require.NoError(t, err)
		require.Equal(t, m, again)
		_ = m.CheckDaemonCompat("v0.1.0")
	})
}

func FuzzParseToolset(f *testing.F) {
	for _, seed := range []string{
		`{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"https://x/mkfs","sha256":"ab","size":4}}}`,
		`{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":0}}}`,
		`{"version":"v0.2.3","mkfs":{"":{"url":"u","sha256":"ab","size":9223372036854775807}}}`,
		`{"version":"v0.2.2","mkfs":{}}`,
		`{"version":"v0.2.3","mkfs":null}`,
		`{"version":"v0.2.3","mkfs":{"linux-amd64":null}}`,
		`{}`, `[]`, ``, `{"version":`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ts, err := ParseToolset(data, "v0.2.3")
		if err != nil {
			require.Nil(t, ts)
			return
		}
		require.Equal(t, "v0.2.3", ts.Version)
		require.NotEmpty(t, ts.Mkfs)
		for _, a := range ts.Mkfs {
			require.NotEmpty(t, a.URL)
			require.NotEmpty(t, a.SHA256)
			require.Positive(t, a.Size, "an mkfs download without a size bound was accepted")
		}
	})
}

func FuzzVerify(f *testing.F) {
	s := fuzzSigner()
	msg := []byte(`{"channel":"stable"}`)
	good := s.sign(msg, "timestamp:1")
	f.Add(msg, good)
	f.Add([]byte("other"), good)
	f.Add(msg, bytes.ReplaceAll(good, []byte("\n"), []byte("\r\n")))
	f.Add(msg, good[:len(good)/2])
	f.Add(msg, bytes.ToUpper(good))
	f.Add(msg, []byte("a\nb\nc\nd\n"))
	f.Add(msg, []byte("untrusted comment: x\nRWQ=\ntrusted comment: y\nAAAA\n"))
	f.Add([]byte{}, []byte{})
	pk, err := ParsePublicKey(s.publicKey())
	require.NoError(f, err)

	f.Fuzz(func(t *testing.T, message, sigFile []byte) {
		if pk.Verify(message, sigFile) != nil {
			return
		}
		// An accepted signature is bound to this exact message: it must not
		// also verify a different one.
		other := append(append([]byte{}, message...), 'x')
		require.Error(t, pk.Verify(other, sigFile))
		if len(message) > 0 {
			flipped := append([]byte{}, message...)
			flipped[0] ^= 0x01
			require.Error(t, pk.Verify(flipped, sigFile))
		}
		// And it is one the trusted key made, not merely well-formed.
		lines := nonEmptyLines(string(sigFile))
		require.GreaterOrEqual(t, len(lines), 4)
		require.True(t, strings.HasPrefix(lines[2], "trusted comment: "))
	})
}

func FuzzParsePublicKey(f *testing.F) {
	s := fuzzSigner()
	f.Add(s.publicKey())
	f.Add("untrusted comment: minisign public key\n" + s.publicKey() + "\n")
	f.Add(PublicKeyB64)
	f.Add("RWQ=")
	f.Add("")
	f.Add("\r\n\r\n")
	f.Add(strings.Repeat("A", 56))
	f.Fuzz(func(t *testing.T, key string) {
		pk, err := ParsePublicKey(key)
		if err != nil {
			require.Nil(t, pk.key, "a key that failed to parse is still usable")
			return
		}
		require.Len(t, pk.key, ed25519.PublicKeySize)
		// Parsing a key never makes it accept an unsigned message.
		require.Error(t, pk.Verify([]byte("m"), nil))
	})
}

func FuzzVersions(f *testing.F) {
	for _, seed := range [][2]string{
		{"v0.4.0", "v0.5.0"}, {"0.4.0", "v0.4.0"}, {"v1.2.3-rc1", "v1.2.3"}, {"dev", "v0.1.0"},
		{"", ""}, {"v", "."}, {"1..2", "1.2."}, {"99999999999999999999.0.0", "1.0.0"},
		{"-1.0.0", "v-1"}, {" v1.0.0 ", "v1.0.0+build"}, {"../x", "v0.2.3/../.."}, {"1.2.3.4", "a.b.c"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		// Ordering must be an ordering: an update prompt that can go both
		// ways would loop.
		require.False(t, IsNewer(a, a))
		require.False(t, IsNewer(a, b) && IsNewer(b, a))
		_ = IsReleaseVersion(a)

		v, err := NormalizeToolsetVersion(a)
		if err != nil {
			require.Error(t, CheckToolsetVersion(a))
			return
		}
		// A normalized version is safe to place in a URL path and a
		// directory name, and normalizing is stable.
		require.True(t, strings.HasPrefix(v, "v"))
		require.NotContains(t, v, "/")
		require.NotContains(t, v, `\`)
		require.NotContains(t, v, "..")
		require.Equal(t, strings.TrimSpace(v), v)
		again, err := NormalizeToolsetVersion(v)
		require.NoError(t, err)
		require.Equal(t, v, again)
	})
}
