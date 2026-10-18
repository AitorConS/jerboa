// Package release is the single, shared path for discovering and downloading
// Jerboa's distributable components (kernel, daemon, CLI, distro, desktop).
//
// It replaces the four ad-hoc downloaders scattered across the tree with one
// manifest-driven model: a minisign-signed channel manifest is the source of
// truth, and every artifact is verified by the SHA-256 recorded in that signed
// manifest. Because the manifest's signature transitively covers each artifact
// hash, individual artifacts need no separate signature and can be streamed and
// verified without buffering large files in memory.
package release

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/AitorConS/jerboa/internal/httpclient"
)

// ErrChannelNotPublished indicates the requested release channel's manifest does
// not exist yet (HTTP 404) — as opposed to a transport failure. Callers use it to
// tell "the beta channel isn't published" apart from "the network is down" so the
// message is not misleadingly framed as a fetch error (E2E finding F-005).
var ErrChannelNotPublished = errors.New("release channel is not published")

// DefaultBaseURL is the public read origin backed by the R2 bucket.
const DefaultBaseURL = "https://releases.jerboa.dev"

// StallTimeout is how long an artifact download may make no progress before it
// is aborted. It bounds a dead connection without capping a healthy transfer's
// total duration. A var so tests can shrink it; defaults to the shared value.
var StallTimeout = httpclient.DefaultStallTimeout

// Channel names.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

// Client fetches and verifies release manifests and artifacts.
type Client struct {
	// BaseURL is the release origin. Empty means DefaultBaseURL.
	BaseURL string
	// Pub is the minisign public key the manifest is verified against.
	Pub PublicKey
	// HTTP is the client used for downloads. Nil means httpclient.Default.
	HTTP *http.Client
}

// NewClient builds a Client from a base URL and an embedded minisign public key.
// An empty baseURL falls back to DefaultBaseURL.
func NewClient(baseURL, publicKey string) (*Client, error) {
	pk, err := ParsePublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Pub: pk}, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return httpclient.Default
}

// streamingClient is the client used for large artifact bodies. It has no total
// deadline (which would abort a big but healthy download); liveness is enforced
// by the stall watchdog in DownloadArtifact instead. Tests may still override
// c.HTTP to inject a client, in which case it is honored for streaming too.
func (c *Client) streamingClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return httpclient.Streaming
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// FetchManifest downloads channel/<channel>.json and its .minisig, verifies the
// signature against the embedded public key, and returns the parsed manifest.
func (c *Client) FetchManifest(ctx context.Context, channel string) (*Manifest, error) {
	manifestURL := c.base() + "/channels/" + channel + ".json"
	sigURL := manifestURL + ".minisig"

	manifestBytes, err := c.get(ctx, manifestURL)
	if err != nil {
		return nil, fmt.Errorf("release: fetch manifest: %w", err)
	}
	sigBytes, err := c.get(ctx, sigURL)
	if err != nil {
		return nil, fmt.Errorf("release: fetch manifest signature: %w", err)
	}
	if err := c.Pub.Verify(manifestBytes, sigBytes); err != nil {
		return nil, err
	}
	return ParseManifest(manifestBytes)
}

// DownloadArtifact streams a.URL to dest, verifying length and SHA-256 against
// the (already signature-verified) manifest asset. The write is atomic: it lands
// in dest.partial and is renamed into place only after verification, so a crash
// or hash mismatch never leaves a partial artifact at dest. An interrupted
// download resumes from dest.partial on the next call.
func (c *Client) DownloadArtifact(ctx context.Context, a Asset, dest string) error {
	if a.SHA256 == "" {
		return fmt.Errorf("release: refusing to download %s with no sha256", a.URL)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("release: create dest dir: %w", err)
	}

	// A large artifact is streamed with no total deadline: a healthy transfer of
	// any size must complete. The stall watchdog aborts only a dead connection.
	// MaxSize caps the stream so a misbehaving/compromised origin cannot write
	// an unbounded amount to disk before the hash check fails.
	partial := dest + httpclient.PartialSuffix
	dl := httpclient.Resumable{Client: c.streamingClient(), StallTimeout: StallTimeout, MaxSize: a.Size}
	written, sum, err := dl.Fetch(ctx, a.URL, partial)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}

	if a.Size > 0 && written != a.Size {
		httpclient.DiscardPartial(partial)
		return fmt.Errorf("release: %s size mismatch: got %d, want %d", a.URL, written, a.Size)
	}
	got := hex.EncodeToString(sum)
	want := strings.ToLower(strings.TrimSpace(a.SHA256))
	if got != want {
		httpclient.DiscardPartial(partial)
		return fmt.Errorf("release: %s sha256 mismatch: got %s, want %s", a.URL, got, want)
	}

	if err := httpclient.CommitPartial(partial, dest); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// get performs a GET and returns the body, capping the response size so a
// misbehaving origin cannot exhaust memory on a small metadata fetch.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("release: build request: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: HTTP 404 from %s", ErrChannelNotPublished, url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	const maxMeta = 1 << 20 // 1 MiB is plenty for a manifest or signature.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMeta))
	if err != nil {
		return nil, fmt.Errorf("release: read %s: %w", url, err)
	}
	return body, nil
}
