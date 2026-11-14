// Package credentials keeps the CLI's registry credentials: refresh tokens
// from jerboa login, or personal access tokens, per registry host. Only the
// CLI reads them; the daemon only ever receives short-lived push tokens. In
// a GitHub Actions job allowed to request OIDC tokens, the job's identity
// stands in for them (GitHubActions).
package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AitorConS/jerboa/internal/durable"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/AitorConS/jerboa/internal/naming"
)

// EnvToken overrides the stored credential for one registry, for CI.
const EnvToken = "JERBOA_REGISTRY_TOKEN" //nolint:gosec // the name of an environment variable, not a credential

// EnvHost names the registry EnvToken is for; unset means
// naming.DefaultRegistry. The token is never sent to any other registry.
const EnvHost = "JERBOA_REGISTRY_HOST"

// GitHubActions is the credential For returns in a GitHub Actions job that
// may request OIDC tokens (permissions: id-token: write) and has no
// JERBOA_REGISTRY_TOKEN: the registry client exchanges an OIDC token, for the
// registry as audience, for a push token, which a trusted publisher of the
// namespace must allow. It is not a secret and is never sent as one.
const GitHubActions = "github-actions-oidc"

// The variables GitHub Actions sets in a job allowed to request OIDC tokens.
const (
	EnvActionsTokenURL = "ACTIONS_ID_TOKEN_REQUEST_URL"
	EnvActionsToken    = "ACTIONS_ID_TOKEN_REQUEST_TOKEN" //nolint:gosec // the name of an environment variable, not a credential
)

// A registry behind Cloudflare Access (a private staging deployment) answers
// only requests that carry a service token. These name the token and the one
// registry it is for; all three must be set.
const (
	EnvAccessHost         = "JERBOA_REGISTRY_ACCESS_HOST"
	EnvAccessClientID     = "JERBOA_REGISTRY_ACCESS_CLIENT_ID"
	EnvAccessClientSecret = "JERBOA_REGISTRY_ACCESS_CLIENT_SECRET" //nolint:gosec // the name of an environment variable, not a credential
)

// AccessToken returns the Cloudflare Access service token to send to host:
// the registry EnvAccessHost names, or a subdomain of it (its CDN). The token
// is never sent to any other host.
func AccessToken(host string) (id, secret string, ok bool) {
	want := strings.ToLower(strings.TrimSpace(os.Getenv(EnvAccessHost)))
	id, secret = os.Getenv(EnvAccessClientID), os.Getenv(EnvAccessClientSecret)
	if want == "" || id == "" || secret == "" {
		return "", "", false
	}
	host = strings.ToLower(host)
	if host != want && !strings.HasSuffix(host, "."+want) {
		return "", "", false
	}
	return id, secret, true
}

// AccessConfigured reports whether a service token is set for some registry.
func AccessConfigured() bool {
	return os.Getenv(EnvAccessHost) != "" && os.Getenv(EnvAccessClientID) != "" && os.Getenv(EnvAccessClientSecret) != ""
}

// InGitHubActions reports whether the process runs in a GitHub Actions job
// that may request OIDC tokens.
func InGitHubActions() bool {
	return os.Getenv(EnvActionsTokenURL) != "" && os.Getenv(EnvActionsToken) != ""
}

// Entry is the credential for one registry.
type Entry struct {
	Token string `json:"token"`
	Login string `json:"login,omitempty"`
}

// File is the credentials file.
type File struct {
	Registries map[string]Entry `json:"registries"`
}

// Path is ~/.jerboa/credentials.json.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credentials: %w", err)
	}
	return filepath.Join(home, ".jerboa", "credentials.json"), nil
}

// Load reads the credentials file; a missing file is empty.
func Load() (File, error) {
	f := File{Registries: map[string]Entry{}}
	p, err := Path()
	if err != nil {
		return f, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // the user's own credentials file
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("credentials: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		// What a power cut leaves of a file that was being created: no
		// logins, not a file that blocks every later login.
		return f, nil
	}
	if err := json.Unmarshal(data, &f); err != nil {
		f.Registries = map[string]Entry{}
		return f, fmt.Errorf("credentials: %s: %w", p, err)
	}
	if f.Registries == nil {
		f.Registries = map[string]Entry{}
	}
	return f, nil
}

// Save writes the file readable by its owner only.
func (f File) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if err := durable.WriteFile(p, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	return nil
}

// For returns the credential to use with registry: JERBOA_REGISTRY_TOKEN
// when it is set and registry is the one it is for; otherwise GitHubActions
// in a job that may request OIDC tokens; otherwise the stored one.
func For(registry string) (string, error) {
	t := os.Getenv(EnvToken)
	if t != "" && registry == envHost() {
		return t, nil
	}
	if InGitHubActions() {
		return GitHubActions, nil
	}
	f, err := Load()
	if err != nil {
		return "", err
	}
	if e, ok := f.Registries[registry]; ok && e.Token != "" {
		return e.Token, nil
	}
	if t != "" {
		return "", fmt.Errorf("not logged in to %s; %s is for %s (set %s=%s to use it there)", registry, EnvToken, envHost(), EnvHost, registry)
	}
	return "", fmt.Errorf("not logged in to %s; run jerboa login %s, set %s, or in GitHub Actions grant the job permissions: id-token: write", registry, registry, EnvToken)
}

func envHost() string {
	if h := strings.TrimSpace(os.Getenv(EnvHost)); h != "" {
		return h
	}
	return naming.DefaultRegistry
}
