package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fakeGitHub(t *testing.T, answers []map[string]any) *httptest.Server {
	t.Helper()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/login/device/code":
			if r.Form.Get("scope") != "read:org" {
				http.Error(w, "missing read:org scope", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(DeviceCode{DeviceCode: "dev", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5})
		case "/login/oauth/access_token":
			if r.Form.Get("device_code") != "dev" {
				http.Error(w, "wrong device code", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(answers[min(polls, len(answers)-1)])
			polls++
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDeviceFlow(t *testing.T) {
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	srv := fakeGitHub(t, []map[string]any{{"error": "authorization_pending"}, {"error": "slow_down", "interval": 10}, {"access_token": "gho_token"}})
	f := DeviceFlow{GitHub: srv.URL, sleep: sleep}
	dc, err := f.Start(context.Background(), "client")
	require.NoError(t, err)
	require.Equal(t, "ABCD-1234", dc.UserCode)
	tok, err := f.Wait(context.Background(), "client", dc)
	require.NoError(t, err)
	require.Equal(t, "gho_token", tok)
	require.Equal(t, []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}, waits)

	for answer, want := range map[string]string{"expired_token": "expired", "access_denied": "denied"} {
		srv := fakeGitHub(t, []map[string]any{{"error": answer}})
		f := DeviceFlow{GitHub: srv.URL, sleep: sleep}
		_, err := f.Wait(context.Background(), "client", dc)
		require.ErrorContains(t, err, want)
	}
}
