//go:build linux || (darwin && arm64)

package vm

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachPipe_WriteWithoutReaderNeverBlocks(t *testing.T) {
	p := newAttachPipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		line := bytes.Repeat([]byte("x"), 4096)
		for range 4 * attachPipeMaxBytes / len(line) {
			n, err := p.Write(line)
			if n != len(line) || err != nil {
				t.Errorf("write: n=%d err=%v", n, err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writes blocked with nobody reading")
	}
	p.mu.Lock()
	held := len(p.buf)
	p.mu.Unlock()
	assert.LessOrEqual(t, held, attachPipeMaxBytes)
}

func TestAttachPipe_KeepsNewestOutput(t *testing.T) {
	p := newAttachPipe()
	p.max = 8
	_, _ = p.Write([]byte("0123"))
	_, _ = p.Write([]byte("456789"))
	require.NoError(t, p.Close())
	got, err := io.ReadAll(p)
	require.NoError(t, err)
	assert.Equal(t, "23456789", string(got))

	// One write bigger than the buffer keeps its tail.
	p = newAttachPipe()
	p.max = 4
	_, _ = p.Write([]byte("abcdefgh"))
	require.NoError(t, p.Close())
	got, err = io.ReadAll(p)
	require.NoError(t, err)
	assert.Equal(t, "efgh", string(got))
}

func TestAttachPipe_CloseDeliversBufferedOutputThenError(t *testing.T) {
	p := newAttachPipe()
	_, _ = p.Write([]byte("last words\n"))
	exitErr := errors.New("guest exited with code 3")
	require.NoError(t, p.CloseWithError(exitErr))
	require.NoError(t, p.CloseWithError(errors.New("second close is ignored")))

	buf := make([]byte, 64)
	n, err := p.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "last words\n", string(buf[:n]))
	_, err = p.Read(buf)
	assert.ErrorIs(t, err, exitErr)

	// Writes after close are dropped, not failed.
	n, err = p.Write([]byte("late"))
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	_, err = p.Read(buf)
	assert.ErrorIs(t, err, exitErr)
}

func TestAttachPipe_ReaderWakesOnWriteAndClose(t *testing.T) {
	p := newAttachPipe()
	var wg sync.WaitGroup
	var got bytes.Buffer
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, readErr = io.Copy(&got, p)
	}()
	for range 100 {
		_, _ = p.Write([]byte("line\n"))
	}
	require.NoError(t, p.Close())
	wg.Wait()
	require.NoError(t, readErr) // io.Copy maps io.EOF to nil
	assert.Equal(t, bytes.Repeat([]byte("line\n"), 100), got.Bytes())
}
