package pack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pack travels over the network and sits in storage neither side trusts.
// These tests damage packs, their indexes and the blobs they are made from in
// the ways transfers and disks do, and hold the package to one rule: bytes
// that are not the blob its digest names never come out as that blob.

// Every prefix of an upload, as a connection cut at each point would leave
// it, is refused: none of them is a smaller valid upload.
func TestFaults_TruncatedUploadIsNeverValid(t *testing.T) {
	p := onePack(t, []byte("first blob"), bytes.Repeat([]byte("second blob "), 50), []byte("third"))
	body, err := EncodeUpload(p)
	require.NoError(t, err)
	_, err = DecodeUpload(body)
	require.NoError(t, err)
	for n := range len(body) {
		_, err := DecodeUpload(body[:n])
		require.Error(t, err, "the first %d of %d bytes were accepted as an upload", n, len(body))
	}
	_, err = DecodeUpload(append(bytes.Clone(body), 0))
	require.Error(t, err, "a byte after the pack was accepted")
}

// Any single flipped bit of an upload is noticed, wherever it lands: in the
// length, the index, or a frame.
func TestFaults_BitFlipAnywhereInAnUploadIsNoticed(t *testing.T) {
	p := onePack(t, []byte("first blob"), bytes.Repeat([]byte("second blob "), 20))
	body, err := EncodeUpload(p)
	require.NoError(t, err)
	for i := range body {
		for _, bit := range []byte{0x01, 0x80} {
			damaged := bytes.Clone(body)
			damaged[i] ^= bit
			got, err := DecodeUpload(damaged)
			if err == nil {
				// The only flips that may pass are ones that leave every
				// blob and the pack's own digest as they were.
				require.Equal(t, p.Digest, got.Digest, "byte %d", i)
				require.Equal(t, p.Data, got.Data, "byte %d", i)
				require.Equal(t, p.Entries, got.Entries, "byte %d", i)
			}
		}
	}
}

// A frame is read from a range of a pack. Whatever is wrong with the bytes
// that came back, the blob is not produced.
func TestFaults_DamagedFrameNeverYieldsABlob(t *testing.T) {
	content := bytes.Repeat([]byte("the bytes of a library "), 300)
	other := bytes.Repeat([]byte("the bytes of another one "), 300)
	p := onePack(t, content, other)
	e := p.Entries[0]
	frame := p.Data[e.Offset : e.Offset+e.Length]
	otherFrame := p.Data[p.Entries[1].Offset : p.Entries[1].Offset+p.Entries[1].Length]

	damages := map[string][]byte{
		"empty":               nil,
		"truncated":           frame[:len(frame)/2],
		"one byte short":      frame[:len(frame)-1],
		"zeroed":              make([]byte, len(frame)),
		"another blob":        otherFrame,
		"two frames":          append(bytes.Clone(frame), frame...),
		"followed by garbage": append(bytes.Clone(frame), "garbage"...),
		"not zstd":            content[:len(frame)],
		"only the magic":      {0x28, 0xb5, 0x2f, 0xfd},
	}
	for i := range frame {
		flipped := bytes.Clone(frame)
		flipped[i] ^= 0x10
		damages["bit flip at "+strings.Repeat("0", 4-len(itoa(i)))+itoa(i)] = flipped
	}
	for name, data := range damages {
		var out bytes.Buffer
		err := CopyBlob(&out, data, e)
		if err == nil {
			// A flip in a part of the frame that carries no content may
			// decode: then it must have decoded to the very same bytes.
			require.Equal(t, content, out.Bytes(), name)
			continue
		}
		got, rerr := ReadBlob(data, e)
		require.Error(t, rerr, name)
		assert.Nil(t, got, "%s: ReadBlob returned bytes beside its error", name)
	}
}

func itoa(n int) string {
	var b []byte
	for ; n > 0 || len(b) == 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// An index that lies about a blob's size or digest. The frame is intact, so
// only the checks against the index can catch it.
func TestFaults_IndexThatLiesAboutABlob(t *testing.T) {
	content := []byte("a blob whose index entry is wrong")
	p := onePack(t, content)
	frame := p.Data
	lies := map[string]func(e *Entry){
		"one byte less":      func(e *Entry) { e.Size-- },
		"one byte more":      func(e *Entry) { e.Size++ },
		"zero size":          func(e *Entry) { e.Size = 0 },
		"negative size":      func(e *Entry) { e.Size = -1 },
		"absurd size":        func(e *Entry) { e.Size = 1 << 40 },
		"another digest":     func(e *Entry) { e.Digest = "sha256:" + strings.Repeat("0", 64) },
		"empty digest":       func(e *Entry) { e.Digest = "" },
		"digest upper-cased": func(e *Entry) { e.Digest = strings.ToUpper(e.Digest) },
	}
	for name, lie := range lies {
		t.Run(name, func(t *testing.T) {
			e := p.Entries[0]
			lie(&e)
			var out bytes.Buffer
			require.Error(t, CopyBlob(&out, frame, e))
			assert.LessOrEqual(t, int64(out.Len()), max(e.Size, 0)+1, "more was written than the index allows")
		})
	}
}

// failingWriter is a disk that fills up after some bytes.
type failingWriter struct {
	left int
	err  error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		n := f.left
		f.left = 0
		return n, f.err
	}
	f.left -= len(p)
	return len(p), nil
}

// The destination fails while a blob is decompressed into it: the error is
// the destination's, not a digest mismatch that would blame the registry.
func TestFaults_CopyBlobReportsAFailingDestination(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 5000)
	p := onePack(t, content)
	for _, after := range []int{0, 1, len(content) / 2, len(content) - 1} {
		err := CopyBlob(&failingWriter{left: after, err: syscall.ENOSPC}, p.Data, p.Entries[0])
		require.ErrorIs(t, err, syscall.ENOSPC, "after %d bytes", after)
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func (f *failingReader) Close() error { return nil }

// A blob that cannot be opened, or whose read fails half-way (a disk error
// under the local store), is not packed, and what was packed before it is
// still a valid pack: the failed blob leaves no trace in it.
func TestFaults_WriterWithABlobThatCannotBeRead(t *testing.T) {
	w, err := NewWriter(zstd.SpeedDefault)
	require.NoError(t, err)
	good := []byte("a blob that reads fine")
	require.NoError(t, w.Add(blob(good)))

	bad := blob(bytes.Repeat([]byte("unreadable "), 1000))
	boom := errors.New("input/output error")
	unopenable := bad
	unopenable.Open = func() (io.ReadCloser, error) { return nil, boom }
	require.ErrorIs(t, w.Add(unopenable), boom)

	halfRead := bad
	halfRead.Open = func() (io.ReadCloser, error) {
		return &failingReader{data: bytes.Repeat([]byte("unreadable "), 500), err: boom}, nil
	}
	require.ErrorIs(t, w.Add(halfRead), boom)

	shortRead := bad
	shortRead.Open = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("unreadable "), 500))), nil
	}
	require.ErrorContains(t, w.Add(shortRead), "changed while it was packed")

	after := []byte("a blob added after the failures")
	require.NoError(t, w.Add(blob(after)))
	packs := w.Close()
	require.Len(t, packs, 1)
	require.NoError(t, Verify(packs[0]), "a failed blob left the pack invalid")
	require.Len(t, packs[0].Entries, 2)
	for i, want := range [][]byte{good, after} {
		e := packs[0].Entries[i]
		got, err := ReadBlob(packs[0].Data[e.Offset:e.Offset+e.Length], e)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	assert.Empty(t, w.Close(), "closing twice yields the same pack twice")
}

// Locations come from the registry. Planning ranges from hostile ones must
// not produce a range that Validate would refuse piece by piece.
func TestFaults_LocationsAtTheEdges(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	ok := []Location{{d, 0, 1}, {d, 0, MaxSize}, {d, MaxSize - 1, 1}, {d, 1, MaxSize - 1}}
	for _, l := range ok {
		require.NoError(t, l.Validate(), l)
	}
	bad := []Location{
		{d, 0, 0}, {d, -1, 1}, {d, 0, -1}, {d, 0, MaxSize + 1}, {d, MaxSize, 1}, {d, 1, MaxSize},
		{d, 1 << 62, 1 << 62}, {d, -1 << 63, 1}, {d, 1, -1 << 63}, {d, 1<<63 - 1, 1<<63 - 1},
		{"", 0, 1}, {"sha256:", 0, 1}, {strings.Repeat("a", 64), 0, 1}, {"sha256:" + strings.Repeat("A", 64), 0, 1},
		{d + "/../x", 0, 1}, {"sha256:" + strings.Repeat("a", 63) + "\n", 0, 1},
	}
	for _, l := range bad {
		require.Error(t, l.Validate(), l)
	}
}

func FuzzDecodeUpload(f *testing.F) {
	w, err := NewWriter(zstd.SpeedDefault)
	require.NoError(f, err)
	require.NoError(f, w.Add(blob([]byte("first blob"))))
	require.NoError(f, w.Add(blob(bytes.Repeat([]byte("second "), 40))))
	p := w.Close()[0]
	body, err := EncodeUpload(p)
	require.NoError(f, err)
	f.Add(body)
	f.Add(body[:len(body)/2])
	f.Add(body[:4])
	f.Add([]byte{})
	f.Add(binary.BigEndian.AppendUint32(nil, 0xffffffff))
	f.Add(append(binary.BigEndian.AppendUint32(nil, 2), "{}"...))
	f.Add(append(binary.BigEndian.AppendUint32(nil, 4), "null"...))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := DecodeUpload(data)
		if err != nil {
			return
		}
		// What decodes is a pack that verifies on its own and encodes back
		// to an upload that decodes to the same pack.
		require.NoError(t, Verify(got))
		require.LessOrEqual(t, len(got.Data), MaxSize)
		again, err := EncodeUpload(got)
		require.NoError(t, err)
		back, err := DecodeUpload(again)
		require.NoError(t, err)
		require.Equal(t, got.Digest, back.Digest)
		require.Equal(t, got.Entries, back.Entries)
	})
}

func FuzzCopyBlob(f *testing.F) {
	content := bytes.Repeat([]byte("fuzzed frame content "), 30)
	w, err := NewWriter(zstd.SpeedDefault)
	require.NoError(f, err)
	require.NoError(f, w.Add(blob(content)))
	p := w.Close()[0]
	e := p.Entries[0]
	f.Add(p.Data, e.Size)
	f.Add(p.Data[:len(p.Data)/2], e.Size)
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 0x00, 0x01, 0x00, 0x00}, int64(0))
	f.Add([]byte{}, int64(-1))
	f.Add(p.Data, int64(1)<<40)
	f.Fuzz(func(t *testing.T, frame []byte, size int64) {
		var out bytes.Buffer
		entry := Entry{Digest: e.Digest, Size: size, Length: int64(len(frame))}
		err := CopyBlob(&out, frame, entry)
		// Never more than the index allows, whatever the frame holds, and
		// success only for the blob the digest names.
		if size >= 0 {
			require.LessOrEqual(t, int64(out.Len()), size+1)
		} else {
			require.Zero(t, out.Len())
		}
		if err == nil {
			require.Equal(t, content, out.Bytes())
		}
	})
}

func FuzzPlanRanges(f *testing.F) {
	f.Add(int64(0), int64(10), int64(10), int64(10), int64(100), int64(5), int64(64<<10))
	f.Add(int64(0), int64(10), int64(5), int64(10), int64(0), int64(10), int64(0))
	f.Add(int64(100), int64(1), int64(0), int64(1), int64(50), int64(1), int64(-1))
	f.Fuzz(func(t *testing.T, o1, l1, o2, l2, o3, l3, gap int64) {
		d := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
		packA, packB := d("a"), d("b")
		want := map[string]Location{d("1"): {packA, o1, l1}, d("2"): {packA, o2, l2}, d("3"): {packB, o3, l3}}
		for _, l := range want {
			if l.Validate() != nil {
				return // the caller validates every location first
			}
		}
		ranges := PlanRanges(want, map[string]int64{}, gap)
		seen := map[string]bool{}
		for _, r := range ranges {
			require.Positive(t, r.Length)
			require.NoError(t, Location{Pack: r.Pack, Offset: r.Offset, Length: min(r.Length, MaxSize)}.Validate())
			for _, e := range r.Entries {
				require.False(t, seen[e.Digest], "a blob is fetched twice")
				seen[e.Digest] = true
				// Every frame lies wholly inside the range that fetches it.
				require.GreaterOrEqual(t, e.Offset, r.Offset)
				require.LessOrEqual(t, e.Offset+e.Length, r.Offset+r.Length)
				require.Equal(t, want[e.Digest].Pack, r.Pack)
			}
		}
		require.Len(t, seen, len(want), "a blob was left out of the plan")
	})
}
