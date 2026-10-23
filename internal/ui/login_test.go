//go:build linux || (darwin && arm64)

package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
	require.Equal(t, httpauth.CookieName, cookies[0].Name)
	require.True(t, cookies[0].HttpOnly)

	req := httptest.NewRequest(http.MethodGet, "/ui/api/vms", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
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
