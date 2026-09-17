package image

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeMBR writes a one-sector image whose first two partition entries have
// the given sector counts.
func writeMBR(t *testing.T, path string, bootfsSectors, rootSectors uint32) {
	t.Helper()
	mbr := make([]byte, mbrSize)
	binary.LittleEndian.PutUint32(mbr[mbrPartitionTable+mbrEntryNSectorsOff:], bootfsSectors)
	binary.LittleEndian.PutUint32(mbr[mbrPartitionTable+mbrPartitionEntry+mbrEntryNSectorsOff:], rootSectors)
	mbr[510], mbr[511] = 0x55, 0xaa
	require.NoError(t, os.WriteFile(path, mbr, 0o600))
}

func TestParseLayout(t *testing.T) {
	for in, want := range map[string]string{"": LayoutStandard, "standard": LayoutStandard, "compact": LayoutCompact} {
		got, err := ParseLayout(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := ParseLayout("tiny")
	require.Error(t, err)
}

func TestCheckImageLayout(t *testing.T) {
	dir := t.TempDir()
	compact := filepath.Join(dir, "compact.img")
	writeMBR(t, compact, 0, 8192)
	standard := filepath.Join(dir, "standard.img")
	writeMBR(t, standard, 5888, 8192)

	require.NoError(t, checkImageLayout(compact, LayoutCompact))
	require.ErrorContains(t, checkImageLayout(standard, LayoutCompact), "did not produce the compact layout")
	require.NoError(t, checkImageLayout(standard, LayoutStandard), "standard layout is not checked")

	short := filepath.Join(dir, "short.img")
	require.NoError(t, os.WriteFile(short, []byte("fake disk"), 0o600))
	require.Error(t, checkImageLayout(short, LayoutCompact))
}

func TestBuildManifestCompactKey(t *testing.T) {
	require.NotContains(t, BuildManifest(BuildConfig{BinaryPath: "/bin/app"}), "compact:")
	require.Contains(t, BuildManifest(BuildConfig{BinaryPath: "/bin/app", Layout: LayoutCompact}), "\n    compact:true\n")
}

func TestBuildCompactLayout(t *testing.T) {
	dir := t.TempDir()
	binPath := writeELF(t, dir)
	var gotManifest string
	mkfs := func(compactOutput bool) MkfsFunc {
		return func(_ context.Context, imgPath, _ string, manifest string) *exec.Cmd {
			gotManifest = manifest
			if compactOutput {
				writeMBR(t, imgPath, 0, 8192)
			} else {
				writeMBR(t, imgPath, 5888, 8192)
			}
			return successCmd()
		}
	}

	m, err := NewBuilder(makeStore(t)).Build(context.Background(), BuildConfig{
		Name: "app", BinaryPath: binPath, MkfsRun: mkfs(true), Layout: LayoutCompact,
	})
	require.NoError(t, err)
	require.Equal(t, LayoutCompact, m.Layout)
	require.True(t, strings.Contains(gotManifest, "compact:true"))

	// An old mkfs ignores the key and writes a standard image.
	_, err = NewBuilder(makeStore(t)).Build(context.Background(), BuildConfig{
		Name: "app", BinaryPath: binPath, MkfsRun: mkfs(false), Layout: LayoutCompact,
	})
	require.ErrorContains(t, err, "compact layout")

	_, err = NewBuilder(makeStore(t)).Build(context.Background(), BuildConfig{
		Name: "app", BinaryPath: binPath, MkfsRun: mkfs(true), Layout: "tiny",
	})
	require.ErrorContains(t, err, "invalid image layout")
}

func TestManifestLayoutValidation(t *testing.T) {
	m := validManifest()
	m.Layout = LayoutCompact
	data, err := Marshal(m)
	require.NoError(t, err)
	parsed, err := Parse(data)
	require.NoError(t, err)
	require.Equal(t, LayoutCompact, parsed.Layout)

	m.Layout = "tiny"
	data, err = Marshal(m)
	require.NoError(t, err)
	_, err = Parse(data)
	require.ErrorContains(t, err, "unsupported layout")

	legacy := validManifest()
	data, err = Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(data), "layout", "standard manifests stay byte-compatible")
}
