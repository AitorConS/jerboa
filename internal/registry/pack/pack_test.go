package pack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func blob(data []byte) Blob {
	sum := sha256.Sum256(data)
	return Blob{
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Size:   int64(len(data)),
		Open:   func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestWriterRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	w, err := NewWriter(zstd.SpeedDefault)
	require.NoError(t, err)
	inputs := map[string][]byte{}
	// Incompressible blobs force several packs; an empty file is a valid blob.
	for _, n := range []int{0, 10, 3 << 20, 3 << 20, 3 << 20, 100} {
		data := randomBytes(r, n)
		b := blob(data)
		inputs[b.Digest] = data
		require.NoError(t, w.Add(b))
	}
	packs := w.Close()
	require.Len(t, packs, 2)
	got := map[string][]byte{}
	for _, p := range packs {
		require.LessOrEqual(t, len(p.Data), TargetSize+(3<<20)+1024)
		body, err := EncodeUpload(p)
		require.NoError(t, err)
		back, err := DecodeUpload(body)
		require.NoError(t, err)
		require.Equal(t, p.Digest, back.Digest)
		for _, e := range back.Entries {
			data, err := ReadBlob(back.Data[e.Offset:e.Offset+e.Length], e)
			require.NoError(t, err)
			got[e.Digest] = data
		}
	}
	require.Len(t, got, len(inputs))
	for d, data := range inputs {
		require.True(t, bytes.Equal(data, got[d]), d)
	}
}

func onePack(t *testing.T, payloads ...[]byte) Pack {
	t.Helper()
	w, err := NewWriter(zstd.SpeedDefault)
	require.NoError(t, err)
	for _, p := range payloads {
		require.NoError(t, w.Add(blob(p)))
	}
	packs := w.Close()
	require.Len(t, packs, 1)
	return packs[0]
}

func reframe(t *testing.T, idx Index, data []byte) []byte {
	t.Helper()
	j, err := json.Marshal(idx)
	require.NoError(t, err)
	out := binary.BigEndian.AppendUint32(nil, uint32(len(j)))
	return append(append(out, j...), data...)
}

func TestDecodeUploadRejectsLies(t *testing.T) {
	p := onePack(t, []byte("first blob"), []byte("second blob"))
	idx := Index{Digest: p.Digest, Size: int64(len(p.Data)), Blobs: p.Entries}

	// A blob registered under someone else's digest.
	lie := idx
	lie.Blobs = append([]Entry(nil), p.Entries...)
	lie.Blobs[0].Digest = blob([]byte("victim content")).Digest
	_, err := DecodeUpload(reframe(t, lie, p.Data))
	require.ErrorContains(t, err, "does not match its digest")

	// Wrong uncompressed size.
	lie.Blobs = append([]Entry(nil), p.Entries...)
	lie.Blobs[1].Size++
	_, err = DecodeUpload(reframe(t, lie, p.Data))
	require.ErrorContains(t, err, "index says")

	// Frames must tile the pack.
	lie.Blobs = append([]Entry(nil), p.Entries...)
	lie.Blobs[1].Offset--
	_, err = DecodeUpload(reframe(t, lie, p.Data))
	require.ErrorContains(t, err, "does not follow")
	lie.Blobs = p.Entries[:1]
	_, err = DecodeUpload(reframe(t, lie, p.Data))
	require.ErrorContains(t, err, "after the last blob")

	// Pack bytes that are not what the digest names.
	tampered := append([]byte(nil), p.Data...)
	tampered[len(tampered)-1] ^= 1
	_, err = DecodeUpload(reframe(t, idx, tampered))
	require.ErrorContains(t, err, "digest does not match")

	_, err = DecodeUpload([]byte{0, 0, 0})
	require.Error(t, err)
	_, err = DecodeUpload([]byte{0xff, 0xff, 0xff, 0xff, '{'})
	require.ErrorContains(t, err, "index length")
}

// A small frame that expands enormously is cut off at the declared size.
func TestReadBlobBoundsDecompression(t *testing.T) {
	big := make([]byte, 64<<20)
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	frame := enc.EncodeAll(big, nil)
	require.Less(t, len(frame), 1<<20)
	e := blob([]byte("small")).Digest
	_, err = ReadBlob(frame, Entry{Digest: e, Size: 5, Length: int64(len(frame))})
	require.ErrorContains(t, err, "index says 5")
}

func TestAddRejectsChangedOrOversizedBlob(t *testing.T) {
	w, err := NewWriter(zstd.SpeedFastest)
	require.NoError(t, err)
	b := blob([]byte("declared"))
	b.Open = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("actual"))), nil }
	require.ErrorContains(t, w.Add(b), "changed while it was packed")

	r := rand.New(rand.NewPCG(3, 4))
	require.ErrorContains(t, w.Add(blob(randomBytes(r, MaxSize+1024))), "over the")
}

func TestPlanRangesMergesNearbyFrames(t *testing.T) {
	want := map[string]Location{
		"a": {Pack: "p1", Offset: 0, Length: 100},
		"b": {Pack: "p1", Offset: 150, Length: 100}, // 50-byte gap: merged
		"c": {Pack: "p1", Offset: 10_000, Length: 10},
		"d": {Pack: "p2", Offset: 0, Length: 5},
	}
	ranges := PlanRanges(want, map[string]int64{}, 64)
	require.Len(t, ranges, 3)
	require.Equal(t, Range{Pack: "p1", Offset: 0, Length: 250}, Range{Pack: ranges[0].Pack, Offset: ranges[0].Offset, Length: ranges[0].Length})
	require.Len(t, ranges[0].Entries, 2)
	require.Equal(t, int64(10_000), ranges[1].Offset)
	require.Equal(t, "p2", ranges[2].Pack)
}

// A frame that inflates to far more than it weighs is decoded without
// buffering the blob: memory stays near the decoder's window, not the size
// the index states.
func TestCopyBlobStreamsLargeBlobs(t *testing.T) {
	const size = 256 << 20
	h := sha256.New()
	var frame bytes.Buffer
	enc, err := zstd.NewWriter(&frame, zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	chunk := make([]byte, 1<<20)
	for i := 0; i < size/len(chunk); i++ {
		_, _ = h.Write(chunk)
		_, err := enc.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, enc.Close())
	e := Entry{Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Size: size, Length: int64(frame.Len())}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	require.NoError(t, CopyBlob(io.Discard, frame.Bytes(), e))
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<20))
}

// A single-segment frame states its content size as its window; one larger
// than maxWindow (but under the library's own 512 MiB default) is refused
// before anything is decoded.
func TestCopyBlobRefusesHugeSingleSegmentFrames(t *testing.T) {
	const size = 256 << 20
	frame := binary.LittleEndian.AppendUint32(nil, 0xFD2FB528)
	frame = append(frame, 0xE0) // single segment, 8-byte content size
	frame = binary.LittleEndian.AppendUint64(frame, size)
	// One last RLE block repeating a zero byte 128 KiB times.
	hdr := uint32(1) | 1<<1 | (128<<10)<<3
	frame = append(frame, byte(hdr), byte(hdr>>8), byte(hdr>>16), 0)
	e := Entry{Digest: "sha256:" + hex.EncodeToString(make([]byte, 32)), Size: size, Length: int64(len(frame))}
	err := CopyBlob(io.Discard, frame, e)
	require.ErrorIs(t, err, zstd.ErrDecoderSizeExceeded)
}

// Writer's frames decode under the same limits, also for blobs larger than
// the decoder's window.
func TestWriterFramesDecodeUnderTheLimits(t *testing.T) {
	data := bytes.Repeat([]byte("jerboa "), (maxWindow+(8<<20))/7)
	w, err := NewWriter(zstd.SpeedDefault)
	require.NoError(t, err)
	require.NoError(t, w.Add(blob(data)))
	packs := w.Close()
	require.Len(t, packs, 1)
	e := packs[0].Entries[0]
	got, err := ReadBlob(packs[0].Data[e.Offset:e.Offset+e.Length], e)
	require.NoError(t, err)
	require.Equal(t, data, got)
}
