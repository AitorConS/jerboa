// Package durable writes state files so that a crash, a full disk or a power
// cut leaves each file holding either what it held before or all of the new
// content, never a truncated or empty mix of the two.
//
// Every write goes to a temporary file in the same directory, is flushed to
// stable storage, and only then renamed over the target; the directory is
// flushed afterwards so the rename itself survives a power cut. Each of those
// steps can be made to fail through SetFault, which is how the stores built on
// this package are tested against the failures they are meant to survive.
package durable

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

// Step names one operation of a write, in the order they happen.
type Step int

const (
	// StepCreate creates the temporary file beside the target.
	StepCreate Step = iota
	// StepWrite writes the content to the temporary file.
	StepWrite
	// StepSync flushes the temporary file to stable storage.
	StepSync
	// StepClose closes the temporary file.
	StepClose
	// StepRename moves the temporary file over the target.
	StepRename
	// StepSyncDir flushes the directory, making the rename durable.
	StepSyncDir
)

// Steps lists every step of a write in order, for tests that fail each in turn.
var Steps = []Step{StepCreate, StepWrite, StepSync, StepClose, StepRename, StepSyncDir}

func (s Step) String() string {
	switch s {
	case StepCreate:
		return "create"
	case StepWrite:
		return "write"
	case StepSync:
		return "sync"
	case StepClose:
		return "close"
	case StepRename:
		return "rename"
	case StepSyncDir:
		return "syncdir"
	}
	return fmt.Sprintf("step(%d)", int(s))
}

// ErrCrash, returned by a fault, stops the write where it stands and leaves
// everything on disk as it is, the way a killed process or a power cut would:
// no cleanup runs, so a temporary file may stay behind.
var ErrCrash = errors.New("durable: simulated crash")

// Fault is called before each step of a write to path. A non-nil error fails
// the step: ErrCrash abandons the write mid-way, any other error (a full disk,
// an I/O error) is returned to the caller after the usual cleanup. A fault on
// StepWrite leaves half of the content in the temporary file first, as a short
// write does.
type Fault func(step Step, path string) error

var fault atomic.Pointer[Fault]

// SetFault installs f for every later write in the process and returns the
// function that removes it. It exists for tests; a nil f removes the fault.
func SetFault(f Fault) (restore func()) {
	var prev *Fault
	if f == nil {
		prev = fault.Swap(nil)
	} else {
		prev = fault.Swap(&f)
	}
	return func() { fault.Store(prev) }
}

func check(step Step, path string) error {
	if f := fault.Load(); f != nil {
		return (*f)(step, path)
	}
	return nil
}

// tempMarker is in the name of every temporary file this package creates.
const tempMarker = ".tmp-"

// WriteFile replaces path with data, with mode perm. After it returns nil the
// content survives a power cut; if it fails, or the machine dies while it
// runs, path still holds what it held before.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, true)
}

// WriteFileNoSync replaces path with data atomically but does not wait for
// stable storage. A killed process never leaves a partial file; a power cut
// shortly after may leave the previous content or, on some filesystems, an
// empty file, so use it only for state that is rebuilt when unreadable and is
// written too often to flush every time.
func WriteFileNoSync(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, false)
}

func write(path string, data []byte, perm os.FileMode, sync bool) (err error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if err := check(StepCreate, path); err != nil {
		return stepErr(StepCreate, path, err)
	}
	tmp, err := os.CreateTemp(dir, "."+base+tempMarker+"*")
	if err != nil {
		return stepErr(StepCreate, path, err)
	}
	name := tmp.Name()
	open := true
	defer func() {
		if errors.Is(err, ErrCrash) {
			// A crash cleans nothing up; the descriptor is closed only so the
			// test process does not leak it.
			if open {
				_ = tmp.Close()
			}
			return
		}
		if err != nil {
			if open {
				_ = tmp.Close()
			}
			_ = os.Remove(name)
		}
	}()

	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		return stepErr(StepCreate, path, err)
	}
	if ferr := check(StepWrite, path); ferr != nil {
		_, _ = tmp.Write(data[:len(data)/2])
		return stepErr(StepWrite, path, ferr)
	}
	if _, err := tmp.Write(data); err != nil {
		return stepErr(StepWrite, path, err)
	}
	if sync {
		if err := check(StepSync, path); err != nil {
			return stepErr(StepSync, path, err)
		}
		if err := tmp.Sync(); err != nil {
			return stepErr(StepSync, path, err)
		}
	}
	if err := check(StepClose, path); err != nil {
		return stepErr(StepClose, path, err)
	}
	open = false
	if err := tmp.Close(); err != nil {
		return stepErr(StepClose, path, err)
	}
	if err := check(StepRename, path); err != nil {
		return stepErr(StepRename, path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return stepErr(StepRename, path, err)
	}
	if sync {
		// The new content is in place from here on: a failure to flush the
		// directory is reported, but the file is not rolled back.
		if err := check(StepSyncDir, path); err != nil {
			if errors.Is(err, ErrCrash) {
				return stepErr(StepSyncDir, path, err)
			}
			return nil
		}
		SyncDir(dir)
	}
	return nil
}

func stepErr(step Step, path string, err error) error {
	return fmt.Errorf("durable write %s (%s): %w", filepath.Base(path), step, err)
}

// SyncDir flushes a directory so the names created, renamed or removed in it
// survive a power cut. It is best effort: Windows cannot flush a directory,
// and a filesystem that refuses must not fail a write that already happened.
func SyncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir) //nolint:gosec // G304: a directory the caller just wrote to.
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// IsTemp reports whether name is a temporary file left by an interrupted write.
func IsTemp(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, tempMarker)
}

// RemoveTemps deletes the temporary files interrupted writes left in dir and
// returns how many it removed and the bytes they held. Only call it while no
// write to dir can be in progress, such as when a store is opened.
func RemoveTemps(dir string) (count int, bytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if e.IsDir() || !IsTemp(e.Name()) {
			continue
		}
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			count++
			bytes += size
		}
	}
	return count, bytes
}
