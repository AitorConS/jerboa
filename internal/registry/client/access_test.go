package client

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/registry/credentials"
)

type recordingTransport struct{ seen []*http.Request }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = append(r.seen, req)
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
}

func TestAccessTokenOnlyReachesItsRegistry(t *testing.T) {
	t.Setenv(credentials.EnvAccessHost, "staging.registry.example")
	t.Setenv(credentials.EnvAccessClientID, "id.access")
	t.Setenv(credentials.EnvAccessClientSecret, "s3cret")
	rec := &recordingTransport{}
	c := New("staging.registry.example")
	c.HTTP = &http.Client{Transport: rec}

	for _, tc := range []struct {
		url  string
		sent bool
	}{
		{"https://staging.registry.example/v2/", true},
		{"https://cdn.staging.registry.example/packs/abc", true},
		{"https://STAGING.registry.example:443/v2/", true},
		{"http://staging.registry.example/v2/", false},
		{"https://registry.example/v2/", false},
		{"https://evilstaging.registry.example/v2/", false},
		{"https://staging.registry.example.evil.test/v2/", false},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		require.NoError(t, err)
		// A redirect carries the previous request's headers along.
		req.Header.Set(accessClientSecretHeader, "from-a-redirect")
		resp, err := c.http().Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		got := rec.seen[len(rec.seen)-1].Header
		if tc.sent {
			require.Equal(t, "id.access", got.Get(accessClientIDHeader), tc.url)
			require.Equal(t, "s3cret", got.Get(accessClientSecretHeader), tc.url)
		} else {
			require.Empty(t, got.Get(accessClientIDHeader), tc.url)
			require.Empty(t, got.Get(accessClientSecretHeader), tc.url)
		}
		require.Equal(t, "from-a-redirect", req.Header.Get(accessClientSecretHeader), "the caller's request is not modified")
	}
}

func TestNoAccessTokenLeavesTheClientAlone(t *testing.T) {
	t.Setenv(credentials.EnvAccessHost, "staging.registry.example")
	t.Setenv(credentials.EnvAccessClientID, "")
	t.Setenv(credentials.EnvAccessClientSecret, "s3cret")
	hc := &http.Client{}
	c := New("staging.registry.example")
	c.HTTP = hc
	require.Same(t, hc, c.http())
}
