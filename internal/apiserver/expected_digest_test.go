//go:build linux || (darwin && arm64)

package apiserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/stretchr/testify/require"
)

// run --verify sends the verified disk digest with the name:tag: the daemon
// runs that tag (with its own manifest) only while it still names that disk.
func TestHandleRun_ExpectedDigest(t *testing.T) {
	e := newFaultEnv(t)
	disk := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(disk, []byte("verified disk"), 0o600))
	m, err := e.imgs.PutMove("app", "v1", image.Manifest{
		SchemaVersion: image.SchemaVersion, Created: time.Now().UTC(),
		Config: image.Config{Memory: "128M", CPUs: 1}, DiskSize: 13,
	}, disk)
	require.NoError(t, err)

	_, rpcErr := e.run(api.RunParams{Image: "app:v1", ExpectedDigest: "sha256:" + strings.Repeat("0", 64)})
	require.NotNil(t, rpcErr)
	require.Contains(t, rpcErr.Message, "not the verified")
	_, rpcErr = e.run(api.RunParams{ImagePath: "/images/app.img", ExpectedDigest: m.DiskDigest})
	require.NotNil(t, rpcErr)
	require.Contains(t, rpcErr.Message, "expected_digest applies only")

	_, rpcErr = e.run(api.RunParams{Image: "app:v1", ExpectedDigest: m.DiskDigest})
	require.Nil(t, rpcErr)
}
