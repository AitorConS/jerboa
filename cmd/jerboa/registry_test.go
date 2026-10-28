package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
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

func TestRepoRmDeletesATagOrTheRepository(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/auth/token":
			require.Equal(t, "Bearer jpat_ci", r.Header.Get("Authorization"))
			body, _ := io.ReadAll(r.Body)
			require.JSONEq(t, `{"scopes":["repository:acme/web:push,delete"]}`, string(body))
			_, _ = w.Write([]byte(`{"token":"jwt_delete"}`))
		case r.Method == http.MethodDelete:
			require.Equal(t, "Bearer jwt_delete", r.Header.Get("Authorization"))
			calls = append(calls, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv(credentials.EnvToken, "jpat_ci")
	t.Setenv(credentials.EnvHost, host)
	orig := os.Stdin
	t.Cleanup(func() { os.Stdin = orig })
	answer := func(text string) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		_, err = w.WriteString(text)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		os.Stdin = r
	}
	run := func(args ...string) (string, error) {
		cmd := newRegistryCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"repo", "rm"}, args...))
		err := cmd.Execute()
		return out.String(), err
	}

	out, err := run(host + "/acme/web:v1")
	require.NoError(t, err)
	require.Equal(t, "Deleted acme/web:v1\n", out)
	require.Equal(t, []string{"/v2/acme/web/tags/v1"}, calls)

	// A whole repository needs a yes, typed or given with --yes; anything
	// else, including no answer at all, deletes nothing.
	calls = nil
	for _, text := range []string{"", "n\n"} {
		answer(text)
		out, err = run(host + "/acme/web")
		require.NoError(t, err)
		require.Equal(t, "Aborted.\n", out)
		require.Empty(t, calls)
	}
	answer("y\n")
	out, err = run(host + "/acme/web")
	require.NoError(t, err)
	require.Equal(t, "Deleted acme/web\n", out)
	require.Equal(t, []string{"/v2/acme/web"}, calls)

	calls = nil
	out, err = run(host+"/acme/web", "--yes")
	require.NoError(t, err)
	require.Equal(t, "Deleted acme/web\n", out)
	require.Equal(t, []string{"/v2/acme/web"}, calls)

	_, err = run(host + "/acme/web@sha256:" + strings.Repeat("a", 64))
	require.ErrorContains(t, err, "not a digest")
}
