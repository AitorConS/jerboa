package pkg

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"

	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/AitorConS/jerboa/internal/release"
)

// indexPublicKey is the minisign key the package index is verified against:
// the release key, which signs packages.json in the Package Build workflow.
// A var so tests can sign with their own key.
var indexPublicKey = release.PublicKeyB64

// maxIndexSignatureSize bounds the detached signature read into memory.
const maxIndexSignatureSize = 4 << 10

// errIndexNotFound reports that an index URL names nothing (HTTP 404, or no
// such file), as opposed to a transport failure.
var errIndexNotFound = errors.New("not found")

// verifyIndex checks data against the minisign signature published beside the
// index at <indexURL>.minisig. An index served from this machine (file:// or a
// loopback http server: local mirrors and tests) may be unsigned; it is then
// accepted with a warning. Any signature that is present must verify.
func verifyIndex(indexURL string, data []byte) error {
	sig, err := readIndexURL(indexURL+".minisig", maxIndexSignatureSize)
	switch {
	case errors.Is(err, errIndexNotFound) && isLocalIndex(indexURL):
		slog.Warn("package index is not signed; accepting it because it is served from this machine", "url", indexURL)
		return nil
	case err != nil:
		return fmt.Errorf("package index signature: %w", err)
	}
	pk, err := release.ParsePublicKey(indexPublicKey)
	if err != nil {
		return fmt.Errorf("package index signature: %w", err)
	}
	if err := pk.Verify(data, sig); err != nil {
		return fmt.Errorf("package index signature: %w", err)
	}
	return nil
}

// isLocalIndex reports whether an index URL is served from this machine.
func isLocalIndex(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "file":
		return true
	case "http", "https":
		return isLoopbackHost(u.Hostname())
	}
	return false
}

// readIndexURL reads at most limit bytes from an http(s) or file:// URL.
func readIndexURL(raw string, limit int64) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("package index url: %w", err)
	}
	var body io.ReadCloser
	switch u.Scheme {
	case "file":
		f, err := os.Open(u.Path) //nolint:gosec // the index location is the user's configuration
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", raw, errIndexNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("package index: %w", err)
		}
		body = f
	case "http", "https":
		req, err := http.NewRequest(http.MethodGet, raw, nil) //nolint:noctx // callers don't thread ctx yet
		if err != nil {
			return nil, fmt.Errorf("package index request: %w", err)
		}
		resp, err := httpclient.Default.Do(req)
		if err != nil {
			return nil, fmt.Errorf("package index fetch: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("HTTP 404 from %s: %w", u.Redacted(), errIndexNotFound)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("package index: HTTP %d from %s", resp.StatusCode, u.Redacted())
		}
		body = resp.Body
	default:
		return nil, fmt.Errorf("package index url %s: only https and file URLs are accepted", u.Redacted())
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("package index read: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", u.Redacted(), limit)
	}
	return data, nil
}
