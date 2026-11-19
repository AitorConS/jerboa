//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/stretchr/testify/require"
)

// An image's run defaults reach the hypervisor's command line. One stored
// before pulls checked them, with defaults a registry chose, is refused at
// run time rather than handed to QEMU.
func TestRunRefusesInvalidImageRunDefaults(t *testing.T) {
	e := newSysEnv(t)
	store := func(name string, cfg image.Config) {
		src := filepath.Join(t.TempDir(), "disk.img")
		require.NoError(t, os.WriteFile(src, []byte(name), 0o644))
		m := image.Manifest{
			SchemaVersion: image.SchemaVersion, Name: name, Tag: "v1", Created: time.Now().UTC(), Architecture: runtime.GOARCH,
			Config: cfg, DiskSize: int64(len(name)),
		}
		require.NoError(t, e.s.imgStore.Put(name, "v1", m, src))
	}
	run := func(ref string) *api.RPCError {
		params, err := json.Marshal(api.RunParams{Image: ref})
		require.NoError(t, err)
		_, rerr := e.s.handleRun(context.Background(), params)
		return rerr
	}

	store("good", image.Config{Memory: "512M", CPUs: 2})
	require.Nil(t, run("good:v1"))

	for name, cfg := range map[string]image.Config{
		"options": {Memory: "1G,maxmem=64G", CPUs: 1},
		"huge":    {Memory: "4096T", CPUs: 1},
		"cpus":    {Memory: "256M", CPUs: 100000},
		"disk":    {Memory: "256M", CPUs: 1, MinDiskSize: -1},
	} {
		store(name, cfg)
		rerr := run(name + ":v1")
		require.NotNil(t, rerr, name)
		require.Contains(t, rerr.Message, "rebuild it", name)
	}
}
