package release

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolsetServer(t *testing.T, body, sig []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kernel/v0.2.3/toolset.json":
			_, _ = w.Write(body)
		case "/kernel/v0.2.3/toolset.json.minisig":
			_, _ = w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchToolset(t *testing.T) {
	s := newTestSigner(t)
	body := []byte(`{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"https://x/mkfs-linux-amd64","sha256":"ab","size":3},` +
		`"darwin-arm64":{"url":"https://x/mkfs-darwin-arm64","sha256":"cd","size":4}}}`)
	srv := toolsetServer(t, body, s.sign(body, "toolset"))
	c, err := NewClient(srv.URL, s.publicKey())
	require.NoError(t, err)

	// Versions are accepted with or without the leading v.
	ts, err := c.FetchToolset(context.Background(), "0.2.3")
	require.NoError(t, err)
	a, err := ts.MkfsAsset("darwin", "arm64")
	require.NoError(t, err)
	assert.Equal(t, "https://x/mkfs-darwin-arm64", a.URL)
	_, err = ts.MkfsAsset("linux", "arm64")
	require.ErrorContains(t, err, "no mkfs for linux-arm64")
}

func TestFetchToolsetRejects(t *testing.T) {
	s := newTestSigner(t)
	attacker := newTestSigner(t)
	valid := []byte(`{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":1}}}`)
	cases := map[string]struct {
		body []byte
		sig  []byte
		want string
	}{
		"wrong key":      {valid, attacker.sign(valid, "x"), "signature"},
		"other version":  {[]byte(`{"version":"v0.2.2","mkfs":{"linux-amd64":{"url":"u","sha256":"ab","size":1}}}`), nil, "want \"v0.2.3\""},
		"no mkfs":        {[]byte(`{"version":"v0.2.3","mkfs":{}}`), nil, "has no mkfs"},
		"missing size":   {[]byte(`{"version":"v0.2.3","mkfs":{"linux-amd64":{"url":"u","sha256":"ab"}}}`), nil, "url/sha256/size"},
		"unknown fields": {[]byte(`{"version":"v0.2.3","mkfs":{},"extra":1}`), nil, "parse toolset"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sig := tc.sig
			if sig == nil {
				sig = s.sign(tc.body, "x")
			}
			srv := toolsetServer(t, tc.body, sig)
			c, err := NewClient(srv.URL, s.publicKey())
			require.NoError(t, err)
			_, err = c.FetchToolset(context.Background(), "v0.2.3")
			require.Error(t, err)
			assert.Contains(t, fmt.Sprint(err), tc.want)
		})
	}
}

func TestNormalizeToolsetVersion(t *testing.T) {
	for in, want := range map[string]string{"0.2.3": "v0.2.3", "v0.2.3": "v0.2.3", "v1.0.0-rc.1": "v1.0.0-rc.1"} {
		got, err := NormalizeToolsetVersion(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	for _, bad := range []string{"", "latest", "../v0.2.3", "v0.2", "v0.2.3/../x"} {
		_, err := NormalizeToolsetVersion(bad)
		assert.Error(t, err, bad)
	}
}

func TestCheckToolsetVersionRefusesRevoked(t *testing.T) {
	revoked := map[string]string{"v0.1.0": "mkfs parser flaw"}
	require.ErrorContains(t, checkToolsetVersion("0.1.0", revoked), "revoked")
	require.NoError(t, checkToolsetVersion("v0.2.0", revoked))
	require.Error(t, checkToolsetVersion("latest", revoked))
	require.NoError(t, CheckToolsetVersion("v0.2.2"))
}
