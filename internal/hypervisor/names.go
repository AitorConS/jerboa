package hypervisor

import (
	"fmt"
	"strings"
)

// The microVM backend (Firecracker in this package's constants) has a
// different public name on each platform. On Linux it is Firecracker, the
// upstream VMM. On macOS it is Spark, Jerboa's Hypervisor.framework fork of
// it, and users only ever see "spark": in flags, config values, status,
// errors and logs. Code keeps the backend's internal name; these functions
// translate at the edges.

// Spark is the public name of the microVM backend on macOS.
const Spark = "spark"

// MicroVM is how this platform names the microVM backend in commands,
// config values and the daemon API: "spark" on macOS, "firecracker"
// elsewhere.
const MicroVM = microVM

// Label is the microVM backend's name in sentences: "Spark" on macOS,
// "Firecracker" elsewhere.
const Label = label

// DefaultBin is the microVM executable looked up when none is named.
const DefaultBin = microVM

// Public returns the name users see for an internal hypervisor name: the
// microVM backend's platform name, or the name unchanged.
func Public(name string) string {
	if name == Firecracker {
		return MicroVM
	}
	return name
}

// Values lists the accepted hypervisor values for messages and help text.
func Values() string {
	return Auto + ", " + QEMU + " or " + MicroVM
}

// Parse turns a configured or requested hypervisor value into its internal
// name, with "" for an unset value. The microVM backend's platform name is
// accepted, and on macOS also "firecracker", the name earlier releases
// wrote into config files and service definitions.
func Parse(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch {
	case v == "":
		return "", nil
	case v == Auto || v == QEMU:
		return v, nil
	case v == MicroVM || v == legacyMicroVM:
		return Firecracker, nil
	default:
		return "", fmt.Errorf("hypervisor must be %s, got %q", Values(), value)
	}
}
