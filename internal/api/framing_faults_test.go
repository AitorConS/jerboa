package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What must hold when the connection under a framed stream fails: the reader
// never reports a stream that was cut as complete, never hands out bytes that
// were not sent, and the writer reports every write it could not finish.

// framedStream returns the wire form of payload split into frames of at most
// chunk bytes, terminator included.
func framedStream(t *testing.T, payload []byte, chunk int) []byte {
	t.Helper()
	var wire bytes.Buffer
	fw := NewFrameWriter(&wire)
	for off := 0; off < len(payload); off += chunk {
		_, err := fw.Write(payload[off:min(off+chunk, len(payload))])
		require.NoError(t, err)
	}
	require.NoError(t, fw.Close())
	return wire.Bytes()
}

// failingReader serves data and then fails with err instead of ending, the
// way a socket does when the peer resets the connection.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestFrameReader_StreamCutAtEveryOffset(t *testing.T) {
	payload := []byte("the quick brown fox jumps over the lazy dog")
	wire := framedStream(t, payload, 7)

	for cut := range len(wire) {
		// The peer closes the connection after cut bytes.
		got, err := io.ReadAll(NewFrameReader(bytes.NewReader(wire[:cut])))
		require.Error(t, err, "a stream cut at byte %d of %d read as complete", cut, len(wire))
		require.True(t, bytes.HasPrefix(payload, got), "cut at %d delivered bytes that were never sent: %q", cut, got)

		// The connection is reset after cut bytes.
		got, err = io.ReadAll(NewFrameReader(&failingReader{data: wire[:cut], err: syscall.ECONNRESET}))
		require.ErrorIs(t, err, syscall.ECONNRESET, "cut at %d", cut)
		require.True(t, bytes.HasPrefix(payload, got))
	}

	got, err := io.ReadAll(NewFrameReader(bytes.NewReader(wire)))
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestFrameReader_TruncatedPayloadIsNotACleanEnd(t *testing.T) {
	// The header promises 10 bytes and the connection ends after 5. Callers
	// that copy with io.Copy stop silently on the io.EOF value itself, so the
	// reader must hand them something else.
	fr := NewFrameReader(bytes.NewReader(frame(10, []byte("only5"))))
	buf := make([]byte, 64)
	n, err := fr.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "only5", string(buf[:n]))
	_, err = fr.Read(buf)
	require.Error(t, err)
	require.NotEqual(t, io.EOF, err, "a frame cut short must not end the stream cleanly") //nolint:errorlint,testifylint // the identity io.Copy tests is the point

	var out bytes.Buffer
	_, err = io.Copy(&out, NewFrameReader(bytes.NewReader(frame(10, []byte("only5")))))
	require.Error(t, err, "io.Copy took a truncated frame for the end of the stream")
}

func TestFrameReader_SlowAndFragmentedTransport(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	wire := append(framedStream(t, payload, 1000), "NEXT"...)

	transports := map[string]func(io.Reader) io.Reader{
		"one byte per read":   iotest.OneByteReader,
		"half of each read":   iotest.HalfReader,
		"data with the error": iotest.DataErrReader,
	}
	for name, wrap := range transports {
		t.Run(name, func(t *testing.T) {
			src := bytes.NewReader(wire)
			got, err := io.ReadAll(NewFrameReader(wrap(src)))
			require.NoError(t, err)
			require.True(t, bytes.Equal(payload, got), "payload damaged by a fragmented transport")
			if name == "data with the error" {
				return // that wrapper reads ahead of what it hands out
			}
			// What follows the stream belongs to the next message.
			rest, err := io.ReadAll(src)
			require.NoError(t, err)
			assert.Equal(t, "NEXT", string(rest))
		})
	}
}

func TestFrameReader_TimeoutMidFrameIsReported(t *testing.T) {
	// The peer stalls inside a frame and the read deadline fires.
	wire := framedStream(t, []byte("0123456789"), 10)
	fr := NewFrameReader(iotest.TimeoutReader(iotest.OneByteReader(bytes.NewReader(wire))))
	_, err := io.ReadAll(fr)
	require.ErrorIs(t, err, iotest.ErrTimeout)
}

// countingReader records how often it is read.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func TestFrameReader_StaysEndedAfterTerminator(t *testing.T) {
	src := &countingReader{r: bytes.NewReader(append(framedStream(t, []byte("abc"), 3), `{"done":true}`...))}
	fr := NewFrameReader(src)
	got, err := io.ReadAll(fr)
	require.NoError(t, err)
	require.Equal(t, "abc", string(got))

	reads := src.reads
	for range 3 {
		n, err := fr.Read(make([]byte, 16))
		require.Zero(t, n)
		require.Equal(t, io.EOF, err) //nolint:errorlint,testifylint // a Reader ends with the io.EOF value itself
	}
	assert.Equal(t, reads, src.reads, "an ended stream must leave the connection to the next message")
}

func TestFrameReader_EmptyBufferLosesNothing(t *testing.T) {
	fr := NewFrameReader(bytes.NewReader(framedStream(t, []byte("abcdef"), 3)))
	n, err := fr.Read(nil)
	require.NoError(t, err)
	require.Zero(t, n)
	got, err := io.ReadAll(fr)
	require.NoError(t, err)
	assert.Equal(t, "abcdef", string(got))
}

func TestFrameReader_LargestFrameIsAcceptedButStillNeedsItsPayload(t *testing.T) {
	// A header of exactly the limit is legal; the stream behind it is cut.
	fr := NewFrameReader(bytes.NewReader(frame(maxFrameSize, []byte("x"))))
	got, err := io.ReadAll(fr)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrFrameTooLarge)
	assert.Equal(t, "x", string(got))
}

func TestFrameReader_NeverReadsPastAFramesEnd(t *testing.T) {
	// Two frames back to back, read through a buffer larger than both: the
	// second header must not be swallowed as payload of the first.
	wire := append(frame(2, []byte("ab")), frame(3, []byte("cde"))...)
	fr := NewFrameReader(bytes.NewReader(wire))
	buf := make([]byte, 1024)
	n, err := fr.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ab", string(buf[:n]))
	n, err = fr.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "cde", string(buf[:n]))
}

// faultyWriter fails the write that would take it past limit bytes, keeping
// what fit, as a connection does when its peer goes away or a disk fills.
type faultyWriter struct {
	buf   bytes.Buffer
	limit int
	err   error
}

func (w *faultyWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if len(p) <= room {
		return w.buf.Write(p)
	}
	w.buf.Write(p[:max(room, 0)])
	return max(room, 0), w.err
}

func TestFrameWriter_FailureAtEveryByteIsReportedAndSeenByTheReader(t *testing.T) {
	payload := []byte("build context that must arrive whole")
	full := framedStream(t, payload, 9)

	for limit := range len(full) {
		w := &faultyWriter{limit: limit, err: syscall.EPIPE}
		fw := NewFrameWriter(w)
		var werr error
		for off := 0; off < len(payload) && werr == nil; off += 9 {
			_, werr = fw.Write(payload[off:min(off+9, len(payload))])
		}
		if werr == nil {
			werr = fw.Close()
		}
		require.ErrorIs(t, werr, syscall.EPIPE, "the write cut at byte %d went unreported", limit)

		// What did reach the other side is a prefix of the stream, and its
		// reader must not mistake it for the whole context.
		require.True(t, bytes.HasPrefix(full, w.buf.Bytes()))
		got, rerr := io.ReadAll(NewFrameReader(&w.buf))
		require.Error(t, rerr, "the receiver took a stream cut at byte %d for a complete one", limit)
		require.True(t, bytes.HasPrefix(payload, got))
	}
}

func TestFrameWriter_ReportsHowMuchWasFramed(t *testing.T) {
	// The payload fails after its header went out: nothing of it counts as
	// written, so a caller cannot believe part of it arrived as a frame.
	w := &faultyWriter{limit: 6, err: syscall.ENOSPC}
	n, err := NewFrameWriter(w).Write([]byte("0123456789"))
	require.ErrorIs(t, err, syscall.ENOSPC)
	require.Zero(t, n)
}

// shortWriter takes at most max bytes per call and reports no error, which a
// well-behaved writer never does but a wrapped one might.
type shortWriter struct {
	buf bytes.Buffer
	max int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p[:min(len(p), w.max)])
}

func TestFrameWriter_SilentShortWriteIsAnError(t *testing.T) {
	for _, limit := range []int{0, 1, 3, 4, 7} {
		w := &shortWriter{max: limit}
		fw := NewFrameWriter(w)
		_, err := fw.Write([]byte("0123456789"))
		require.ErrorIs(t, err, io.ErrShortWrite, "a writer taking %d bytes per call lost data silently", limit)
	}
	w := &shortWriter{max: 3}
	require.ErrorIs(t, NewFrameWriter(w).Close(), io.ErrShortWrite)

	// A writer that takes everything is not affected.
	w = &shortWriter{max: 1 << 20}
	fw := NewFrameWriter(w)
	n, err := fw.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.NoError(t, fw.Close())
}

func TestFrameWriter_EmptyWriteDoesNotEndTheStream(t *testing.T) {
	// A zero-length frame is the terminator: an empty Write in the middle of
	// a stream must put nothing on the wire, or the reader stops there and
	// takes the rest of the context for the next message.
	var wire bytes.Buffer
	fw := NewFrameWriter(&wire)
	_, err := fw.Write([]byte("before"))
	require.NoError(t, err)
	n, err := fw.Write(nil)
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = fw.Write([]byte{})
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = fw.Write([]byte("after"))
	require.NoError(t, err)
	require.NoError(t, fw.Close())

	got, err := io.ReadAll(NewFrameReader(&wire))
	require.NoError(t, err)
	assert.Equal(t, "beforeafter", string(got))
	assert.Zero(t, wire.Len())
}

// frameSizes parses the headers of a frame stream as it is written, keeping
// only the lengths, so a very large stream is checked without being stored.
type frameSizes struct {
	sizes   []uint32
	hdr     []byte
	payload uint32
}

func (f *frameSizes) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if f.payload > 0 {
			n := min(uint32(len(p)), f.payload)
			f.payload -= n
			p = p[n:]
			continue
		}
		need := 4 - len(f.hdr)
		n := min(need, len(p))
		f.hdr = append(f.hdr, p[:n]...)
		p = p[n:]
		if len(f.hdr) == 4 {
			f.payload = binary.BigEndian.Uint32(f.hdr)
			f.sizes = append(f.sizes, f.payload)
			f.hdr = f.hdr[:0]
		}
	}
	return total, nil
}

func TestFrameWriter_NeverEmitsAFrameTheReaderRefuses(t *testing.T) {
	// io.Copy hands the whole content of a bytes.Reader over in one Write. A
	// context above the frame limit must still reach the daemon as frames it
	// accepts, not as one frame it rejects after the upload.
	payload := make([]byte, maxFrameSize+12345)
	var sizes frameSizes
	fw := NewFrameWriter(&sizes)
	n, err := io.Copy(fw, bytes.NewReader(payload))
	require.NoError(t, err)
	require.Equal(t, int64(len(payload)), n)
	require.NoError(t, fw.Close())

	var sum uint64
	for i, size := range sizes.sizes {
		require.LessOrEqual(t, size, uint32(maxFrameSize), "frame %d is larger than the reader accepts", i)
		sum += uint64(size)
	}
	require.Equal(t, uint64(len(payload)), sum)
	require.Zero(t, sizes.sizes[len(sizes.sizes)-1], "the stream must end with the terminator")
	for _, size := range sizes.sizes[:len(sizes.sizes)-1] {
		require.NotZero(t, size, "a terminator in the middle of the stream cuts it short")
	}
}

func TestFrameReader_ErrorsKeepTheirCause(t *testing.T) {
	// The cause of a failed read must stay reachable: the daemon tells a
	// client that went away from one that sent a hostile length.
	causes := []error{syscall.ECONNRESET, syscall.ETIMEDOUT, io.ErrClosedPipe, errors.New("tls: bad record MAC")}
	for _, cause := range causes {
		_, err := io.ReadAll(NewFrameReader(&failingReader{data: []byte{0, 0}, err: cause}))
		require.ErrorIs(t, err, cause, "header")
		_, err = io.ReadAll(NewFrameReader(&failingReader{data: frame(8, []byte("abc")), err: cause}))
		require.ErrorIs(t, err, cause, "payload")
		require.NotErrorIs(t, err, ErrFrameTooLarge)
	}
}
