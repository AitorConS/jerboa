package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/upgradetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toServer sends every request to one test server, whatever host it names,
// so a client keeps the production origin (which keys its state) in tests.
type toServer struct{ target *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.target.Scheme, t.target.Host
	return http.DefaultTransport.RoundTrip(r) //nolint:wrapcheck // test transport
}

// TestReleaseStateOfAReleasedVersion: the record of the newest stable
// manifest a released version accepted still binds this one. A machine that
// upgrades keeps refusing the manifests it had already moved past, and
// accepts the one it was on and newer ones.
func TestReleaseStateOfAReleasedVersion(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		data, err := os.ReadFile(filepath.Join(f.Data(), stateFile))
		require.NoError(t, err)
		var recorded map[string]string
		require.NoError(t, json.Unmarshal(data, &recorded))
		last := recorded[DefaultBaseURL+" "+ChannelStable]
		require.NotEmpty(t, last, "the released version recorded the stable manifest it accepted")
		accepted, err := time.Parse(time.RFC3339, last)
		require.NoError(t, err)

		signer := newTestSigner(t)
		var manifest []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/channels/stable.json":
				_, _ = w.Write(manifest)
			case "/channels/stable.json.minisig":
				_, _ = w.Write(signer.sign(manifest, "release:test"))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		target, err := url.Parse(srv.URL)
		require.NoError(t, err)
		c, err := NewClient("", signer.publicKey())
		require.NoError(t, err)
		c.HTTP = &http.Client{Transport: toServer{target}}
		c.StateDir = f.Data()
		ctx := context.Background()

		manifest = stableManifest(accepted.Add(-time.Second).UTC().Format(time.RFC3339))
		_, err = c.FetchManifest(ctx, ChannelStable)
		require.ErrorContains(t, err, "older than one already accepted")
		manifest = stableManifest("")
		_, err = c.FetchManifest(ctx, ChannelStable)
		require.ErrorContains(t, err, "no generation time")
		// A refusal leaves the record alone.
		after, err := os.ReadFile(filepath.Join(f.Data(), stateFile))
		require.NoError(t, err)
		assert.Equal(t, data, after)

		manifest = stableManifest(last)
		_, err = c.FetchManifest(ctx, ChannelStable)
		require.NoError(t, err)
		newer := accepted.Add(time.Hour).UTC().Format(time.RFC3339)
		manifest = stableManifest(newer)
		_, err = c.FetchManifest(ctx, ChannelStable)
		require.NoError(t, err)
		seen, err := readState(filepath.Join(f.Data(), stateFile))
		require.NoError(t, err)
		assert.Equal(t, newer, seen[DefaultBaseURL+" "+ChannelStable])

	})
}
