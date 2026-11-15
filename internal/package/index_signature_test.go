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
	"sync"
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
	return s.signComment(msg, "packages")
}

// signComment signs msg with tc as the trusted comment.
func (s *indexSigner) signComment(msg []byte, tc string) []byte {
	sig := ed25519.Sign(s.priv, msg)
	blob := append(append([]byte("Ed"), s.keyID[:]...), sig...)
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

func TestFetchIndexRefusesASignatureMadeForAnotherDocument(t *testing.T) {
	s := useIndexSigner(t)
	for comment, ok := range map[string]bool{
		"packages":            true,
		"packages:123":        true,
		"stable:v1.0.0":       false,
		"toolset:kernel":      false,
		"packagesX":           false,
		"timestamp:1 file:pk": false,
	} {
		t.Run(comment, func(t *testing.T) {
			withIndexURL(t, indexServer(t, []byte(testIndex), s.signComment([]byte(testIndex), comment)))
			_, err := FetchIndex()
			if ok {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "not for the package index")
			}
		})
	}
}

// serialIndex is a signed-format index with the given serial.
func serialIndex(serial int, version string) []byte {
	return []byte(fmt.Sprintf(`{"purpose":"jerboa-package-index","serial":%d,"generated":"2026-11-01T00:00:00Z",`+
		`"packages":{"node":[{"name":"node","version":%q,"sha256":"%064d","size":1,"url":"https://example.com/node.tar.gz"}]}}`,
		serial, version, 0))
}

// switchableIndex serves whichever signed index was set last.
type switchableIndex struct {
	mu         sync.Mutex
	index, sig []byte
}

func (x *switchableIndex) set(index, sig []byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.index, x.sig = index, sig
}

func (x *switchableIndex) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		switch r.URL.Path {
		case "/packages.json":
			_, _ = w.Write(x.index)
		case "/packages.json.minisig":
			_, _ = w.Write(x.sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/packages.json"
}

func TestFetchIndexCachedRefusesAnOlderIndex(t *testing.T) {
	s := useIndexSigner(t)
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	var x switchableIndex
	withIndexURL(t, x.serve(t))
	serve := func(doc []byte) { x.set(doc, s.sign(doc)) }

	serve(serialIndex(5, "22.0.0"))
	idx, err := store.FetchIndexCached()
	require.NoError(t, err)
	require.Equal(t, "22.0.0", idx.Packages["node"][0].Version)

	// A superseded index with its own valid signature is refused, and the
	// cached newer index is used instead.
	serve(serialIndex(4, "20.0.0"))
	_, _, err = fetchIndexRaw(store.root)
	require.ErrorContains(t, err, "older than one already accepted")
	idx, err = store.FetchIndexCached()
	require.NoError(t, err)
	require.Equal(t, "22.0.0", idx.Packages["node"][0].Version, "the cached newer index is kept")

	// So is an index without a serial, once one with a serial was accepted.
	serve([]byte(testIndex))
	_, _, err = fetchIndexRaw(store.root)
	require.ErrorContains(t, err, "has no serial")

	// Without local state (FetchIndex) only the index's own fields count.
	_, err = FetchIndex()
	require.NoError(t, err)

	// The same serial is accepted; a newer one raises the floor.
	serve(serialIndex(5, "22.0.0"))
	_, err = store.FetchIndexCached()
	require.NoError(t, err)
	serve(serialIndex(6, "23.0.0"))
	_, err = store.FetchIndexCached()
	require.NoError(t, err)
	serve(serialIndex(5, "22.0.0"))
	_, _, err = fetchIndexRaw(store.root)
	require.ErrorContains(t, err, "older than one already accepted")
}

func TestFetchIndexRefusesAWrongPurpose(t *testing.T) {
	s := useIndexSigner(t)
	for _, doc := range []string{
		`{"purpose":"jerboa-release-manifest","serial":3,"packages":{}}`,
		`{"serial":3,"packages":{}}`,
		`{"purpose":"jerboa-package-index","packages":{}}`,
		`{"purpose":"jerboa-package-index","serial":3,"generated":"yesterday","packages":{}}`,
	} {
		withIndexURL(t, indexServer(t, []byte(doc), s.sign([]byte(doc))))
		_, err := FetchIndex()
		require.Error(t, err, doc)
	}
}
