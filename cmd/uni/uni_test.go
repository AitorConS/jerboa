package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/network"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/AitorConS/unikernel-engine/internal/vm"
	"github.com/AitorConS/unikernel-engine/internal/volume"
	"github.com/stretchr/testify/require"
)

// fakeQEMUCmd returns a vm.CommandFunc that spawns a fake QEMU process.
// Cross-platform: uses sleep on Unix and PowerShell on Windows.
func fakeQEMUCmd() vm.CommandFunc {
	return func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.Command("powershell", "-Command", "while ($true) { Start-Sleep -Seconds 3600 }")
		}
		return exec.Command("sleep", "3600")
	}
}

const (
	testTimeout = 5 * time.Second
	dialPoll    = 10 * time.Millisecond
)

// startDaemon launches an in-process daemon and returns a connected client.
func startDaemon(t *testing.T) (*api.Client, string) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "unid.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd()))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := api.NewServer(mgr, netStore, socketPath, nil, "")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(cancel)

	var client *api.Client
	require.Eventually(t, func() bool {
		var dialErr error
		client, dialErr = api.Dial(socketPath)
		return dialErr == nil
	}, testTimeout, dialPoll, "daemon did not start")
	t.Cleanup(func() { _ = client.Close() })
	return client, socketPath
}

// execRoot runs the root cobra command with the given args and returns stdout.
func execRoot(t *testing.T, socketPath, storePath string, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	allArgs := []string{"--socket", socketPath, "--store", storePath}
	allArgs = append(allArgs, args...)
	root.SetArgs(allArgs)
	err := root.Execute()
	require.NoError(t, err, "command output: %s", buf.String())
	return buf.String()
}

// makeStore creates an image store with one fake image and returns store path + disk path.
func makeStore(t *testing.T) (storePath, diskPath string) {
	t.Helper()
	storePath = t.TempDir()

	diskPath = filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(diskPath, []byte("fake disk content"), 0o600))

	store, err := image.NewStore(storePath)
	require.NoError(t, err)

	m := image.Manifest{
		SchemaVersion: 1,
		Name:          "hello",
		Tag:           "latest",
		Created:       time.Now(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123",
		DiskSize:      17,
	}
	require.NoError(t, store.Put("hello", "latest", m, diskPath))
	return storePath, diskPath
}

// startRegistry starts an in-process registry HTTP server backed by a fresh store.
func startRegistry(t *testing.T) (url, storePath string) {
	t.Helper()
	storePath = t.TempDir()
	store, err := image.NewStore(storePath)
	require.NoError(t, err)
	srv := registry.NewServer(store)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, storePath
}

func startSecureRegistry(t *testing.T, token string, withOCI bool) (url, storePath string) {
	t.Helper()
	storePath = t.TempDir()
	store, err := image.NewStore(storePath)
	require.NoError(t, err)

	opts := []registry.Option{registry.WithBearerToken(token, "uni-test")}
	if withOCI {
		blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
		require.NoError(t, err)
		ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
		require.NoError(t, err)
		opts = append(opts, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore))
	}

	srv := registry.NewServer(store, opts...)
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, storePath
}

// --- ps ---

func TestPs_Empty(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()
	out := execRoot(t, socketPath, storePath, "ps")
	require.Contains(t, out, "ID")
	require.Contains(t, out, "STATE")
}

func TestPs_WithVM(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	out := execRoot(t, socketPath, storePath, "ps")
	require.Contains(t, out, info.ID)
}

func TestPs_JSON(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()
	out := execRoot(t, socketPath, storePath, "--output", "json", "ps")
	require.Contains(t, out, "[")
}

// --- stop ---

func TestStop_Graceful(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	out := execRoot(t, socketPath, storePath, "stop", info.ID)
	require.Empty(t, strings.TrimSpace(out))
}

func TestStop_Force(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	execRoot(t, socketPath, storePath, "stop", "--force", info.ID)
}

func TestStop_NoVM(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "stop", "nonexistent-id")
	require.Contains(t, msg, "stop")
}

func TestLogs_NoVM(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "logs", "nonexistent-id")
	require.Contains(t, msg, "logs")
}

func TestInspect_NoVM(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "inspect", "nonexistent-id")
	require.Contains(t, msg, "inspect")
}

// --- logs ---

func TestLogs(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	_ = execRoot(t, socketPath, storePath, "logs", info.ID)
}

// --- inspect ---

func TestInspect(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	out := execRoot(t, socketPath, storePath, "inspect", info.ID)
	require.Contains(t, out, info.ID)
	require.Contains(t, out, `"state"`)
}

// --- rm ---

func TestRm_StoppedVM(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	require.NoError(t, client.Stop(context.Background(), info.ID, true))
	require.Eventually(t, func() bool {
		got, gErr := client.Get(context.Background(), info.ID)
		return gErr == nil && got.State == "stopped"
	}, testTimeout, dialPoll)

	execRoot(t, socketPath, storePath, "rm", info.ID)
}

func TestRegistryPushPull_WithTLSAndAuth(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath, _ := makeStore(t)
	registryURL, _ := startSecureRegistry(t, "secret-token", false)

	out := execRoot(t, socketPath, storePath,
		"--registry-token", "secret-token",
		"--registry-insecure",
		"push", "hello:latest", registryURL,
	)
	require.Contains(t, out, "pushed hello:latest")

	pullStore := t.TempDir()
	out = execRoot(t, socketPath, pullStore,
		"--registry-token", "secret-token",
		"--registry-insecure",
		"pull", "hello:latest", registryURL,
	)
	require.Contains(t, out, "hello:latest")
}

func TestRegistrySearch_WithTLSAndAuth(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath, _ := makeStore(t)
	registryURL, _ := startSecureRegistry(t, "secret-token", true)

	execRoot(t, socketPath, storePath,
		"--registry-token", "secret-token",
		"--registry-insecure",
		"push", "hello:latest", registryURL,
	)

	out := execRoot(t, socketPath, storePath,
		"--registry-token", "secret-token",
		"--registry-insecure",
		"search", registryURL+"/hello",
	)
	require.Contains(t, out, "hello")
}

// --- exec ---

func TestExec_Signal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal delivery not supported on Windows")
	}
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	execRoot(t, socketPath, storePath, "exec", "--signal", "SIGTERM", info.ID)
}

// --- run ---

func TestRun_FilePath(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	diskPath := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(diskPath, []byte("fake"), 0o600))

	out := execRoot(t, socketPath, storePath, "run", diskPath)
	require.NotEmpty(t, strings.TrimSpace(out))
}

// --- images ---

func TestImages_Empty(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "images")
	require.Contains(t, out, "DIGEST")
}

func TestImages_WithEntry(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath, _ := makeStore(t)

	out := execRoot(t, socketPath, storePath, "images")
	require.Contains(t, out, "hello")
	require.Contains(t, out, "latest")
}

// --- rmi ---

func TestRmi(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath, _ := makeStore(t)

	out := execRoot(t, socketPath, storePath, "rmi", "hello:latest")
	require.Contains(t, out, "hello:latest")
}

// --- push / pull ---

func TestPush_Pull(t *testing.T) {
	_, socketPath := startDaemon(t)
	srcStorePath, _ := makeStore(t)
	registryURL, dstStorePath := startRegistry(t)

	execRoot(t, socketPath, srcStorePath, "push", "hello:latest", registryURL)
	out := execRoot(t, socketPath, dstStorePath, "pull", "hello:latest", registryURL)
	require.Contains(t, out, "hello")
}

// --- build error paths ---

// execRootExpectError runs the command and returns the error string.
func execRootExpectError(t *testing.T, socketPath, storePath string, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	allArgs := []string{"--socket", socketPath, "--store", storePath}
	allArgs = append(allArgs, args...)
	root.SetArgs(allArgs)
	err := root.Execute()
	require.Error(t, err)
	return err.Error()
}

func TestBuild_MissingMkfs(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	binaryPath := filepath.Join(t.TempDir(), "app")
	require.NoError(t, os.WriteFile(binaryPath, []byte("\x7fELF"), 0o755))

	// mkfs doesn't exist → Build() fails, covers error path in newBuildCmd
	msg := execRootExpectError(t, socketPath, storePath,
		"build", "--name", "myapp", "--mkfs", "/nonexistent/mkfs", binaryPath)
	require.Contains(t, msg, "build")
}

func TestExec_NoVM(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "exec", "--signal", "SIGTERM", "nonexistent-id")
	require.Contains(t, msg, "exec")
}

func TestRm_RunningVM(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img", Memory: "256M",
	})
	require.NoError(t, err)

	msg := execRootExpectError(t, socketPath, storePath, "rm", info.ID)
	require.Contains(t, msg, "rm")
}

// --- resolveImage ---

func TestResolveImage_FilePath(t *testing.T) {
	p := "/some/path/disk.img"
	got, err := resolveImage(p, t.TempDir(), "256M", 1)
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestResolveImage_NameTag(t *testing.T) {
	storePath, _ := makeStore(t)
	got, err := resolveImage("hello:latest", storePath, "256M", 1)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	_, statErr := os.Stat(got)
	require.NoError(t, statErr)
}

func TestResolveImage_NotFound(t *testing.T) {
	_, err := resolveImage("missing:tag", t.TempDir(), "256M", 1)
	require.Error(t, err)
}

// --- helpers ---

func TestIsFilePath(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"/absolute", true},
		{"./relative", true},
		{"../up", true},
		{".", true},
		{"name:tag", false},
		{"myimage", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, isFilePath(tc.s), "isFilePath(%q)", tc.s)
	}
}

func TestFormatSize(t *testing.T) {
	require.Equal(t, "1.0GB", formatSize(1<<30))
	require.Equal(t, "1.0MB", formatSize(1<<20))
	require.Equal(t, "1.0KB", formatSize(1<<10))
	require.Equal(t, "512B", formatSize(512))
}

func TestShortDigest(t *testing.T) {
	require.Equal(t, "sha256:abcdef12345", shortDigest("sha256:abcdef12345"))
	long := "sha256:" + strings.Repeat("a", 64)
	require.Equal(t, long[:19], shortDigest(long))
}

// --- buildEnv ---

func TestBuildEnv_FlagsOnly(t *testing.T) {
	got, err := buildEnv([]string{"FOO=bar", "PORT=8080"}, "")
	require.NoError(t, err)
	require.Equal(t, []string{"FOO=bar", "PORT=8080"}, got)
}

func TestBuildEnv_FileOnly(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.env")
	require.NoError(t, err)
	_, _ = f.WriteString("# comment\nKEY=value\n\nANOTHER=1\n")
	require.NoError(t, f.Close())

	got, err := buildEnv(nil, f.Name())
	require.NoError(t, err)
	require.Equal(t, []string{"KEY=value", "ANOTHER=1"}, got)
}

func TestBuildEnv_FlagsAndFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.env")
	require.NoError(t, err)
	_, _ = f.WriteString("FROM_FILE=yes\n")
	require.NoError(t, f.Close())

	got, err := buildEnv([]string{"INLINE=1"}, f.Name())
	require.NoError(t, err)
	require.Equal(t, []string{"INLINE=1", "FROM_FILE=yes"}, got)
}

func TestBuildEnv_MissingFile(t *testing.T) {
	_, err := buildEnv(nil, "/nonexistent/file.env")
	require.Error(t, err)
}

// --- parseVolumeSpec ---

func TestParseVolumeSpec_ValidRW(t *testing.T) {
	store, err := makeVolumeStore(t)
	require.NoError(t, err)

	spec, err := parseVolumeSpec("data:/mnt/data", store)
	require.NoError(t, err)
	require.Equal(t, "/mnt/data", spec.GuestPath)
	require.False(t, spec.ReadOnly)
}

func TestParseVolumeSpec_ValidRO(t *testing.T) {
	store, err := makeVolumeStore(t)
	require.NoError(t, err)

	spec, err := parseVolumeSpec("data:/mnt/data:ro", store)
	require.NoError(t, err)
	require.True(t, spec.ReadOnly)
}

func TestParseVolumeSpec_NotFound(t *testing.T) {
	store, err := makeVolumeStore(t)
	require.NoError(t, err)

	_, err = parseVolumeSpec("missing:/mnt", store)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestParseVolumeSpec_BadFormat(t *testing.T) {
	store, err := makeVolumeStore(t)
	require.NoError(t, err)

	_, err = parseVolumeSpec("nocodon", store)
	require.Error(t, err)
}

// --- volume CLI ---

func TestVolume_CreateAndList(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "volume", "create", "--size", "1M", "myvol")
	require.Contains(t, out, "myvol")

	out = execRoot(t, socketPath, storePath, "volume", "ls")
	require.Contains(t, out, "myvol")
}

func TestVolume_Inspect(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "volume", "create", "--size", "1M", "inspect-vol")
	out := execRoot(t, socketPath, storePath, "volume", "inspect", "inspect-vol")
	require.Contains(t, out, "inspect-vol")
}

func TestVolume_Remove(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "volume", "create", "--size", "1M", "todel")
	execRoot(t, socketPath, storePath, "volume", "rm", "todel")

	out := execRoot(t, socketPath, storePath, "volume", "ls")
	require.NotContains(t, out, "todel")
}

// --- run with new flags ---

func TestRun_WithPortAndEnv(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	diskPath := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(diskPath, []byte("fake"), 0o600))

	out := execRoot(t, socketPath, storePath, "run",
		"-p", "8080:80",
		"-e", "FOO=bar",
		"--name", "myvm",
		diskPath,
	)
	require.NotEmpty(t, strings.TrimSpace(out))
}

func TestRun_WithInvalidPort(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	diskPath := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(diskPath, []byte("fake"), 0o600))

	msg := execRootExpectError(t, socketPath, storePath, "run", "-p", "bad", diskPath)
	require.Contains(t, msg, "port")
}

func TestGatewayIP(t *testing.T) {
	cases := []struct {
		ip   string
		want string
	}{
		{"10.0.0.5", "10.0.0.1"},
		{"192.168.1.100", "192.168.1.1"},
		{"172.16.0.50", "172.16.0.1"},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, gatewayIP(tc.ip), "gatewayIP(%q)", tc.ip)
	}
}

func TestParseHealthCheck(t *testing.T) {
	cases := []struct {
		input string
		want  api.HealthCheckSpec
	}{
		{"tcp:8080", api.HealthCheckSpec{Type: "tcp", Port: 8080}},
		{"http:3000:/healthz", api.HealthCheckSpec{Type: "http", Port: 3000, Path: "/healthz"}},
		{"http:80", api.HealthCheckSpec{Type: "http", Port: 80}},
		{"http:8080:health", api.HealthCheckSpec{Type: "http", Port: 8080, Path: "/health"}},
	}
	for _, tc := range cases {
		got, err := parseHealthCheck(tc.input)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "parseHealthCheck(%q)", tc.input)
	}
}

func TestParseHealthCheck_Invalid(t *testing.T) {
	_, err := parseHealthCheck("badformat")
	require.Error(t, err)

	_, err = parseHealthCheck("udp:8080")
	require.Error(t, err)

	_, err = parseHealthCheck("tcp:abc")
	require.Error(t, err)

	_, err = parseHealthCheck(":8080")
	require.Error(t, err)
}

// --- helpers ---

func makeVolumeStore(t *testing.T) (*volume.Store, error) {
	t.Helper()
	dir := t.TempDir()
	store, err := volume.NewStore(dir)
	if err != nil {
		return nil, err
	}
	_, err = store.Create("data", 1<<20)
	return store, err
}
