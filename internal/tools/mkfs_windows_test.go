//go:build windows

package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteManifestWindowsPathsToWSL(t *testing.T) {
	in := "(children:(program:(contents:(host:C:/Users/test/app.exe)) certs:(contents:(host:D:/pkg/ca.pem))))"
	out := rewriteManifestWindowsPathsToWSL(in)
	require.Contains(t, out, "host:/mnt/c/Users/test/app.exe")
	require.Contains(t, out, "host:/mnt/d/pkg/ca.pem")
}
