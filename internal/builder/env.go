package builder

import (
	"os"
	"strings"
)

// credentialEnv lists Jerboa's own credentials. Build steps run the project's
// tools and scripts, which have no use for them.
var credentialEnv = map[string]bool{
	"JERBOA_AGENT_TOKEN":         true,
	"JERBOA_AUTH_TOKEN":          true,
	"JERBOA_CLUSTER_TOKEN":       true,
	"JERBOA_MINISIGN_SEED":       true,
	"JERBOA_OBSERVABILITY_TOKEN": true,
	"JERBOA_REGISTRY_TOKEN":      true,
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
