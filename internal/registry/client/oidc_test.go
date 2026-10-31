package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
)

func TestGitHubActionsOIDCExchange(t *testing.T) {
	// GitHub Actions' token endpoint: the request token as bearer, the audience asked for.
	actions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer request-token" || r.URL.Query().Get("api-version") != "2.0" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"value":"oidc-for-` + r.URL.Query().Get("audience") + `"}`))
	}))
	defer actions.Close()
	t.Setenv(credentials.EnvActionsTokenURL, actions.URL+"/token?api-version=2.0")
	t.Setenv(credentials.EnvActionsToken, "request-token")

	// What the registry received: path, Authorization header and body.
	var got []any
	status := http.StatusOK
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, r.URL.Path, r.Header.Get("Authorization"), body)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"DENIED","message":"no trusted publisher"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"push-token","expires_in":900}`))
	}))
	defer registry.Close()
	c := &client.Client{Base: registry.URL}
	host, _ := url.Parse(registry.URL)
	ctx := context.Background()

	token, err := c.PushToken(ctx, credentials.GitHubActions, "acme", "web")
	require.NoError(t, err)
	require.Equal(t, "push-token", token)
	require.Equal(t, []any{"/auth/oidc", "", map[string]any{"token": "oidc-for-" + host.Host, "scopes": []any{"repository:acme/web:push"}}}, got)

	status = http.StatusForbidden
	_, err = c.DeleteToken(ctx, credentials.GitHubActions, "acme", "web")
	require.ErrorContains(t, err, "Trusted publishers")
	status = http.StatusNotFound
	_, err = c.PushToken(ctx, credentials.GitHubActions, "acme", "web")
	require.ErrorContains(t, err, credentials.EnvToken)

	// Personal access tokens need a login.
	_, _, err = c.CreatePAT(ctx, credentials.GitHubActions, client.PATOptions{Name: "ci"})
	require.ErrorContains(t, err, "jerboa login")
	_, err = c.ListPATs(ctx, credentials.GitHubActions)
	require.ErrorContains(t, err, "jerboa login")

	// GitHub refusing the request token, and a request URL that is not https.
	t.Setenv(credentials.EnvActionsToken, "wrong")
	_, err = c.PushToken(ctx, credentials.GitHubActions, "acme", "web")
	require.ErrorContains(t, err, "401")
	t.Setenv(credentials.EnvActionsTokenURL, "http://actions.example/token")
	_, err = c.PushToken(ctx, credentials.GitHubActions, "acme", "web")
	require.ErrorContains(t, err, "not an https URL")
}
