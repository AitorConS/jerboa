package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestBearer_EmptyTokenStaysOpen(t *testing.T) {
	// With no token configured, an unauthenticated request must pass through.
	h := Bearer("", okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "ok", rr.Body.String())
}

func TestBearer_RejectsMissingOrWrongToken(t *testing.T) {
	h := Bearer("s3cret", okHandler())

	for name, header := range map[string]string{
		"missing":         "",
		"no bearer":       "s3cret",
		"wrong token":     "Bearer nope",
		"wrong length":    "Bearer s3cre",
		"case mismatch":   "Bearer S3CRET",
		"bearer no space": "Bearers3cret",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			require.Equal(t, http.StatusUnauthorized, rr.Code)
			require.Equal(t, "Bearer", rr.Header().Get("WWW-Authenticate"))
		})
	}
}

func TestBearer_AcceptsCorrectToken(t *testing.T) {
	h := Bearer("s3cret", okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "ok", rr.Body.String())
}

func TestBearer_AcceptsCookie(t *testing.T) {
	h := Bearer("s3cret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestLoopbackHost(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := LoopbackHost("127.0.0.1:8080", ok)
	for host, want := range map[string]int{
		"127.0.0.1:8080":        http.StatusOK,
		"localhost:8080":        http.StatusOK,
		"[::1]:8080":            http.StatusOK,
		"attacker.example:8080": http.StatusMisdirectedRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, want, rec.Code, host)
	}
	// An endpoint bound off loopback is protected by its token instead.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "dashboard.example:8080"
	rec := httptest.NewRecorder()
	LoopbackHost(":8080", ok).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
