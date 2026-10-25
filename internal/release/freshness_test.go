package release

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// rotatingOrigin serves whichever signed stable manifest current points at.
func rotatingOrigin(t *testing.T, s *testSigner) (*Client, *atomic.Value) {
	t.Helper()
	var current atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, _ := current.Load().([]byte)
		switch r.URL.Path {
		case "/channels/stable.json":
			_, _ = w.Write(m)
		case "/channels/stable.json.minisig":
			_, _ = w.Write(s.sign(m, "timestamp:1"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, s.publicKey())
	require.NoError(t, err)
	c.StateDir = t.TempDir()
	return c, &current
}

func stableManifest(generated string) []byte {
	g := ""
	if generated != "" {
		g = fmt.Sprintf(`"generated":%q,`, generated)
	}
	return []byte(`{"channel":"stable",` + g + `"components":{"cli":{"version":"v0.4.0","url":"https://x","sha256":"ab"}}}`)
}

func TestFetchManifestRefusesAnOlderManifest(t *testing.T) {
	c, current := rotatingOrigin(t, newTestSigner(t))
	ctx := context.Background()

	current.Store(stableManifest("2026-10-20T10:00:00Z"))
	_, err := c.FetchManifest(ctx, ChannelStable)
	require.NoError(t, err)

	// The same manifest again, and a newer one, are fine.
	_, err = c.FetchManifest(ctx, ChannelStable)
	require.NoError(t, err)
	current.Store(stableManifest("2026-10-21T10:00:00Z"))
	_, err = c.FetchManifest(ctx, ChannelStable)
	require.NoError(t, err)

	// Replaying the first one is refused, as is one without a time.
	current.Store(stableManifest("2026-10-20T10:00:00Z"))
	_, err = c.FetchManifest(ctx, ChannelStable)
	require.ErrorContains(t, err, "older than one already accepted")
	current.Store(stableManifest(""))
	_, err = c.FetchManifest(ctx, ChannelStable)
	require.ErrorContains(t, err, "no generation time")
}

func TestFetchManifestAcceptsUntimedManifestsUntilATimedOne(t *testing.T) {
	c, current := rotatingOrigin(t, newTestSigner(t))
	current.Store(stableManifest(""))
	_, err := c.FetchManifest(context.Background(), ChannelStable)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(c.StateDir, stateFile))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestFetchManifestRefusesAnInvalidTime(t *testing.T) {
	c, current := rotatingOrigin(t, newTestSigner(t))
	current.Store(stableManifest("yesterday"))
	_, err := c.FetchManifest(context.Background(), ChannelStable)
	require.ErrorContains(t, err, "invalid generation time")
}

func TestFreshnessIsTrackedPerOrigin(t *testing.T) {
	s := newTestSigner(t)
	a, currentA := rotatingOrigin(t, s)
	b, currentB := rotatingOrigin(t, s)
	b.StateDir = a.StateDir
	currentA.Store(stableManifest("2026-10-21T10:00:00Z"))
	currentB.Store(stableManifest("2026-10-01T10:00:00Z"))
	_, err := a.FetchManifest(context.Background(), ChannelStable)
	require.NoError(t, err)
	_, err = b.FetchManifest(context.Background(), ChannelStable)
	require.NoError(t, err)
}
