package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/AitorConS/jerboa/internal/registry/credentials"
)

// A GitHub Actions job pushes as a trusted publisher of the namespace: the
// job's OIDC token, requested for the registry's host as audience, is
// exchanged at POST /auth/oidc for a push token (docs/registry-protocol.md).
// GitHub signs the token; the registry checks it names a repository and
// workflow an admin of the namespace trusts.

// actionsTimeout bounds the request for the job's OIDC token.
const actionsTimeout = 30 * time.Second

// actionsToken requests the job's OIDC token for audience from GitHub
// Actions, with the request URL and token it sets in the job's environment.
func (c *Client) actionsToken(ctx context.Context, audience string) (string, error) {
	u, err := url.Parse(os.Getenv(credentials.EnvActionsTokenURL))
	// Plain HTTP only to a loopback test server.
	if err != nil || (u.Scheme != "https" && u.Hostname() != "127.0.0.1") {
		return "", fmt.Errorf("github actions: %s is not an https URL", credentials.EnvActionsTokenURL)
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, actionsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("github actions: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv(credentials.EnvActionsToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("github actions: request an OIDC token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("github actions: request an OIDC token: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github actions: requesting an OIDC token answered %s", resp.Status)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Value == "" {
		return "", errors.New("github actions: the OIDC token answer has no token")
	}
	return out.Value, nil
}

// oidcToken exchanges the job's OIDC token for a push token for scope.
func (c *Client) oidcToken(ctx context.Context, scope string) (string, error) {
	base, err := url.Parse(c.Base)
	if err != nil {
		return "", fmt.Errorf("registry: %w", err)
	}
	idToken, err := c.actionsToken(ctx, base.Host)
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	body := jsonBody(map[string]any{"token": idToken, "scopes": []string{scope}})
	err = c.do(ctx, http.MethodPost, c.Base+"/auth/oidc", "", body, "application/json", &out)
	switch {
	case IsStatus(err, http.StatusNotFound):
		return "", fmt.Errorf("%s does not accept GitHub Actions OIDC tokens; set %s instead: %w", base.Host, credentials.EnvToken, err)
	case IsStatus(err, http.StatusForbidden):
		return "", fmt.Errorf("no trusted publisher allows this workflow; an admin of the namespace adds one on the hub, under Settings, Trusted publishers: %w", err)
	}
	return out.Token, err
}

// errLoginOnly refuses what only a login or personal access token can do.
var errLoginOnly = fmt.Errorf("in GitHub Actions without %s, jerboa only pushes, with the job's OIDC token; this needs jerboa login or a personal access token", credentials.EnvToken)
