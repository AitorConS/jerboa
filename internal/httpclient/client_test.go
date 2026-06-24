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
