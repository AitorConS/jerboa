package pkg

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// indexSigner signs documents in the minisign format the index is verified in.
type indexSigner struct {
	keyID [8]byte
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

// useIndexSigner makes fetchIndexRaw trust a fresh key for the test.
func useIndexSigner(t *testing.T) *indexSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s := &indexSigner{pub: pub, priv: priv}
	_, err = rand.Read(s.keyID[:])
	require.NoError(t, err)
	prev := indexPublicKey
	indexPublicKey = base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), s.keyID[:]...), s.pub...))
	t.Cleanup(func() { indexPublicKey = prev })
	return s
}

func (s *indexSigner) sign(msg []byte) []byte {
	sig := ed25519.Sign(s.priv, msg)
	blob := append(append([]byte("Ed"), s.keyID[:]...), sig...)
	const tc = "packages"
	global := ed25519.Sign(s.priv, append(append([]byte{}, sig...), tc...))
	return []byte(fmt.Sprintf("untrusted comment: test\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(blob), tc, base64.StdEncoding.EncodeToString(global)))
}

const testIndex = `{"packages":{"node":[{"name":"node","version":"20.0.0","sha256":"` +
	"0000000000000000000000000000000000000000000000000000000000000000" + `","size":1,"url":"https://example.com/node.tar.gz"}]}}`

// indexServer serves an index and, when sig is not nil, its signature.
func indexServer(t *testing.T, index, sig []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/packages.json":
			_, _ = w.Write(index)
		case r.URL.Path == "/packages.json.minisig" && sig != nil:
			_, _ = w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/packages.json"
}

func withIndexURL(t *testing.T, u string) {
	t.Helper()
	prev := IndexURL
	IndexURL = u
	t.Cleanup(func() { IndexURL = prev })
}

func TestFetchIndexVerifiesSignature(t *testing.T) {
	s := useIndexSigner(t)
	withIndexURL(t, indexServer(t, []byte(testIndex), s.sign([]byte(testIndex))))
	idx, err := FetchIndex()
	require.NoError(t, err)
	require.Len(t, idx.Packages["node"], 1)
}

func TestFetchIndexRefusesABadSignature(t *testing.T) {
	s := useIndexSigner(t)
	other := useIndexSigner(t)
	_ = other // the trusted key is now other's; s signs with a key nobody trusts
	withIndexURL(t, indexServer(t, []byte(testIndex), s.sign([]byte(testIndex))))
	_, err := FetchIndex()
	require.ErrorContains(t, err, "package index signature")

	trusted := useIndexSigner(t)
	tampered := []byte(testIndex[:len(testIndex)-2] + `,"x":[]}}`)
	withIndexURL(t, indexServer(t, tampered, trusted.sign([]byte(testIndex))))
	_, err = FetchIndex()
	require.ErrorContains(t, err, "package index signature")
}

func TestFetchIndexRefusesAnUnsignedRemoteIndex(t *testing.T) {
	useIndexSigner(t)
	// A remote index without a signature: only its host differs from a local
	// one, so check the decision directly rather than through a public server.
	require.False(t, isLocalIndex("https://github.com/AitorConS/jerboa/releases/download/pkg-index/packages.json"))
	require.False(t, isLocalIndex("http://10.0.0.1/packages.json"))
	require.True(t, isLocalIndex("http://127.0.0.1:8080/packages.json"))
	require.True(t, isLocalIndex("file:///srv/packages.json"))

	err := verifyIndex("https://unreachable.invalid/packages.json", []byte(testIndex))
	require.ErrorContains(t, err, "package index signature")
}

func TestFetchIndexAcceptsAnUnsignedLocalIndex(t *testing.T) {
	useIndexSigner(t)
	withIndexURL(t, indexServer(t, []byte(testIndex), nil))
	_, err := FetchIndex()
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "packages.json")
	require.NoError(t, os.WriteFile(path, []byte(testIndex), 0o600))
	withIndexURL(t, "file://"+path)
	idx, err := FetchIndex()
	require.NoError(t, err)
	require.Len(t, idx.Packages["node"], 1)
}

func TestFetchIndexChecksALocalSignatureWhenPresent(t *testing.T) {
	useIndexSigner(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "packages.json")
	require.NoError(t, os.WriteFile(path, []byte(testIndex), 0o600))
	require.NoError(t, os.WriteFile(path+".minisig", []byte("untrusted comment: x\nnot a signature\n"), 0o600))
	withIndexURL(t, "file://"+path)
	_, err := FetchIndex()
	require.ErrorContains(t, err, "package index signature")
}
