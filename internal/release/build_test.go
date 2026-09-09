package release

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}
	cliWin := write("jerboa-windows-amd64.exe", "WINDOWS-CLI")
	cliLin := write("jerboa-linux-amd64", "LINUX-CLI")
	kimg := write("kernel.img", "KERNEL")
	rootfs := write("jerboa-rootfs-amd64.tar.gz", "ROOTFS")
	desktop := write("jerboa-desktop-setup-0.2.0.exe", "DESKTOP")

	spec := Spec{
		Channel: "stable",
		Base:    "https://releases.jerboa.dev/",
		Components: map[string]SpecComponent{
			"cli": {Version: "v0.4.0", Platforms: map[string]string{
				"windows-amd64": cliWin,
				"linux-amd64":   cliLin,
			}},
			"kernel": {Version: "v0.2.1", Files: map[string]string{"kernel.img": kimg}},
			"distro": {Version: "v0.4.0", File: rootfs, KernelVer: "v0.2.1"},
			"desktop": {Version: "v0.2.0", Platforms: map[string]string{
				"windows-amd64": desktop,
			}},
		},
	}

	m, err := BuildManifest(spec)
	require.NoError(t, err)

	// The generated manifest must pass the same validation clients apply.
	raw, err := m.Marshal()
	require.NoError(t, err)
	parsed, err := ParseManifest(raw)
	require.NoError(t, err)

	cli, _ := parsed.Component(ComponentCLI)
	win, err := cli.Asset("windows", "amd64")
	require.NoError(t, err)
	assert.Equal(t, "https://releases.jerboa.dev/cli/v0.4.0/jerboa-windows-amd64.exe", win.URL)
	assert.NotEmpty(t, win.SHA256)
	assert.Equal(t, int64(len("WINDOWS-CLI")), win.Size)

	kern, _ := parsed.Component(ComponentKernel)
	assert.Equal(t, "https://releases.jerboa.dev/kernel/v0.2.1/kernel.img", kern.Files["kernel.img"].URL)

	distro, _ := parsed.Component(ComponentDistro)
	assert.Equal(t, "v0.2.1", distro.KernelVer)
	assert.Equal(t, "https://releases.jerboa.dev/distro/v0.4.0/jerboa-rootfs-amd64.tar.gz", distro.URL)

	// The signed manifest must reference the updater's existing installer,
	// rather than requiring a second copy in desktop/<version>/.
	desk, ok := parsed.Component(ComponentDesktop)
	require.True(t, ok)
	installer, err := desk.Asset("windows", "amd64")
	require.NoError(t, err)
	assert.Equal(t, "https://releases.jerboa.dev/desktop/jerboa-desktop-setup-0.2.0.exe", installer.URL)
	sum, size, err := hashFile(desktop)
	require.NoError(t, err)
	assert.Equal(t, sum, installer.SHA256)
	assert.Equal(t, size, installer.Size)
}

func TestBuildManifestMissingFile(t *testing.T) {
	spec := Spec{
		Channel:    "stable",
		Base:       "https://x",
		Components: map[string]SpecComponent{"cli": {Version: "v1", File: "/does/not/exist"}},
	}
	_, err := BuildManifest(spec)
	assert.Error(t, err)
}
