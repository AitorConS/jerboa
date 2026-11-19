//go:build unix

package builder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own and returns what
// to call once it has ended: it kills whatever is left in the group, so a
// confined step cannot leave a process running behind the build (on macOS
// one would keep the step's confinement but could still change the project
// while the build reads it). cancel must cancel cmd's context; it is called
// when this process is interrupted, so the step's group is killed rather
// than left running. The interrupt is then delivered to this process again
// once the step is gone, so it ends as it would have.
//
// A process that leaves the group on purpose (setsid) is not reached; this
// is defense in depth, not confinement.
func ownProcessGroup(cmd *exec.Cmd, cancel context.CancelFunc) func() {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	// Run by exec's own watcher once the process exists.
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }

	// Outside the terminal's foreground group, the step no longer receives
	// the terminal's Ctrl-C itself.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stop := make(chan struct{})
	exited := make(chan os.Signal, 1)
	go func() {
		select {
		case s := <-sigs:
			cancel()
			exited <- s
		case <-stop:
			exited <- nil
		}
	}()
	return func() {
		close(stop)
		caught := <-exited
		signal.Stop(sigs)
		if cmd.Process != nil {
			_ = killProcessGroup(cmd.Process)
		}
		if s, ok := caught.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), s)
		}
	}
}

// killProcessGroup kills the process group p leads. A group that is already
// gone is not an error.
func killProcessGroup(p *os.Process) error {
	if p == nil || p.Pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group %d: %w", p.Pid, err)
	}
	return nil
}
