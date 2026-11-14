// Package durabletest drives the failures a durable write can meet, so every
// store that persists state can be tested against a full disk, an I/O error
// and a crash at each step of each write, not only against the happy path.
//
// Faults are process-wide: tests that inject them must not run in parallel.
package durabletest

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
)

// Failure is one way a durable write can go wrong.
type Failure struct {
	// Name is usable as a subtest name.
	Name string
	Step durable.Step
	// Err is what the step fails with; durable.ErrCrash abandons the write
	// without cleanup, as a killed process or a power cut does.
	Err error
}

// Crash reports whether the failure is a crash rather than a returned error.
func (f Failure) Crash() bool { return f.Err == durable.ErrCrash } //nolint:errorlint // sentinel identity is the point

// Failures lists every step of a write failing three ways: the disk is full,
// the device returns an I/O error, and the process dies on the spot.
func Failures() []Failure {
	var out []Failure
	for _, step := range durable.Steps {
		out = append(out,
			Failure{Name: fmt.Sprintf("%s/disk-full", step), Step: step, Err: syscall.ENOSPC},
			Failure{Name: fmt.Sprintf("%s/io-error", step), Step: step, Err: syscall.EIO},
			Failure{Name: fmt.Sprintf("%s/crash", step), Step: step, Err: durable.ErrCrash},
		)
	}
	return out
}

// Injection reports what an injected failure did.
type Injection struct {
	writes atomic.Int64
	fired  atomic.Bool
}

// Fired reports whether the failure was reached.
func (i *Injection) Fired() bool { return i.fired.Load() }

// Writes returns how many durable writes began (matching or not).
func (i *Injection) Writes() int { return int(i.writes.Load()) }

// Inject makes the nth durable write (0 is the first) fail with f, once, and
// removes the fault when the test ends. Writes whose path match rejects are
// neither counted nor failed; a nil match accepts every path.
func Inject(t testing.TB, f Failure, nth int, match func(path string) bool) *Injection {
	t.Helper()
	inj := &Injection{}
	var seen atomic.Int64
	restore := durable.SetFault(func(step durable.Step, path string) error {
		if match != nil && !match(path) {
			return nil
		}
		if step == durable.StepCreate {
			inj.writes.Add(1)
			seen.Add(1)
		}
		if int(seen.Load())-1 != nth || step != f.Step {
			return nil
		}
		if !inj.fired.CompareAndSwap(false, true) {
			return nil
		}
		return f.Err
	})
	t.Cleanup(restore)
	return inj
}

// Count records how many durable writes an operation makes, without failing
// any, so a test can then fail each of them in turn.
func Count(t testing.TB, match func(path string) bool, op func()) int {
	t.Helper()
	var n atomic.Int64
	restore := durable.SetFault(func(step durable.Step, path string) error {
		if step == durable.StepCreate && (match == nil || match(path)) {
			n.Add(1)
		}
		return nil
	})
	defer restore()
	op()
	return int(n.Load())
}
