package builder

import (
	"os"
	"os/exec"
	"sync"
)

var bwrapProbe struct {
	once sync.Once
	path string
}

// bwrapPath returns bubblewrap's path when it is installed and can create a
// sandbox here (user namespaces may be disabled), or "".
func bwrapPath() string {
	bwrapProbe.once.Do(func() {
		p, err := exec.LookPath("bwrap")
		if err != nil {
			return
		}
		if exec.Command(p, "--ro-bind", "/", "/", "--unshare-net", "--", "true").Run() == nil { //nolint:gosec,noctx // a fixed probe
			bwrapProbe.path = p
		}
	})
	return bwrapProbe.path
}

// confine runs the step under bubblewrap. Of the temporary directories it
// may write only to its own.
func confine(st Step, tmp string, sockets []string, name string, args []string) ([]string, bool) {
	bwrap := bwrapPath()
	if bwrap == "" {
		return nil, false
	}
	home, _ := os.UserHomeDir()
	return bwrapArgs(bwrap, st, hiddenPaths(home, st), []string{tmp}, sockets, name, args), true
}

func sandboxName() string {
	if bwrapPath() != "" {
		return "bubblewrap"
	}
	return ""
}
