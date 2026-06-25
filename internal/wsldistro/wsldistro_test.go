package wsldistro

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDistroList_UTF16(t *testing.T) {
	// wsl --list --quiet emits UTF-16LE (each ASCII char followed by a NUL),
	// CRLF line endings, and a trailing blank line.
	utf16 := func(s string) []byte {
		var b []byte
		for _, r := range s {
			b = append(b, byte(r), 0)
		}
		return b
	}
	raw := utf16("Ubuntu\r\njerboa\r\ndocker-desktop\r\n\r\n")

	got := parseDistroList(raw)
	require.Equal(t, []string{"Ubuntu", "jerboa", "docker-desktop"}, got)
}

func TestParseDistroList_Empty(t *testing.T) {
	require.Empty(t, parseDistroList(nil))
}

func TestDecodeWSLOutput_StripsNUL(t *testing.T) {
	// UTF-16LE "hi" plus a CR is decoded back to ASCII.
	require.Equal(t, "hi\r", decodeWSLOutput([]byte{'h', 0, 'i', 0, '\r', 0}))
}

func TestInstallDaemonBinary_OpenError(t *testing.T) {
	// A missing source binary fails before any `wsl` invocation, so the open
	// error path is exercised without a real distro.
	err := InstallDaemonBinary(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "wsldistro: open")
}

func TestDefaultInstallDir_FallbackHome(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	require.Contains(t, DefaultInstallDir(), filepath.Join(".jerboa", "distro"))
}

func TestDefaultInstallDir_UsesLocalAppData(t *testing.T) {
	base := `C:\Users\test\AppData\Local`
	t.Setenv("LOCALAPPDATA", base)
	// filepath.Join uses the host separator, so build the expectation the same
	// way to keep the test portable across the Linux CI and Windows.
	require.Equal(t, filepath.Join(base, "jerboa", "distro"), DefaultInstallDir())
}
