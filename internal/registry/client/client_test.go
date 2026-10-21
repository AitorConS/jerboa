package client_test

import (
	"context"
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
