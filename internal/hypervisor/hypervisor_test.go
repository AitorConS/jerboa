package hypervisor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChoosePrefersFirecrackerWhereverItCanRun(t *testing.T) {
	onPath := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	beside := func(path string) bool { return path == "/bundle/bin/firecracker" }
	never := func(string) bool { return false }

	got := choose("firecracker", "", probe{lookPath: onPath, isExecutable: never})
	require.Equal(t, Choice{Name: Firecracker, FCBin: "/usr/bin/firecracker", Reason: "this host can run " + Label}, got)

	// A bundle keeps the VMM beside the daemon, off PATH.
	got = choose("firecracker", "/bundle/bin", probe{lookPath: notOnPath, isExecutable: beside})
	require.Equal(t, Firecracker, got.Name)
	require.Equal(t, "/bundle/bin/firecracker", got.FCBin)

	got = choose("firecracker", "/bundle/bin", probe{lookPath: notOnPath, isExecutable: never})
	require.Equal(t, QEMU, got.Name)
	require.Contains(t, got.Reason, "no firecracker binary")

	got = choose("firecracker", "", probe{unsupported: "KVM is not available", lookPath: onPath, isExecutable: never})
	require.Equal(t, Choice{Name: QEMU, Reason: "KVM is not available"}, got)
}

func TestDefaultNamesAHypervisor(t *testing.T) {
	c := Default("firecracker-that-does-not-exist", "")
	require.Equal(t, QEMU, c.Name)
	require.NotEmpty(t, c.Reason)
}

func TestParseAcceptsThePlatformNameAndTheLegacyOne(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""}, {"auto", Auto}, {"QEMU", QEMU}, {MicroVM, Firecracker}, {" " + MicroVM + " ", Firecracker},
		// What earlier macOS releases wrote, and what Linux calls it.
		{"firecracker", Firecracker},
	} {
		in, want := c.in, c.want
		got, err := Parse(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := Parse("xen")
	require.ErrorContains(t, err, Values())
	require.Equal(t, MicroVM, Public(Firecracker))
	require.Equal(t, QEMU, Public(QEMU))
}
