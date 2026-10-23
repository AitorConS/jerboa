//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"os"
	"testing"
)

// Most tests run VMs from made-up image paths with a fake runner; the host
// path policy has its own tests.
func TestMain(m *testing.M) {
	hostImagePathCheck = func(_ context.Context, p string) (string, error) { return p, nil }
	os.Exit(m.Run())
}
