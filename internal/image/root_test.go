package image

import (
	"os"
	"testing"
)

// skipIfRoot skips tests that assert a permission failure. Root bypasses file
// permissions, so the operation under test succeeds and the assertion is
// meaningless — as happens when the suite runs in a container.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions are not enforced")
	}
}
