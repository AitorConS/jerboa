package builder

import (
	"os"
	"strings"
)

// credentialEnv lists Jerboa's own credentials, and what lets a GitHub
// Actions job request OIDC tokens, which push as the job's trusted
// publisher. Build steps run the project's tools and scripts, which have no
// use for them.
var credentialEnv = map[string]bool{
	"ACTIONS_ID_TOKEN_REQUEST_TOKEN": true,
	"ACTIONS_ID_TOKEN_REQUEST_URL":   true,
	"JERBOA_AGENT_TOKEN":             true,
	"JERBOA_AUTH_TOKEN":              true,
	"JERBOA_CLUSTER_TOKEN":           true,
	"JERBOA_MINISIGN_SEED":           true,
	"JERBOA_OBSERVABILITY_TOKEN":     true,
	"JERBOA_REGISTRY_TOKEN":          true,
}

// Env returns the environment for a build subprocess: this process's
// environment without Jerboa's credentials, followed by extra.
func Env(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !credentialEnv[strings.ToUpper(name)] {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}
