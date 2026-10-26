package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/stretchr/testify/require"
)

func TestLocalName(t *testing.T) {
	for in, want := range map[string][2]string{
		"aitorcons/web:v1":         {"aitorcons/web", "v1"},
		"aitorcons/web":            {"aitorcons/web", "latest"},
		"localhost:8788/a/web:dev": {"localhost:8788/a/web", "dev"},
		"web:v2":                   {"web", "v2"},
		"web":                      {"web", "latest"},
	} {
		name, tag, err := localName(in)
		require.NoError(t, err, in)
		require.Equal(t, want, [2]string{name, tag}, in)
	}
	// Registry references are checked when tagging, not first at push.
	for _, in := range []string{"AitorCons/web:v1", "aitorcons/web:-bad", "aitorcons/web@sha256:" + strings.Repeat("0", 64), "a//web"} {
		_, _, err := localName(in)
		require.Error(t, err, in)
	}
}

func TestSplitImageArgKeepsRegistryReferences(t *testing.T) {
	for _, ref := range []string{"aitorcons/web:v1", "localhost:8788/acme/api:dev", "web:latest"} {
		img, path, err := splitImageArg(ref)
		require.NoError(t, err)
		require.Equal(t, ref, img)
		require.Empty(t, path)
	}
	img, path, err := splitImageArg("./disk.img")
	require.NoError(t, err)
	require.Empty(t, img)
	require.Equal(t, "./disk.img", path)
}

func TestGitHubClientIDKeepsTheDefaultRegistrysApp(t *testing.T) {
	ctx := context.Background()
	official := func(context.Context) (string, error) { return "official", nil }
	unreachable := func(context.Context) (string, error) { return "", errors.New("offline") }

	id, err := githubClientID(ctx, naming.DefaultRegistry, client.Discovery{GitHubClientID: "official"}, unreachable)
	require.NoError(t, err)
	require.Equal(t, "official", id)

	id, err = githubClientID(ctx, "registry.example.com", client.Discovery{GitHubClientID: "own"}, official)
	require.NoError(t, err)
	require.Equal(t, "own", id)

	// Another registry may not obtain a token for the default registry's app.
	_, err = githubClientID(ctx, "registry.example.com", client.Discovery{GitHubClientID: "official"}, official)
	require.ErrorContains(t, err, "use --token")
	// Nor log in when that cannot be checked.
	_, err = githubClientID(ctx, "registry.example.com", client.Discovery{GitHubClientID: "own"}, unreachable)
	require.ErrorContains(t, err, "offline")
	_, err = githubClientID(ctx, "registry.example.com", client.Discovery{}, official)
	require.ErrorContains(t, err, "does not offer GitHub login")
}

func TestGitHubLoginUsesTheRegistrysFlowWhenOffered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/device":
			_, _ = w.Write([]byte(`{"login_id":"jdl_x","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":1}`))
		case "/auth/device/token":
			_, _ = w.Write([]byte(`{"token":"jrt_token","login":"alice"}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	unreachable := func(context.Context) (string, error) { return "", errors.New("offline") }

	// The registry runs the flow: no GitHub app is checked, and none is used
	// even if one is advertised too.
	var out strings.Builder
	token, login, err := githubLogin(ctx, &out, "registry.example.com", &client.Client{Base: srv.URL},
		client.Discovery{Login: client.LoginDevice, GitHubClientID: "official"}, unreachable)
	require.NoError(t, err)
	require.Equal(t, [2]string{"jrt_token", "alice"}, [2]string{token, login})
	require.Equal(t, "Open https://github.com/login/device and enter the code ABCD-1234 to log in to registry.example.com\n", out.String())

	// Older registries still go through githubClientID's checks.
	official := func(context.Context) (string, error) { return "official", nil }
	_, _, err = githubLogin(ctx, &out, "registry.example.com", &client.Client{Base: srv.URL}, client.Discovery{GitHubClientID: "official"}, official)
	require.ErrorContains(t, err, "use --token")
	_, _, err = githubLogin(ctx, &out, "registry.example.com", &client.Client{Base: srv.URL}, client.Discovery{}, official)
	require.ErrorContains(t, err, "does not offer GitHub login")
}
