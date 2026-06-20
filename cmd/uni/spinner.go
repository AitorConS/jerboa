package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner is a TTY-aware progress indicator.
// In verbose mode or non-TTY mode it falls back to plain line output.
// Subprocess output is buffered in quiet mode and flushed only on failure.
type Spinner struct {
	w       io.Writer
	verbose bool
	tty     bool

	mu     sync.Mutex
	lbl    string
	buf    bytes.Buffer
	stopCh chan struct{}
	doneCh chan struct{}
}

func newSpinner(w io.Writer, verbose bool) *Spinner {
	tty := false
	if !verbose {
		if f, ok := w.(*os.File); ok {
			tty = isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
		}
	}
	return &Spinner{w: w, verbose: verbose, tty: tty}
}

// SubWriter returns a writer for subprocess output.
// In verbose mode it writes directly to the spinner's writer (raw output).
// In quiet mode it buffers so output is only shown when Fail is called.
func (s *Spinner) SubWriter() io.Writer {
	if s.verbose {
		return s.w
	}
	return &s.buf
}

// Start begins a spinner step with the given label.
func (s *Spinner) Start(label string) {
	s.mu.Lock()
	s.lbl = label
	s.buf.Reset()
	s.mu.Unlock()

	if s.verbose {
		return
	}
	if !s.tty {
		fmt.Fprintf(s.w, "  %s...\n", label)
		return
	}
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	go func() {
		defer close(s.doneCh)
		t := time.NewTicker(80 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				s.mu.Lock()
				lbl := s.lbl
				s.mu.Unlock()
				fmt.Fprintf(s.w, "\r\033[K  %s %s", spinFrames[i%len(spinFrames)], lbl)
				i++
			}
		}
	}()
}

// Update changes the label of a running spinner.
func (s *Spinner) Update(label string) {
	s.mu.Lock()
	s.lbl = label
	s.mu.Unlock()
	if !s.tty && !s.verbose {
		fmt.Fprintf(s.w, "  %s...\n", label)
	}
}

// Done stops the spinner and prints a success line.
func (s *Spinner) Done(label string) {
	s.stopSpin()
	if s.verbose {
		return
	}
	if s.tty {
		fmt.Fprintf(s.w, "\r\033[K  ✓ %s\n", label)
	} else {
		fmt.Fprintf(s.w, "  ✓ %s\n", label)
	}
	s.mu.Lock()
	s.buf.Reset()
	s.mu.Unlock()
}

// Fail stops the spinner with an error marker and flushes any buffered subprocess output.
func (s *Spinner) Fail(label string) {
	s.stopSpin()
	if s.tty {
		fmt.Fprintf(s.w, "\r\033[K  ✗ %s\n", label)
	} else {
		fmt.Fprintf(s.w, "  ✗ %s\n", label)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > 0 {
		fmt.Fprintln(s.w)
		_, _ = s.w.Write(s.buf.Bytes())
		s.buf.Reset()
	}
}

func (s *Spinner) stopSpin() {
	if s.stopCh != nil {
		close(s.stopCh)
		<-s.doneCh
		s.stopCh = nil
		s.doneCh = nil
	}
}
