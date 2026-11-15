// Package hypervisor picks the hypervisor a daemon runs when none was chosen.
package hypervisor

import (
	"os"
	"os/exec"
	"path/filepath"
)

const (
	QEMU        = "qemu"
	Firecracker = "firecracker"
	// Auto is the configured value that leaves the choice to the daemon. It
	// is also what an unset hypervisor means.
	Auto = "auto"
)

// Choice is the hypervisor to run and why.
type Choice struct {
	Name string
	// FCBin is the Firecracker binary found, when Name is Firecracker.
	FCBin string
	// Reason says what decided it, for the daemon's log and for errors.
	Reason string
}

// Default picks Firecracker whenever this host can run it, and QEMU
// otherwise. fcBin is the Firecracker binary to look for, by name or path;
// binDir, when not empty, is also searched (the directory of the daemon in a
// bundle).
func Default(fcBin, binDir string) Choice {
	return choose(fcBin, binDir, hostProbe())
}

// probe is what the choice depends on, so tests can state a host.
type probe struct {
	// unsupported is why this host cannot run Firecracker at all, or "".
	unsupported  string
	lookPath     func(string) (string, error)
	isExecutable func(string) bool
}

func choose(fcBin, binDir string, p probe) Choice {
	if p.unsupported != "" {
		return Choice{Name: QEMU, Reason: p.unsupported}
	}
	bin := ""
	if path, err := p.lookPath(fcBin); err == nil {
		bin = path
	} else if binDir != "" && p.isExecutable(filepath.Join(binDir, filepath.Base(fcBin))) {
		bin = filepath.Join(binDir, filepath.Base(fcBin))
	}
	if bin == "" {
		return Choice{Name: QEMU, Reason: "no " + fcBin + " binary was found"}
	}
	return Choice{Name: Firecracker, FCBin: bin, Reason: "this host can run Firecracker"}
}

func hostProbe() probe {
	return probe{
		unsupported: unsupported(),
		lookPath:    exec.LookPath,
		isExecutable: func(path string) bool {
			info, err := os.Stat(path)
			return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
		},
	}
}
