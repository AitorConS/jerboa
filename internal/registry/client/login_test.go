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

// fakeLoginRegistry serves a registry-run sign-in: polls get the answers in
// turn, each a status and a JSON body, the last one repeated.
func fakeLoginRegistry(t *testing.T, answers []struct {
	status int
	body   string
}) *httptest.Server {
	t.Helper()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/auth/device":
			_ = json.NewEncoder(w).Encode(DeviceLogin{LoginID: "jdl_x", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5})
		case r.Method == http.MethodPost && r.URL.Path == "/auth/device/token":
			var body struct {
				LoginID string `json:"login_id"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.LoginID != "jdl_x" {
				http.Error(w, `{"error":{"code":"BAD_REQUEST","message":"login_id is required"}}`, http.StatusBadRequest)
				return
			}
			a := answers[min(polls, len(answers)-1)]
			polls++
			w.WriteHeader(a.status)
			_, _ = w.Write([]byte(a.body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type answer = struct {
	status int
	body   string
}

func TestRegistryLogin(t *testing.T) {
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	srv := fakeLoginRegistry(t, []answer{
		{http.StatusAccepted, `{"status":"pending","interval":5}`},
		{http.StatusAccepted, `{"status":"pending","interval":10}`},
		{http.StatusOK, `{"token":"jrt_token","login":"alice"}`},
	})
	c := &Client{Base: srv.URL, sleep: sleep}
	l, err := c.StartLogin(context.Background())
	require.NoError(t, err)
	require.Equal(t, DeviceLogin{LoginID: "jdl_x", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5}, l)
	token, login, err := c.WaitLogin(context.Background(), l)
	require.NoError(t, err)
	require.Equal(t, [2]string{"jrt_token", "alice"}, [2]string{token, login})
	require.Equal(t, []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}, waits)
}

func TestRegistryLoginEnds(t *testing.T) {
	sleep := func(context.Context, time.Duration) error { return nil }
	for status, want := range map[int]string{
		http.StatusForbidden:  "denied",
		http.StatusGone:       "expired",
		http.StatusNotFound:   "no longer knows this login",
		http.StatusBadRequest: "BAD_REQUEST",
	} {
		srv := fakeLoginRegistry(t, []answer{{status, `{"error":{"code":"BAD_REQUEST","message":"no"}}`}})
		c := &Client{Base: srv.URL, sleep: sleep}
		_, _, err := c.WaitLogin(context.Background(), DeviceLogin{LoginID: "jdl_x", ExpiresIn: 900, Interval: 5})
		require.ErrorContains(t, err, want, status)
	}

	// Canceling stops the wait.
	srv := fakeLoginRegistry(t, []answer{{http.StatusAccepted, `{"status":"pending","interval":1}`}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := (&Client{Base: srv.URL}).WaitLogin(ctx, DeviceLogin{LoginID: "jdl_x", ExpiresIn: 900, Interval: 1})
	require.ErrorIs(t, err, context.Canceled)

}
