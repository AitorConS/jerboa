package builder

import "github.com/AitorConS/jerboa/internal/childproc"

// Env returns the environment for a build subprocess: this process's
// environment without Jerboa's credentials (see childproc.IsCredential),
// followed by extra. Build steps run the project's tools and scripts, which
// have no use for them.
func Env(extra ...string) []string {
	return childproc.Inherited(extra...)
}
