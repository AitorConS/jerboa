//go:build linux || (darwin && arm64)

package upgradetest_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/snapshot"
	"github.com/AitorConS/jerboa/internal/upgradetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshotsSurvive opens the snapshot store of a released version (only
// the native macOS backend takes snapshots, so only its fixtures hold one):
// every snapshot is listed with what the released version said about it, and
// its artifact still verifies against the hashes recorded when it was taken.
func TestSnapshotsSurvive(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		if len(f.Expected.Snapshots) == 0 {
			t.Skipf("the %s backend of %s takes no snapshots", f.Hypervisor, f.OS)
		}
		store, err := snapshot.Open(filepath.Join(f.Data(), "snapshots"), snapshot.Limits{})
		require.NoError(t, err)
		// Nothing was in flight when the released daemon stopped.
		leftovers, err := store.RecoverStaging()
		require.NoError(t, err)
		assert.Empty(t, leftovers)
		entries, err := store.List()
		require.NoError(t, err)
		require.Len(t, entries, len(f.Expected.Snapshots))
		for _, want := range f.Expected.Snapshots {
			name := str(want, "name")
			info, err := store.Get(name)
			require.NoError(t, err)
			assert.Equal(t, str(want, "vm_id"), info.VMID)
			assert.Equal(t, str(want, "vm_name"), info.VMName)
			assert.Equal(t, str(want, "backend"), info.Backend)
			assert.Equal(t, str(want, "image_digest"), info.ImageDigest)
			assert.Equal(t, str(want, "memory"), info.Memory)
			assert.Equal(t, str(want, "network"), info.Network.Name)
			assert.Equal(t, str(want, "ip_address"), info.Network.IP)
			assert.Equal(t, str(want, "mac"), info.Network.MAC)
			assert.Equal(t, int64(want["size_bytes"].(float64)), info.SizeBytes)
			assert.Len(t, info.Components, int(want["components"].(float64)))
			created, err := time.Parse(time.RFC3339, str(want, "created_at"))
			require.NoError(t, err)
			assert.True(t, created.Equal(info.CreatedAt.Truncate(time.Second)))
			// The VM it belongs to is still there to be restored into.
			assert.Contains(t, f.Expected.VMs, info.VMName)
			// Every component is read back and compared with its hash.
			_, _, _, err = store.Verify(name)
			require.NoError(t, err, "snapshot %s no longer verifies", name)
		}
		count, bytes, err := store.Usage()
		require.NoError(t, err)
		assert.Equal(t, len(f.Expected.Snapshots), count)
		assert.Positive(t, bytes)
	})
}
