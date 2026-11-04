package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func signCmd(t *testing.T, args ...string) (*cobra.Command, *bool, *bytes.Buffer) {
	t.Helper()
	var sign bool
	cmd := &cobra.Command{}
	cmd.Flags().BoolVar(&sign, "sign", false, "")
	require.NoError(t, cmd.ParseFlags(args))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	return cmd, &sign, &stderr
}

func TestShouldSign(t *testing.T) {
	withAttestations := client.Discovery{Features: []string{client.FeatureAttestations}}
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")

	// Outside GitHub Actions: unsigned, unless asked, which fails.
	cmd, sign, _ := signCmd(t)
	ok, err := shouldSign(cmd, *sign, withAttestations, "r")
	require.NoError(t, err)
	require.False(t, ok)
	cmd, sign, _ = signCmd(t, "--sign")
	_, err = shouldSign(cmd, *sign, withAttestations, "r")
	require.ErrorContains(t, err, "id-token: write")

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.com/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "x")
	cmd, sign, _ = signCmd(t)
	ok, err = shouldSign(cmd, *sign, withAttestations, "r")
	require.NoError(t, err)
	require.True(t, ok)
	cmd, sign, _ = signCmd(t, "--sign=false")
	ok, err = shouldSign(cmd, *sign, withAttestations, "r")
	require.NoError(t, err)
	require.False(t, ok)

	// A registry without attestations: a note by default, an error when asked.
	cmd, sign, stderr := signCmd(t)
	ok, err = shouldSign(cmd, *sign, client.Discovery{}, "old.example")
	require.NoError(t, err)
	require.False(t, ok)
	require.Contains(t, stderr.String(), "old.example does not store attestations")
	cmd, sign, _ = signCmd(t, "--sign")
	_, err = shouldSign(cmd, *sign, client.Discovery{}, "old.example")
	require.ErrorContains(t, err, "cannot keep signatures")
}

func TestRequiredIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ref := func(s string) naming.ImageReference {
		r, err := naming.ParseImageReference(s)
		require.NoError(t, err)
		return r
	}

	id, ok, err := requiredIdentity(ref("jerboa/postgresql:16"), "", false)
	require.NoError(t, err)
	require.True(t, ok, "official images are always checked")
	require.Equal(t, "AitorConS/jerboa-images", id.Repository)

	_, ok, err = requiredIdentity(ref("acme/web:v1"), "", false)
	require.NoError(t, err)
	require.False(t, ok)
	_, _, err = requiredIdentity(ref("acme/web:v1"), "", true)
	require.ErrorContains(t, err, "pass --identity")

	id, ok, err = requiredIdentity(ref("acme/web:v1"), "acme/web", false)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "acme/web", id.String())
	_, _, err = requiredIdentity(ref("acme/web:v1"), "acme", false)
	require.Error(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(home, ".jerboa"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".jerboa", "policy.json"),
		[]byte(`{"require":[{"namespace":"acme","identity":"acme/infra/.github/workflows/publish.yml@refs/heads/main"}]}`), 0o600))
	id, ok, err = requiredIdentity(ref("acme/web:v1"), "", false)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "acme/infra/.github/workflows/publish.yml@refs/heads/main", id.String())
}
