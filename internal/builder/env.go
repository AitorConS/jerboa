package builder

import (
	"os"
	"strings"
)

// credentialEnv lists credentials build steps have no use for: Jerboa's
// own, what lets a GitHub Actions job request OIDC tokens (which push as the
// job's trusted publisher), and well-known tokens and keys of code hosts,
// cloud providers and APIs. Build steps run the project's tools and
// scripts, which can read their environment and, for the language tools,
// reach the network. Variables package managers read to fetch private
// dependencies (NODE_AUTH_TOKEN, NPM_TOKEN, CARGO_REGISTRIES_*_TOKEN,
// GOPRIVATE, PIP_INDEX_URL) are kept; a build that needs one of these
// passes it under such a name.
var credentialEnv = map[string]bool{
	"ACTIONS_ID_TOKEN_REQUEST_TOKEN": true,
	"ACTIONS_ID_TOKEN_REQUEST_URL":   true,
	"JERBOA_MINISIGN_SEED":           true,

	"GITHUB_TOKEN":                      true,
	"GH_TOKEN":                          true,
	"GH_ENTERPRISE_TOKEN":               true,
	"GITHUB_ENTERPRISE_TOKEN":           true,
	"AWS_ACCESS_KEY_ID":                 true,
	"AWS_SECRET_ACCESS_KEY":             true,
	"AWS_SESSION_TOKEN":                 true,
	"AWS_SECURITY_TOKEN":                true,
	"AWS_WEB_IDENTITY_TOKEN_FILE":       true,
	"GOOGLE_APPLICATION_CREDENTIALS":    true,
	"GOOGLE_CREDENTIALS":                true,
	"CLOUDSDK_AUTH_ACCESS_TOKEN":        true,
	"AZURE_CLIENT_SECRET":               true,
	"AZURE_CLIENT_CERTIFICATE_PASSWORD": true,
	"AZURE_CLIENT_CERTIFICATE_PATH":     true,
	"AZURE_PASSWORD":                    true,
	"AZURE_FEDERATED_TOKEN_FILE":        true,
	"AZURE_STORAGE_KEY":                 true,
	"AZURE_STORAGE_CONNECTION_STRING":   true,
	"AZURE_STORAGE_SAS_TOKEN":           true,
	"VAULT_TOKEN":                       true,
	"CLOUDFLARE_API_TOKEN":              true,
	"CLOUDFLARE_API_KEY":                true,
	"ANTHROPIC_API_KEY":                 true,
	"OPENAI_API_KEY":                    true,
}

// isCredentialEnv reports whether the variable name is one of
// credentialEnv or one of Jerboa's own tokens and secrets (JERBOA_*TOKEN*,
// JERBOA_*SECRET*).
func isCredentialEnv(name string) bool {
	name = strings.ToUpper(name)
	if credentialEnv[name] {
		return true
	}
	rest, ok := strings.CutPrefix(name, "JERBOA_")
	return ok && (strings.Contains(rest, "TOKEN") || strings.Contains(rest, "SECRET"))
}

// Env returns the environment for a build subprocess: this process's
// environment without the credentials above, followed by extra.
func Env(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !isCredentialEnv(name) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}
