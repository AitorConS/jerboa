//go:build linux || (darwin && arm64)

package ui

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/httpauth"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

func TestServer_BrowserIsSentToSignIn(t *testing.T) {
	h := NewServer(":8080", "s3cret", vm.NewMockManager(), "test")

	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/ui/login", rec.Header().Get("Location"))

	api := httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, api)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServer_SignInSetsCookie(t *testing.T) {
	h := NewServer(":8080", "s3cret", vm.NewMockManager(), "test")
	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusUnauthorized, post("wrong").Code)

	rec := post("s3cret")
	require.Equal(t, http.StatusSeeOther, rec.Code)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, "jerboa_ui_8080", cookies[0].Name, "the cookie name carries the dashboard's port")
	require.True(t, cookies[0].HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)
	require.False(t, cookies[0].Secure, "plain HTTP on loopback never sends a Secure cookie")
	require.Positive(t, cookies[0].MaxAge)
	require.NotContains(t, cookies[0].Value, "s3cret", "the cookie must not hold the token")

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// What another listener on the host receives in the cookie is no bearer
	// token for the dashboard or /metrics.
	leaked := &http.Request{Header: http.Header{"Authorization": {"Bearer " + cookies[0].Value}}}
	require.False(t, httpauth.Authorized("s3cret", leaked))
	req = httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
	req.Header.Set("Authorization", "Bearer "+cookies[0].Value)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServer_RawTokenCookieIsRefused(t *testing.T) {
	h := NewServer(":8080", "s3cret", vm.NewMockManager(), "test")
	for _, name := range []string{"jerboa_observability", "jerboa_ui_8080"} {
		req := httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
		req.AddCookie(&http.Cookie{Name: name, Value: "s3cret"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, name)
	}
}

func TestSessions_ForgedOrExpiredCookieIsRefused(t *testing.T) {
	s := newSessions("127.0.0.1:8080")
	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodPost, "/ui/login", nil)
	rec := httptest.NewRecorder()
	s.issue(rec, req)
	cookie := rec.Result().Cookies()[0]

	check := func(c *http.Cookie) bool {
		r := httptest.NewRequest(http.MethodGet, "/ui", nil)
		r.AddCookie(c)
		return s.valid(r)
	}
	require.True(t, check(cookie))

	exp, sig, _ := strings.Cut(cookie.Value, ".")
	require.False(t, check(&http.Cookie{Name: cookie.Name, Value: "9999999999." + sig}), "a later expiry needs a new MAC")
	require.False(t, check(&http.Cookie{Name: cookie.Name, Value: exp + ".AAAA"}))
	require.False(t, check(&http.Cookie{Name: "jerboa_ui_9090", Value: cookie.Value}), "a cookie for another port")
	require.False(t, newSessions("127.0.0.1:8080").valid(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/ui", nil)
		r.AddCookie(cookie)
		return r
	}()), "a restarted daemon has a new key")

	now = now.Add(sessionTTL)
	require.False(t, check(cookie), "the session expires")
}

func TestSessions_TLSUsesHostPrefix(t *testing.T) {
	s := newSessions("0.0.0.0:8443")
	req := httptest.NewRequest(http.MethodPost, "https://dash.example:8443/ui/login", nil)
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	s.issue(rec, req)
	c := rec.Result().Cookies()[0]
	require.Equal(t, "__Host-jerboa_ui_8443", c.Name)
	require.True(t, c.Secure)
	require.Equal(t, "/", c.Path)
	require.Empty(t, c.Domain)

	r := httptest.NewRequest(http.MethodGet, "https://dash.example:8443/ui", nil)
	r.TLS = &tls.ConnectionState{}
	r.AddCookie(c)
	require.True(t, s.valid(r))
}

func TestServer_LoopbackRejectsForeignHost(t *testing.T) {
	h := NewServer("127.0.0.1:8080", "", vm.NewMockManager(), "test")
	req := httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
	req.Host = "attacker.example:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusMisdirectedRequest, rec.Code)
}

func TestMaskEnv(t *testing.T) {
	require.Equal(t, []string{"DB_PASSWORD=****", "EMPTY=****"}, maskEnv([]string{"DB_PASSWORD=hunter2", "EMPTY="}))
	require.Nil(t, maskEnv(nil))
}
