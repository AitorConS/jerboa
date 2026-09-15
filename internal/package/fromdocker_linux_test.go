//go:build linux

package pkg

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestElfClosure_RealDynamicBinary proves the closure walker resolves a real
// dynamically linked binary's interpreter and DT_NEEDED libraries to their true
// absolute paths, using actual ELF files from the test host assembled into a tar
// the way `docker export` would present them. This is the end-to-end guard for
// F-024/F-030: the previous cat/ldd + basename approach could not do this on a
// scratch image and lost the paths preflight needs.
func TestElfClosure_RealDynamicBinary(t *testing.T) {
	bin := ""
	for _, cand := range []string{"/usr/bin/head", "/usr/bin/cat", "/bin/cat", "/usr/bin/tr", "/usr/bin/env"} {
		if interp, _ := elfInterp(cand); interp != "" {
			bin = cand
			break
		}
	}
	if bin == "" {
		t.Skip("no dynamically linked system binary found to test against")
	}

	interp, err := elfInterp(bin)
	require.NoError(t, err)
	require.NotEmpty(t, interp, "chosen binary must be dynamic")

	binData, err := os.ReadFile(bin)
	require.NoError(t, err)
	info, err := readELFInfo(binData)
	require.NoError(t, err)
	require.NotEmpty(t, info.needed, "chosen binary must have DT_NEEDED libraries")

	// Assemble the tar: the binary and interpreter at their real paths, plus each
	// direct DT_NEEDED library located under the standard search dirs.
	type realFile struct{ guest, host string }
	realFiles := []realFile{
		{strings.TrimPrefix(bin, "/"), bin},
		{strings.TrimPrefix(interp, "/"), interp},
	}
	libGuestPaths := map[string]bool{}
	for _, soname := range info.needed {
		for _, dir := range defaultLibDirs {
			hp := filepath.Join(dir, soname)
			if st, statErr := os.Stat(hp); statErr == nil && !st.IsDir() {
				gp := strings.TrimPrefix(hp, "/")
				realFiles = append(realFiles, realFile{gp, hp})
				libGuestPaths[gp] = true
				break
			}
		}
	}
	require.NotEmpty(t, libGuestPaths, "expected to locate at least one needed library on the host")

	tarPath := filepath.Join(t.TempDir(), "export.tar")
	tf, err := os.Create(tarPath)
	require.NoError(t, err)
	tw := tar.NewWriter(tf)
	for _, rf := range realFiles {
		data, rerr := os.ReadFile(rf.host)
		require.NoError(t, rerr)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     rf.guest,
			Typeflag: tar.TypeReg,
			Mode:     0o755,
			Size:     int64(len(data)),
		}))
		_, werr := tw.Write(data)
		require.NoError(t, werr)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, tf.Close())

	cfs := newContainerFS(t, tarPath)
	closure, err := cfs.elfClosure(bin)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, c := range closure {
		got[c] = true
	}
	require.Truef(t, got[bin], "closure %v should include the binary %s", closure, bin)
	require.Truef(t, got[interp], "closure %v should include the interpreter %s", closure, interp)
	for gp := range libGuestPaths {
		require.Truef(t, got["/"+gp], "closure %v should include needed lib /%s", closure, gp)
	}
}
