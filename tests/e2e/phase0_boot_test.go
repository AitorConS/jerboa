//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/image"
	"github.com/stretchr/testify/require"
)

// TestPhase0KernelBoots verifies the kernel boots a hello-world ELF on QEMU.
// Requires: qemu-system-x86_64, kernel image built via `make kernel`.
func TestPhase0KernelBoots(t *testing.T) {
	requireBinary(t, "qemu-system-x86_64")

	helloELF := buildHelloWorld(t)
	image := buildImage(t, helloELF)

	output := runQEMU(t, image, 30*time.Second)

	require.Contains(t, output, "hello from unikernel",
		"kernel did not produce expected output")
}

func requireBinary(t *testing.T, name string) {
	t.Helper()
	_, err := exec.LookPath(name)
	require.NoError(t, err, "%s not found in PATH", name)
}

func buildHelloWorld(t *testing.T) string {
	t.Helper()
	src := "testdata/hello.c"
	out := t.TempDir() + "/hello"
	cmd := exec.Command("gcc", "-static", "-o", out, src)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run(), "failed to compile hello.c")
	return out
}

func buildImage(t *testing.T, binaryPath string) string {
	t.Helper()
	mkfs := "../../kernel/output/tools/bin/mkfs"
	if _, err := os.Stat(mkfs); err != nil {
		t.Skipf("kernel not built: %s not found (run make kernel && make mkfs first)", mkfs)
	}
	out := t.TempDir() + "/test.img"
	cmd := exec.Command(mkfs, "-b", "../../kernel/output/platform/pc/boot/boot.img", "-k", "../../kernel/output/platform/pc/bin/kernel.img", out)
	cmd.Stdin = strings.NewReader(image.BuildManifest(image.BuildConfig{BinaryPath: binaryPath}))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run(), "mkfs failed")
	return out
}

func runQEMU(t *testing.T, imagePath string, timeout time.Duration) string {
	t.Helper()

	var out bytes.Buffer
	cmd := exec.Command("qemu-system-x86_64",
		"-m", "256M",
		"-drive", "file="+imagePath+",format=raw,if=virtio",
		"-nographic",
		"-no-reboot",
	)
	cmd.Stdout = &out
	cmd.Stderr = &out

	require.NoError(t, cmd.Start())

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("QEMU timed out after %s\nOutput:\n%s", timeout, out.String())
	case <-done:
	}

	return strings.TrimSpace(out.String())
}
