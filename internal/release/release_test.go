package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSigner is a minisign keypair used to produce valid signatures in tests,
// mirroring the on-disk minisign format the CI signer emits.
type testSigner struct {
	keyID [8]byte
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s := &testSigner{pub: pub, priv: priv}
	_, err = rand.Read(s.keyID[:])
	require.NoError(t, err)
	return s
}

// publicKey returns the base64 minisign public-key payload.
func (s *testSigner) publicKey() string {
	raw := append([]byte("Ed"), s.keyID[:]...)
	raw = append(raw, s.pub...)
	return base64.StdEncoding.EncodeToString(raw)
}

// sign returns a minisign .minisig file (legacy mode) over message.
func (s *testSigner) sign(message []byte, trustedComment string) []byte {
	sig := ed25519.Sign(s.priv, message)
	blob := append([]byte("Ed"), s.keyID[:]...)
	blob = append(blob, sig...)
	global := ed25519.Sign(s.priv, append(append([]byte{}, sig...), []byte(trustedComment)...))
	return []byte(fmt.Sprintf(
		"untrusted comment: test\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(blob),
		trustedComment,
		base64.StdEncoding.EncodeToString(global),
	))
}

func TestParsePublicKeyRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	pk, err := ParsePublicKey(s.publicKey())
	require.NoError(t, err)
	assert.Equal(t, s.keyID, pk.keyID)
	assert.Equal(t, []byte(s.pub), []byte(pk.key))
}

func TestParsePublicKeyWithComment(t *testing.T) {
	s := newTestSigner(t)
	full := "untrusted comment: minisign public key ABC\n" + s.publicKey() + "\n"
	pk, err := ParsePublicKey(full)
	require.NoError(t, err)
	assert.Equal(t, s.keyID, pk.keyID)
}

func TestParsePublicKeyRejectsGarbage(t *testing.T) {
	_, err := ParsePublicKey("not base64 !!!")
	require.Error(t, err)
	_, err = ParsePublicKey("")
	assert.Error(t, err)
}

func TestVerifyValidSignature(t *testing.T) {
	s := newTestSigner(t)
	pk, err := ParsePublicKey(s.publicKey())
	require.NoError(t, err)

	msg := []byte(`{"channel":"stable"}`)
	sig := s.sign(msg, "timestamp:1700000000")
	require.NoError(t, pk.Verify(msg, sig))
}

func TestVerifyRejectsTamperedMessage(t *testing.T) {
	s := newTestSigner(t)
	pk, _ := ParsePublicKey(s.publicKey())
	msg := []byte("original")
	sig := s.sign(msg, "tc")
	assert.Error(t, pk.Verify([]byte("tampered"), sig))
}

func TestVerifyRejectsTamperedTrustedComment(t *testing.T) {
	s := newTestSigner(t)
	pk, _ := ParsePublicKey(s.publicKey())
	msg := []byte("payload")
	sig := s.sign(msg, "tc")
	// Swap the trusted comment; the global signature must catch it.
	tampered := []byte("untrusted comment: test\n" +
		base64line(sig, 1) + "\ntrusted comment: evil\n" + base64line(sig, 3) + "\n")
	assert.Error(t, pk.Verify(msg, tampered))
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	signer := newTestSigner(t)
	other := newTestSigner(t)
	pk, _ := ParsePublicKey(other.publicKey())
	msg := []byte("payload")
	sig := signer.sign(msg, "tc")
	assert.Error(t, pk.Verify(msg, sig))
}

func TestParseManifest(t *testing.T) {
	data := []byte(`{
		"channel": "stable",
		"components": {
			"cli":    {"version":"v0.4.0","url":"https://x/cli","sha256":"abc","size":10},
			"daemon": {"version":"v0.4.0","url":"https://x/d","sha256":"def","min_cli":"v0.3.0","proto":3}
		}
	}`)
	m, err := ParseManifest(data)
	require.NoError(t, err)
	assert.Equal(t, "stable", m.Channel)
	cli, ok := m.Component(ComponentCLI)
	require.True(t, ok)
	assert.Equal(t, "v0.4.0", cli.Version)
	d, _ := m.Component(ComponentDaemon)
	assert.Equal(t, 3, d.Proto)
	assert.Equal(t, "v0.3.0", d.MinCLI)
}

func TestComponentAssetPlatformResolution(t *testing.T) {
	data := []byte(`{
		"channel": "stable",
		"components": {
			"cli": {"version":"v0.4.0","platforms":{
				"windows-amd64":{"url":"https://x/win","sha256":"aa","size":10},
				"linux-amd64":{"url":"https://x/lin","sha256":"bb","size":20}
			}}
		}
	}`)
	m, err := ParseManifest(data)
	require.NoError(t, err)
	cli, _ := m.Component(ComponentCLI)

	win, err := cli.Asset("windows", "amd64")
	require.NoError(t, err)
	assert.Equal(t, "https://x/win", win.URL)

	_, err = cli.Asset("darwin", "arm64")
	assert.Error(t, err) // no such platform
}

func TestComponentAssetInline(t *testing.T) {
	c := Component{Version: "v1", URL: "https://x/k", SHA256: "cc"}
	a, err := c.Asset("linux", "amd64")
	require.NoError(t, err)
	assert.Equal(t, "https://x/k", a.URL)
}

func TestParseManifestRejectsMissingFields(t *testing.T) {
	_, err := ParseManifest([]byte(`{"channel":"stable","components":{"cli":{"version":"v1"}}}`))
	require.Error(t, err) // missing url/sha256
	_, err = ParseManifest([]byte(`{"components":{}}`))
	assert.Error(t, err) // missing channel
}

func TestIsNewer(t *testing.T) {
	assert.True(t, IsNewer("v0.3.0", "v0.4.0"))
	assert.True(t, IsNewer("(unknown)", "v0.1.0"))
	assert.False(t, IsNewer("v0.4.0", "v0.4.0"))
	assert.False(t, IsNewer("v0.5.0", "v0.4.0"))
	assert.True(t, IsNewer("v0.4.0", "v0.4.1"))
}

func TestFetchManifestVerifiesSignature(t *testing.T) {
	s := newTestSigner(t)
	manifest := []byte(`{"channel":"stable","components":{"cli":{"version":"v0.4.0","url":"https://x","sha256":"ab"}}}`)
	sig := s.sign(manifest, "timestamp:1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/channels/stable.json":
			_, _ = w.Write(manifest)
		case "/channels/stable.json.minisig":
			_, _ = w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, s.publicKey())
	require.NoError(t, err)
	m, err := c.FetchManifest(context.Background(), ChannelStable)
	require.NoError(t, err)
	assert.Equal(t, "stable", m.Channel)
}

func TestFetchManifestRejectsOtherChannel(t *testing.T) {
	s := newTestSigner(t)
	manifest := []byte(`{"channel":"beta","components":{"cli":{"version":"v0.5.0-rc1","url":"https://x","sha256":"ab"}}}`)
	sig := s.sign(manifest, "timestamp:1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/channels/stable.json":
			_, _ = w.Write(manifest)
		case "/channels/stable.json.minisig":
			_, _ = w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, s.publicKey())
	require.NoError(t, err)
	_, err = c.FetchManifest(context.Background(), ChannelStable)
	require.ErrorContains(t, err, `signed for channel "beta"`)
}

func TestFetchManifestRejectsBadSignature(t *testing.T) {
	s := newTestSigner(t)
	attacker := newTestSigner(t)
	manifest := []byte(`{"channel":"stable","components":{"cli":{"version":"v9","url":"https://evil","sha256":"ab"}}}`)
	sig := attacker.sign(manifest, "tc") // signed with the wrong key

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channels/stable.json" {
			_, _ = w.Write(manifest)
			return
		}
		_, _ = w.Write(sig)
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, s.publicKey())
	_, err := c.FetchManifest(context.Background(), ChannelStable)
	assert.Error(t, err)
}

func TestDownloadArtifactVerifiesHash(t *testing.T) {
	body := []byte("kernel image bytes")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	s := newTestSigner(t)
	c, _ := NewClient("", s.publicKey())
	c.HTTP = srv.Client()

	dest := filepath.Join(t.TempDir(), "kernel.img")
	asset := Asset{
		URL:    srv.URL + "/kernel.img",
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(body)),
	}
	require.NoError(t, c.DownloadArtifact(context.Background(), asset, dest))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestDownloadArtifactRejectsHashMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("corrupted"))
	}))
	defer srv.Close()

	s := newTestSigner(t)
	c, _ := NewClient("", s.publicKey())
	c.HTTP = srv.Client()

	dest := filepath.Join(t.TempDir(), "artifact")
	asset := Asset{URL: srv.URL + "/x", SHA256: hex.EncodeToString(sha256.New().Sum(nil))}
	err := c.DownloadArtifact(context.Background(), asset, dest)
	require.Error(t, err)
	// The bad download must not be left behind.
	_, statErr := os.Stat(dest)
	assert.True(t, os.IsNotExist(statErr))
	_, tmpErr := os.Stat(dest + ".tmp")
	assert.True(t, os.IsNotExist(tmpErr))
}

// base64line extracts the Nth (0-indexed) line of a signature file, used to
// reassemble a tampered signature in tests.
func base64line(sig []byte, n int) string {
	return nonEmptyLines(string(sig))[n]
}

// TestFetchManifest_Unpublished404 is the F-005 regression: a 404 on the channel
// manifest (channel not published yet) must surface as ErrChannelNotPublished so
// the CLI can distinguish it from a transport failure.
func TestFetchManifest_Unpublished404(t *testing.T) {
	s := newTestSigner(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, s.publicKey())
	_, err := c.FetchManifest(context.Background(), ChannelBeta)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrChannelNotPublished)
}

// TestIsReleaseVersion is the F-001 regression: only real semver builds should be
// treated as comparable release versions; dev/hash builds must not trigger the
// "newer version available" nag.
func TestIsReleaseVersion(t *testing.T) {
	release := map[string]bool{
		"0.51.1":       true,
		"v0.51.1":      true,
		"1.2":          true,
		"1.2.3-beta.1": true,
		"dev":          false,
		"":             false,
		"abc123":       false,
		"v":            false,
		"1":            false,
	}
	for v, want := range release {
		if got := IsReleaseVersion(v); got != want {
			t.Errorf("IsReleaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}
