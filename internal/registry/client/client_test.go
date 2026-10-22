package client_test

import (
	"context"
	"net/http"
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
	_, err = c.TagRecipe(ctx, cdn, "aitorcons", "web", "nope")
	require.EqualError(t, err, "aitorcons/web:nope not found in the registry")
}
