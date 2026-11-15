package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/registrytest"
)

func TestEditRepoSendsOnlyTheFieldsSet(t *testing.T) {
	srv := registrytest.New()
	defer srv.Close()
	c := client.New(srv.Host())

	description := "Static web server"
	require.NoError(t, c.EditRepo(context.Background(), "push-token", "aitorcons", "web", client.RepoChanges{Description: &description}))
	require.Equal(t, map[string]string{"description": description}, srv.Repo("aitorcons", "web"))

	readme := "# web\n"
	require.NoError(t, c.EditRepo(context.Background(), "push-token", "aitorcons", "web", client.RepoChanges{Readme: &readme}))
	require.Equal(t, map[string]string{"description": description, "readme": readme}, srv.Repo("aitorcons", "web"))

	require.Error(t, c.EditRepo(context.Background(), "", "aitorcons", "web", client.RepoChanges{Readme: &readme}))
}

func TestDeleteRepo(t *testing.T) {
	srv := registrytest.New()
	defer srv.Close()
	c := client.New(srv.Host())
	ctx := context.Background()

	description := "Static web server"
	require.NoError(t, c.EditRepo(ctx, "push-token", "aitorcons", "web", client.RepoChanges{Description: &description}))
	require.Error(t, c.DeleteRepo(ctx, "", "aitorcons", "web"))
	require.NoError(t, c.DeleteRepo(ctx, "push-token", "aitorcons", "web"))
	require.Nil(t, srv.Repo("aitorcons", "web"))

	var re *client.Error
	require.ErrorAs(t, c.DeleteRepo(ctx, "push-token", "aitorcons", "web"), &re)
	require.Equal(t, http.StatusNotFound, re.Status)
}

func TestMissingRecipeAndTagSayNotFound(t *testing.T) {
	srv := registrytest.New()
	defer srv.Close()
	c := client.New(srv.Host())
	ctx := context.Background()
	cdn := srv.URL + "/cdn"

	digest := "sha256:" + strings.Repeat("0", 64)
	_, err := c.Recipe(ctx, cdn, digest)
	require.EqualError(t, err, "recipe "+digest+" not found in the registry")
	_, err = c.Tag(ctx, cdn, "aitorcons", "web", "nope")
	require.EqualError(t, err, "aitorcons/web:nope not found in the registry")
}

func TestTagRejectsInvalidDigests(t *testing.T) {
	srv := registrytest.New()
	defer srv.Close()
	srv.SetObject("tags/alice/web/v1", []byte(`{"recipe":"../../etc"}`))
	srv.SetObject("tags/alice/web/v2", []byte(`{"recipe":"sha256:`+strings.Repeat("0", 64)+`","index":"../../etc"}`))
	c := client.New(srv.Host())
	_, err := c.Tag(context.Background(), srv.URL+"/cdn", "alice", "web", "v1")
	require.ErrorContains(t, err, "invalid recipe")
	_, err = c.Tag(context.Background(), srv.URL+"/cdn", "alice", "web", "v2")
	require.ErrorContains(t, err, "invalid index")
}

func TestIndexChecksItsDigest(t *testing.T) {
	srv := registrytest.New()
	defer srv.Close()
	digest := "sha256:" + strings.Repeat("1", 64)
	srv.SetObject("indexes/"+strings.Repeat("1", 64)+".json", []byte("{}"))
	c := client.New(srv.Host())
	_, err := c.Index(context.Background(), srv.URL+"/cdn", digest)
	require.ErrorContains(t, err, "does not match its digest")
	_, err = c.Index(context.Background(), srv.URL+"/cdn", "sha256:"+strings.Repeat("2", 64))
	require.ErrorContains(t, err, "not found in the registry")
}

func TestReadRangeRefusesOutOfBoundsRanges(t *testing.T) {
	srv := registrytest.New()
	defer srv.Close()
	c := client.New(srv.Host())
	p := "sha256:" + strings.Repeat("a", 64)
	for _, rg := range [][2]int64{{0, 0}, {-1, 10}, {0, 1 << 40}, {1 << 40, 1}} {
		_, err := c.ReadRange(context.Background(), srv.URL+"/cdn", p, rg[0], rg[1])
		require.ErrorContains(t, err, "outside a pack", rg)
	}
	_, err := c.ReadRange(context.Background(), srv.URL+"/cdn", "../x", 0, 1)
	require.ErrorContains(t, err, "invalid pack digest")
}

func TestReadRangeRetriesACutOrFailedRead(t *testing.T) {
	p := "sha256:" + strings.Repeat("a", 64)
	for name, first := range map[string]func(http.ResponseWriter){
		"server error": func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
		"cut short": func(w http.ResponseWriter) {
			// Promise four bytes, send two, and drop the connection.
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("ab"))
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			var ranges []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				ranges = append(ranges, r.Header.Get("Range"))
				if requests == 1 {
					first(w)
					return
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("abcd"))
			}))
			defer srv.Close()
			data, err := client.New(srv.URL).ReadRange(context.Background(), srv.URL, p, 8, 4)
			require.NoError(t, err)
			require.Equal(t, "abcd", string(data))
			require.Equal(t, []string{"bytes=8-11", "bytes=8-11"}, ranges)
		})
	}
}

func TestReadRangeDoesNotRetryARefusal(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := client.New(srv.URL).ReadRange(context.Background(), srv.URL, "sha256:"+strings.Repeat("a", 64), 0, 4)
	require.ErrorContains(t, err, "HTTP 404")
	require.Equal(t, 1, requests)
}

func TestTokenScopesAndPATOptions(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Path+" "+string(b))
		if strings.Contains(string(b), "delete") && r.URL.Path == "/auth/token" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"DENIED","message":"no"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"x","id":"1"}`))
	}))
	defer srv.Close()
	c := &client.Client{Base: srv.URL}
	ctx := context.Background()

	_, err := c.PushToken(ctx, "jrt", "acme", "web")
	require.NoError(t, err)
	_, err = c.DeleteToken(ctx, "jrt", "acme", "web")
	require.ErrorContains(t, err, "organization admin")
	_, _, err = c.CreatePAT(ctx, "jrt", client.PATOptions{Name: "ci", ExpiresInDays: 30, Namespaces: []string{"acme"}, Delete: true})
	require.NoError(t, err)
	require.Equal(t, []string{
		`/auth/token {"scopes":["repository:acme/web:push"]}`,
		`/auth/token {"scopes":["repository:acme/web:push,delete"]}`,
		`/auth/pats {"name":"ci","expires_in_days":30,"namespaces":["acme"],"delete":true}`,
	}, bodies)
}

func TestDiscoverAsksTheRegistryOnceAWhile(t *testing.T) {
	asked := 0
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if fail {
			http.Error(w, `{"error":{"code":"INTERNAL","message":"down"}}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"api":1,"cdn":"https://cdn.example/","features":["index"]}`)
	}))
	defer srv.Close()
	client.ForgetDiscoveries()
	t.Cleanup(client.ForgetDiscoveries)
	ctx := context.Background()
	host := strings.TrimPrefix(srv.URL, "http://")

	// A failure is not remembered.
	_, err := client.New(host).Discover(ctx)
	require.Error(t, err)
	fail = false
	first, err := client.New(host).Discover(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, asked)

	// Another client of the same registry reuses the answer, and cannot change it.
	first.Features[0] = "changed"
	second, err := client.New(host).Discover(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, asked)
	require.Equal(t, "https://cdn.example", second.CDN)
	require.True(t, second.Supports(client.FeatureIndex))

	client.ForgetDiscoveries()
	_, err = client.New(host).Discover(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, asked)
}
