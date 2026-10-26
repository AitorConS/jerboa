package httpclient_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/stretchr/testify/require"
)

func TestDefault_HasTimeout(t *testing.T) {
	require.Greater(t, httpclient.Default.Timeout, time.Duration(0),
		"Default client must have a non-zero timeout to avoid hanging on slow servers")
}

func TestDefault_Get_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := httpclient.Default.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDefault_Get_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	resp, err := httpclient.Default.Get(srv.URL)
	require.NoError(t, err, "HTTP 500 is a valid response, not a transport error")
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestDefault_Get_ConnectionRefused(t *testing.T) {
	// Port 1 is reserved and will refuse connections.
	resp, err := httpclient.Default.Get("http://127.0.0.1:1/") //nolint:noctx
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err, "connection refused must return an error")
}

func TestClientsRefuseHTTPSDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain"))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			_, _ = w.Write([]byte("ok"))
		case "/up":
			http.Redirect(w, r, "/same", http.StatusFound)
		default:
			http.Redirect(w, r, plain.URL, http.StatusFound)
		}
	}))
	defer secure.Close()

	for name, c := range map[string]*http.Client{"default": httpclient.Default, "streaming": httpclient.Streaming} {
		t.Run(name, func(t *testing.T) {
			client := secure.Client()
			client.CheckRedirect = c.CheckRedirect
			resp, err := client.Get(secure.URL + "/down")
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.ErrorContains(t, err, "non-https")

			resp, err = client.Get(secure.URL + "/up")
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}
