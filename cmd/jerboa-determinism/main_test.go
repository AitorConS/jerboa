package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The catalog must be the same bytes on every run and host: the digests CI
// compares are only meaningful if the inputs are.
func TestCatalogIsReproducible(t *testing.T) {
	fingerprint := func(dir string) map[string]string {
		out := map[string]string{}
		for _, spec := range catalog() {
			cfg, err := spec.materialize(filepath.Join(dir, spec.name))
			require.NoError(t, err)
			h := sha256.New()
			for _, f := range cfg.PkgFiles {
				h.Write([]byte(f.GuestPath))
				if f.IsDir {
					continue
				}
				data, err := os.ReadFile(f.HostPath)
				require.NoError(t, err)
				info, err := os.Stat(f.HostPath)
				require.NoError(t, err)
				h.Write(data)
				h.Write([]byte{byte(info.Mode().Perm() >> 6)})
			}
			out[spec.name] = hex.EncodeToString(h.Sum(nil))
		}
		return out
	}
	first, second := fingerprint(t.TempDir()), fingerprint(t.TempDir())
	require.Len(t, first, len(catalog()))
	require.Equal(t, first, second)
}
