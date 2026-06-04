package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	pkg "github.com/AitorConS/unikernel-engine/internal/package"
	"github.com/stretchr/testify/require"
)

func successCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit", "0")
	}
	return exec.Command("true")
}

func failureCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit", "1")
	}
	return exec.Command("false")
}

func TestBuildManifest_NoPkgFiles(t *testing.T) {
	got := BuildManifest(filepath.FromSlash("/usr/bin/hello"), nil)
	require.Contains(t, got, "program:/program")
	require.Contains(t, got, "program:(contents:(host:")
	require.NotContains(t, got, "node")
}

func TestBuildManifest_WithPkgFiles(t *testing.T) {
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/home/user/.uni/packages/node/20.11.0/files/bin/node"), GuestPath: "node"},
		{HostPath: filepath.FromSlash("/home/user/.uni/packages/node/20.11.0/files/lib/libnode.so"), GuestPath: "libnode.so"},
	}
	got := BuildManifest(filepath.FromSlash("/usr/bin/hello"), pkgFiles)
	require.Contains(t, got, "program:/program")
	require.Contains(t, got, "node:(contents:(host:")
	require.Contains(t, got, "libnode.so:(contents:(host:")
}

func TestBuildManifest_OpsSysrootPkgFiles(t *testing.T) {
	pkgFiles := []pkg.File{
		{HostPath: filepath.FromSlash("/home/user/.uni/packages-ops/eyberg/node_v16/files/node"), GuestPath: "node"},
		{HostPath: filepath.FromSlash("/home/user/.uni/packages-ops/eyberg/node_v16/files/sysroot/lib/x86_64-linux-gnu/libnss_dns.so.2"), GuestPath: "lib/x86_64-linux-gnu/libnss_dns.so.2"},
		{HostPath: filepath.FromSlash("/home/user/.uni/packages-ops/eyberg/node_v16/files/sysroot/etc/ssl/certs/ca-certificates.crt"), GuestPath: "etc/ssl/certs/ca-certificates.crt"},
	}
	got := BuildManifest(filepath.FromSlash("/usr/bin/hello"), pkgFiles)
	require.Contains(t, got, "node:(contents:(host:")
	// Nested tree — no slash-separated flat keys (the Nanos parser rejects them).
	require.NotContains(t, got, "lib/x86_64-linux-gnu")
	require.NotContains(t, got, "etc/ssl/certs")
	require.Contains(t, got, "lib:(\n")
	require.Contains(t, got, "x86_64-linux-gnu:(\n")
	require.Contains(t, got, "libnss_dns.so.2:(contents:(host:")
	require.Contains(t, got, "etc:(\n")
	require.Contains(t, got, "ca-certificates.crt:(contents:(host:")
}

func TestBuildManifest_PkgFilesIntegration(t *testing.T) {
	pkgDir := t.TempDir()
	binDir := filepath.Join(pkgDir, "bin")
	libDir := filepath.Join(pkgDir, "lib")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.MkdirAll(libDir, 0o755))

	binPath := filepath.Join(binDir, "myapp")
	libPath := filepath.Join(libDir, "libmyapp.so")
	require.NoError(t, os.WriteFile(binPath, []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(libPath, []byte("sharedlib"), 0o644))

	pkgFiles := []pkg.File{
		{HostPath: binPath, GuestPath: "myapp"},
		{HostPath: libPath, GuestPath: "libmyapp.so"},
	}
	got := BuildManifest(binPath, pkgFiles)

	require.Contains(t, got, "myapp:(contents:(host:")
	require.Contains(t, got, "libmyapp.so:(contents:(host:")
	require.Contains(t, got, "program:/program")

	lines := strings.Count(got, ":(contents:(host:")
	require.Equal(t, 3, lines)
}

func TestNewBuilder(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)
	require.NotNil(t, b)
	require.Equal(t, s, b.store)
}

func writeELF(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "hello")
	require.NoError(t, os.WriteFile(p, []byte{0x7f, 'E', 'L', 'F', 1, 2, 3, 4}, 0o755))
	return p
}

func fakeMkfsRun(t *testing.T) MkfsFunc {
	t.Helper()
	return func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd {
		f, err := os.Create(imgPath)
		require.NoError(t, err)
		_, err = f.WriteString("fake disk")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return successCmd()
	}
}

func TestBuild_Success(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)
	dir := t.TempDir()
	binPath := writeELF(t, dir)

	cfg := BuildConfig{
		Name:       "hello",
		Tag:        "v1",
		BinaryPath: binPath,
		MkfsRun:    fakeMkfsRun(t),
		Memory:     "512M",
		CPUs:       2,
	}

	m, err := b.Build(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, "hello", m.Name)
	require.Equal(t, "v1", m.Tag)
	require.Equal(t, "512M", m.Config.Memory)
	require.Equal(t, 2, m.Config.CPUs)
	require.True(t, m.DiskSize > 0)
	require.True(t, strings.HasPrefix(m.DiskDigest, "sha256:"))
}

func TestBuild_Defaults(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)
	dir := t.TempDir()
	binPath := writeELF(t, dir)

	cfg := BuildConfig{
		Name:       "hello",
		BinaryPath: binPath,
		MkfsRun:    fakeMkfsRun(t),
	}

	m, err := b.Build(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, "latest", m.Tag)
	require.Equal(t, "256M", m.Config.Memory)
	require.Equal(t, 1, m.Config.CPUs)
}

func TestBuild_WithPkgFiles(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)
	dir := t.TempDir()
	binPath := writeELF(t, dir)

	pkgFile := filepath.Join(dir, "node")
	require.NoError(t, os.WriteFile(pkgFile, []byte("nodebin"), 0o755))

	mkfsRun := func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd {
		require.Contains(t, manifest, "node:(contents:(host:")
		f, err := os.Create(imgPath)
		require.NoError(t, err)
		_, err = f.WriteString("x")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return successCmd()
	}

	cfg := BuildConfig{
		Name:       "hello",
		BinaryPath: binPath,
		MkfsRun:    mkfsRun,
		PkgFiles:   []pkg.File{{HostPath: pkgFile, GuestPath: "node"}},
	}

	m, err := b.Build(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, "hello", m.Name)
}

func TestBuild_ValidationError(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)

	cases := []struct {
		name string
		cfg  BuildConfig
	}{
		{"missing name", BuildConfig{BinaryPath: "/bin/true", MkfsRun: func(context.Context, string, string, string) *exec.Cmd { return nil }}},
		{"missing binary", BuildConfig{Name: "hello", MkfsRun: func(context.Context, string, string, string) *exec.Cmd { return nil }}},
		{"missing mkfs", BuildConfig{Name: "hello", BinaryPath: "/bin/true"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Build(context.Background(), tc.cfg)
			require.Error(t, err)
		})
	}
}

func TestBuild_NotELF(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)
	dir := t.TempDir()
	binPath := filepath.Join(dir, "notelf")
	require.NoError(t, os.WriteFile(binPath, []byte("not an elf binary"), 0o755))

	cfg := BuildConfig{
		Name:       "hello",
		BinaryPath: binPath,
		MkfsRun:    func(context.Context, string, string, string) *exec.Cmd { return successCmd() },
	}

	_, err := b.Build(context.Background(), cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not an ELF binary")
}

func TestBuild_ELFMissingFile(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)

	cfg := BuildConfig{
		Name:       "hello",
		BinaryPath: "/nonexistent/path/to/binary",
		MkfsRun:    func(context.Context, string, string, string) *exec.Cmd { return successCmd() },
	}

	_, err := b.Build(context.Background(), cfg)
	require.Error(t, err)
}

func TestBuild_MkfsFailure(t *testing.T) {
	s := makeStore(t)
	b := NewBuilder(s)
	dir := t.TempDir()
	binPath := writeELF(t, dir)

	cfg := BuildConfig{
		Name:       "hello",
		BinaryPath: binPath,
		MkfsRun:    func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd { return failureCmd() },
	}

	_, err := b.Build(context.Background(), cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mkfs")
}

func TestBuild_StorePutFailure(t *testing.T) {
	dir := t.TempDir()

	readonlyDir := filepath.Join(dir, "readonly")
	require.NoError(t, os.MkdirAll(readonlyDir, 0o755))
	sBad, err := NewStore(readonlyDir)
	require.NoError(t, err)

	lockedFile := filepath.Join(readonlyDir, "refs.json")
	require.NoError(t, os.WriteFile(lockedFile, []byte("{}"), 0o644))
	os.Chmod(lockedFile, 0o444)
	defer os.Chmod(lockedFile, 0o644)

	b := NewBuilder(sBad)
	binPath := writeELF(t, dir)

	cfg := BuildConfig{
		Name:       "hello",
		BinaryPath: binPath,
		MkfsRun:    fakeMkfsRun(t),
	}

	_, err = b.Build(context.Background(), cfg)
	require.Error(t, err)
}

func TestValidateBuildConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     BuildConfig
		wantErr bool
	}{
		{"valid", BuildConfig{Name: "a", BinaryPath: "/b", MkfsRun: func(context.Context, string, string, string) *exec.Cmd { return nil }}, false},
		{"no name", BuildConfig{BinaryPath: "/b", MkfsRun: func(context.Context, string, string, string) *exec.Cmd { return nil }}, true},
		{"no binary", BuildConfig{Name: "a", MkfsRun: func(context.Context, string, string, string) *exec.Cmd { return nil }}, true},
		{"no mkfs", BuildConfig{Name: "a", BinaryPath: "/b"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBuildConfig(tc.cfg)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCheckELF(t *testing.T) {
	t.Run("valid ELF header", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "elf")
		require.NoError(t, os.WriteFile(p, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o755))
		require.NoError(t, checkELF(p))
	})

	t.Run("invalid magic", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "notelf")
		require.NoError(t, os.WriteFile(p, []byte{0x00, 0x01, 0x02, 0x03}, 0o755))
		err := checkELF(p)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not an ELF binary")
	})

	t.Run("missing file", func(t *testing.T) {
		err := checkELF("/nonexistent/binary")
		require.Error(t, err)
	})

	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "empty")
		require.NoError(t, os.WriteFile(p, []byte{}, 0o755))
		err := checkELF(p)
		require.Error(t, err)
	})

	t.Run("short file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "short")
		require.NoError(t, os.WriteFile(p, []byte{0x7f, 'E'}, 0o755))
		err := checkELF(p)
		require.Error(t, err)
	})
}

func TestRunMkfs(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		mkfsRun := func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd {
			return successCmd()
		}
		require.NoError(t, runMkfs(context.Background(), mkfsRun, "/tmp/img", "/tmp/bin", "manifest"))
	})

	t.Run("failure", func(t *testing.T) {
		mkfsRun := func(ctx context.Context, imgPath, binaryPath string, manifest string) *exec.Cmd {
			return failureCmd()
		}
		err := runMkfs(context.Background(), mkfsRun, "/tmp/img", "/tmp/bin", "manifest")
		require.Error(t, err)
		require.Contains(t, err.Error(), "mkfs")
	})
}
