package builder

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

const sandboxExec = "/usr/bin/sandbox-exec"

var seatbeltProbe struct {
	once sync.Once
	ok   bool
}

// seatbeltAvailable reports whether sandbox-exec can confine a process here.
func seatbeltAvailable() bool {
	seatbeltProbe.once.Do(func() {
		seatbeltProbe.ok = exec.Command(sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true").Run() == nil //nolint:noctx // a fixed probe
	})
	return seatbeltProbe.ok
}

// confine runs the step under sandbox-exec. It may write to its private
// temporary directory and to the per-user cache directory under
// /var/folders that Apple's developer tools use (DARWIN_USER_CACHE_DIR).
func confine(st Step, tmp string, sockets []string, name string, args []string) ([]string, bool) {
	if !seatbeltAvailable() {
		return nil, false
	}
	home, _ := os.UserHomeDir()
	scratch := []string{tmp}
	if out, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_CACHE_DIR").Output(); err == nil { //nolint:noctx // a fixed query
		if d := strings.TrimSpace(string(out)); d != "" {
			scratch = append(scratch, d)
		}
	}
	profile := seatbeltProfile(st, hiddenPaths(home, st), scratch, sockets)
	return append([]string{sandboxExec, "-p", profile, name}, args...), true
}

func sandboxName() string {
	if seatbeltAvailable() {
		return "sandbox-exec"
	}
	return ""
}
