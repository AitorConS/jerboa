//go:build !unix

package builder

import (
	"context"
	"os/exec"
)

// ownProcessGroup does nothing where there are no Unix process groups.
func ownProcessGroup(*exec.Cmd, context.CancelFunc) func() { return func() {} }
