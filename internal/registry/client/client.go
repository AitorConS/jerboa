// Package client speaks the Jerboa registry protocol
// (docs/registry-protocol.md): discovery and authentication against the
// registry's API host, writes with a push token, and reads from its CDN.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/AitorConS/jerboa/internal/registry/pack"
)

// APIVersion is the protocol version this client speaks.
const APIVersion = 1

// Error is an error the registry reported.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("registry: %s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

// IsCode reports whether err is a registry error with the given code.
func IsCode(err error, code string) bool {
	var re *Error
	return errors.As(err, &re) && re.Code == code
}

// Client talks to one registry.
type Client struct {
	// Base is the API origin, e.g. https://registry.jerboa.dev.
	Base string
	// HTTP performs requests; nil means a client suited to large bodies.
	HTTP      *http.Client
	UserAgent string
}

// New returns a client for a registry host. Loopback hosts use plain HTTP,
// for local development; every other host uses HTTPS.
func New(host string) *Client {
	scheme := "https"
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		scheme = "http"
	}
	return &Client{Base: scheme + "://" + host, UserAgent: "jerboa"}
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return httpclient.Streaming
}

// Discovery is the registry's self-description.
type Discovery struct {
	API            int    `json:"api"`
	CDN            string `json:"cdn"`
	GitHubClientID string `json:"github_client_id"`
}

// Discover reads GET /v2/ and checks the protocol version.
func (c *Client) Discover(ctx context.Context) (Discovery, error) {
	var d Discovery
	if err := c.do(ctx, http.MethodGet, c.Base+"/v2/", "", nil, "", &d); err != nil {
		return d, err
	}
	if d.API != APIVersion {
		return d, fmt.Errorf("registry %s speaks protocol %d; this jerboa speaks %d", c.Base, d.API, APIVersion)
	}
	if d.CDN == "" {
		return d, fmt.Errorf("registry %s does not name its CDN", c.Base)
	}
	d.CDN = strings.TrimRight(d.CDN, "/")
	return d, nil
}

// LoginWithGitHub exchanges a GitHub token for a registry refresh token.
func (c *Client) LoginWithGitHub(ctx context.Context, githubToken string) (token, login string, err error) {
	var out struct {
		Token string `json:"token"`
		Login string `json:"login"`
	}
	err = c.do(ctx, http.MethodPost, c.Base+"/auth/github", "", jsonBody(map[string]string{"access_token": githubToken}), "application/json", &out)
	return out.Token, out.Login, err
}

// PushToken exchanges a refresh token or personal access token for a
// short-lived token that may push to namespace/repository.
func (c *Client) PushToken(ctx context.Context, credential, namespace, repository string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	scope := fmt.Sprintf("repository:%s/%s:push", namespace, repository)
	err := c.do(ctx, http.MethodPost, c.Base+"/auth/token", credential, jsonBody(map[string][]string{"scopes": {scope}}), "application/json", &out)
	return out.Token, err
}

// CreatePAT creates a personal access token.
func (c *Client) CreatePAT(ctx context.Context, credential, name string) (id, token string, err error) {
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	err = c.do(ctx, http.MethodPost, c.Base+"/auth/pats", credential, jsonBody(map[string]string{"name": name}), "application/json", &out)
	return out.ID, out.Token, err
}

// PAT describes a personal access token, without its secret.
type PAT struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at"`
}

// ListPATs lists the account's personal access tokens.
func (c *Client) ListPATs(ctx context.Context, credential string) ([]PAT, error) {
	var out struct {
		PATs []PAT `json:"pats"`
	}
	err := c.do(ctx, http.MethodGet, c.Base+"/auth/pats", credential, nil, "", &out)
	return out.PATs, err
}

// RevokePAT revokes one personal access token.
func (c *Client) RevokePAT(ctx context.Context, credential, id string) error {
	return c.do(ctx, http.MethodDelete, c.Base+"/auth/pats/"+id, credential, nil, "", nil)
}

func (c *Client) repo(namespace, repository string) string {
	return c.Base + "/v2/" + namespace + "/" + repository
}

// maxMissingQuery is the protocol's limit of digests per query.
const maxMissingQuery = 10000

// Missing returns which digests the registry does not have.
func (c *Client) Missing(ctx context.Context, token, namespace, repository string, digests []string) ([]string, error) {
	var missing []string
	for start := 0; start < len(digests); start += maxMissingQuery {
		end := min(start+maxMissingQuery, len(digests))
		var out struct {
			Missing []string `json:"missing"`
		}
		body := jsonBody(map[string][]string{"digests": digests[start:end]})
		if err := c.do(ctx, http.MethodPost, c.repo(namespace, repository)+"/blobs/missing", token, body, "application/json", &out); err != nil {
			return nil, err
		}
		missing = append(missing, out.Missing...)
	}
	return missing, nil
}

// PutPack uploads one pack.
func (c *Client) PutPack(ctx context.Context, token, namespace, repository string, p pack.Pack) error {
	body, err := pack.EncodeUpload(p)
	if err != nil {
		return err //nolint:wrapcheck // pack errors are descriptive
	}
	url := c.repo(namespace, repository) + "/packs/" + strings.TrimPrefix(p.Digest, "sha256:")
	return c.do(ctx, http.MethodPut, url, token, body, pack.MediaType, nil)
}

// PutRecipe uploads recipe bytes under their digest.
func (c *Client) PutRecipe(ctx context.Context, token, namespace, repository, digest string, data []byte) error {
	url := c.repo(namespace, repository) + "/recipes/" + strings.TrimPrefix(digest, "sha256:")
	return c.do(ctx, http.MethodPut, url, token, data, "application/vnd.jerboa.image.recipe.v1+json", nil)
}

// PutTag points a tag at a recipe.
func (c *Client) PutTag(ctx context.Context, token, namespace, repository, tag, recipe string) error {
	url := c.repo(namespace, repository) + "/tags/" + tag
	return c.do(ctx, http.MethodPut, url, token, jsonBody(map[string]string{"recipe": recipe}), "application/json", nil)
}

// DeleteTag removes a tag; its recipe and blobs are collected a week later
// unless another tag uses them.
func (c *Client) DeleteTag(ctx context.Context, token, namespace, repository, tag string) error {
	return c.do(ctx, http.MethodDelete, c.repo(namespace, repository)+"/tags/"+tag, token, nil, "", nil)
}

// RepoChanges are the repository fields EditRepo sets; nil fields keep their
// value.
type RepoChanges struct {
	Description *string `json:"description,omitempty"`
	Readme      *string `json:"readme,omitempty"`
}

// EditRepo sets a repository's description or README, which the hub shows.
// It needs a push token for the repository.
func (c *Client) EditRepo(ctx context.Context, token, namespace, repository string, changes RepoChanges) error {
	return c.do(ctx, http.MethodPatch, c.repo(namespace, repository), token, jsonBody(changes), "application/json", nil)
}

// DeleteRepo deletes a repository with its tags, description and README. It
// needs a push token for the repository.
func (c *Client) DeleteRepo(ctx context.Context, token, namespace, repository string) error {
	return c.do(ctx, http.MethodDelete, c.repo(namespace, repository), token, nil, "", nil)
}

// CountPull reports a pull for the hub's counter. It is anonymous, sent once
// without retries, and its answer carries nothing, so callers ignore errors.
func (c *Client) CountPull(ctx context.Context, namespace, repository string) error {
	_, err := c.once(ctx, http.MethodPost, c.repo(namespace, repository)+"/pulls", "", nil, "", nil)
	return err
}

// TagRecipe reads the recipe digest a tag points to, from the CDN.
func (c *Client) TagRecipe(ctx context.Context, cdn, namespace, repository, tag string) (string, error) {
	var out struct {
		Recipe string `json:"recipe"`
	}
	if err := c.do(ctx, http.MethodGet, cdn+"/tags/"+namespace+"/"+repository+"/"+tag, "", nil, "", &out); err != nil {
		var re *Error
		if errors.As(err, &re) && re.Status == http.StatusNotFound {
			return "", fmt.Errorf("%s/%s:%s not found in the registry", namespace, repository, tag)
		}
		return "", err
	}
	if !recipeDigestRE.MatchString(out.Recipe) {
		return "", fmt.Errorf("registry: tag %s/%s:%s names an invalid recipe %q", namespace, repository, tag, out.Recipe)
	}
	return out.Recipe, nil
}

var recipeDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Recipe reads recipe bytes from the CDN and checks them against digest.
func (c *Client) Recipe(ctx context.Context, cdn, digest string) ([]byte, error) {
	var data []byte
	if err := c.do(ctx, http.MethodGet, cdn+"/recipes/"+strings.TrimPrefix(digest, "sha256:")+".json", "", nil, "", &data); err != nil {
		var re *Error
		if errors.As(err, &re) && re.Status == http.StatusNotFound {
			return nil, fmt.Errorf("recipe %s not found in the registry", digest)
		}
		return nil, err
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("registry: recipe %s does not match its digest", digest)
	}
	return data, nil
}

// Locations reads where a recipe's blobs live.
func (c *Client) Locations(ctx context.Context, cdn, recipe string) (map[string]pack.Location, error) {
	var out struct {
		Blobs map[string]pack.Location `json:"blobs"`
	}
	err := c.do(ctx, http.MethodGet, cdn+"/locations/"+strings.TrimPrefix(recipe, "sha256:")+".json", "", nil, "", &out)
	return out.Blobs, err
}

// ReadRange reads length bytes of a pack starting at offset.
func (c *Client) ReadRange(ctx context.Context, cdn, packDigest string, offset, length int64) ([]byte, error) {
	if err := (pack.Location{Pack: packDigest, Offset: offset, Length: length}).Validate(); err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdn+"/packs/"+strings.TrimPrefix(packDigest, "sha256:"), nil)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: read pack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("registry: read pack %s: HTTP %d, want 206", packDigest, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, length+1))
	if err != nil {
		return nil, fmt.Errorf("registry: read pack: %w", err)
	}
	if int64(len(data)) != length {
		return nil, fmt.Errorf("registry: read pack %s: got %d bytes, want %d", packDigest, len(data), length)
	}
	return data, nil
}

func jsonBody(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// maxResponseBytes bounds every response read whole (recipes are the largest).
const maxResponseBytes = 32 << 20

// do performs one request with retries on transport errors and 5xx/429
// answers, and decodes a JSON answer into out (or keeps raw bytes when out is
// a *[]byte).
func (c *Client) do(ctx context.Context, method, url, token string, body []byte, contentType string, out any) error {
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("registry: %w", ctx.Err())
			case <-time.After(time.Duration(1<<attempt) * 250 * time.Millisecond):
			}
		}
		retry, err := c.once(ctx, method, url, token, body, contentType, out)
		if err == nil || !retry {
			return err
		}
		last = err
	}
	return last
}

func (c *Client) once(ctx context.Context, method, url, token string, body []byte, contentType string, out any) (bool, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return false, fmt.Errorf("registry: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("registry: %s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return true, fmt.Errorf("registry: %s %s: %w", method, url, err)
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode, Code: "HTTP", Message: http.StatusText(resp.StatusCode)}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error.Code != "" {
			e.Code, e.Message = env.Error.Code, env.Error.Message
		}
		return resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests, e
	}
	switch o := out.(type) {
	case nil:
	case *[]byte:
		*o = data
	default:
		if err := json.Unmarshal(data, out); err != nil {
			return false, fmt.Errorf("registry: %s %s: decode answer: %w", method, url, err)
		}
	}
	return false, nil
}
