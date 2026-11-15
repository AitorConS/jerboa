//go:build linux || (darwin && arm64)

package vm

import (
	"io"
	"sync"
)

// attachPipeMaxBytes bounds the console output an attach pipe holds for a
// reader that is slow, gone or not yet connected.
const attachPipeMaxBytes = 256 << 10

// attachPipe carries a VM's console output to an attach client. Unlike
// io.Pipe, a write never waits for a reader: os/exec copies the VMM's output
// into it, and cmd.Wait (and with it the monitor's teardown, the stopped
// state, --rm and the restart policy) waits for that copy to finish. With
// io.Pipe a console nobody read any more blocked the copy for good. Output a
// reader has not taken is kept up to attachPipeMaxBytes, dropping the oldest
// bytes first; all of it is in the VM's log buffer as well.
type attachPipe struct {
	mu     sync.Mutex
	ready  *sync.Cond // signaled when data arrives or the pipe closes
	buf    []byte
	max    int
	closed bool
	err    error // returned by Read once closed and drained
}

func newAttachPipe() *attachPipe {
	p := &attachPipe{max: attachPipeMaxBytes}
	p.ready = sync.NewCond(&p.mu)
	return p
}

// Write keeps p for the reader and returns at once. It never fails, so the
// VMM's output copy is never cut short by the console; after Close the bytes
// are dropped.
func (p *attachPipe) Write(b []byte) (int, error) {
	n := len(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || n == 0 {
		return n, nil
	}
	if n >= p.max {
		p.buf = append(p.buf[:0], b[n-p.max:]...)
	} else {
		if over := len(p.buf) + n - p.max; over > 0 {
			p.buf = p.buf[over:]
		}
		p.buf = append(p.buf, b...)
	}
	p.ready.Broadcast()
	return n, nil
}

// Read blocks until output is available or the pipe is closed. Buffered
// output is delivered before the close error.
func (p *attachPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && !p.closed {
		p.ready.Wait()
	}
	if len(p.buf) == 0 {
		return 0, p.err
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	if len(p.buf) == 0 {
		p.buf = nil
	}
	return n, nil
}

// CloseWithError ends the stream: Read returns err (io.EOF when nil) once
// the buffered output has been read.
func (p *attachPipe) CloseWithError(err error) error {
	if err == nil {
		err = io.EOF
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		p.err = err
		p.ready.Broadcast()
	}
	return nil
}

// Close ends the stream with io.EOF.
func (p *attachPipe) Close() error { return p.CloseWithError(nil) }
