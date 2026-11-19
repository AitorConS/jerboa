package builder

import (
	"strings"

	"github.com/AitorConS/jerboa/internal/childproc"
)

// thirdPartyCredentialEnv lists credentials build steps have no use for on
// top of Jerboa's own (childproc.IsCredential): well-known tokens and keys
// of code hosts, cloud providers and APIs. Build steps run the project's
// tools and scripts, which can read their environment and, for the language
// tools, reach the network. Variables package managers read to fetch
// private dependencies (NODE_AUTH_TOKEN, NPM_TOKEN,
// CARGO_REGISTRIES_*_TOKEN, GOPRIVATE, PIP_INDEX_URL) are kept; a build
// that needs one of these passes it under such a name.
var thirdPartyCredentialEnv = map[string]bool{
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

// Env returns the environment for a build subprocess: this process's
// environment without Jerboa's credentials (childproc.Inherited) and those
// above, followed by extra.
func Env(extra ...string) []string {
	inherited := childproc.Inherited()
	env := make([]string, 0, len(inherited)+len(extra))
	for _, kv := range inherited {
		name, _, _ := strings.Cut(kv, "=")
		if !thirdPartyCredentialEnv[strings.ToUpper(name)] {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}
