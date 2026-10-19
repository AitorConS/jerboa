// Package pack groups registry blobs into packs: concatenations of
// independent zstd frames, one per blob, so a client fetches any blob with a
// single HTTP range request and a pack is one object in storage.
//
// A pack carries no header. Its index (blob digest, uncompressed size, offset
// and length of the frame) travels beside it: in the upload body, and in the
// per-recipe locations document a registry publishes. Every blob is
// identified by the SHA-256 of its uncompressed bytes and verified after
// decompression, so neither the index nor the storage needs to be trusted.
package pack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// MediaType identifies an upload body: a framed index followed by the pack.
const MediaType = "application/vnd.jerboa.pack.v1"

const (
	// TargetSize is the compressed size a writer fills a pack to.
	TargetSize = 8 << 20
	// MaxSize bounds a pack, and so the largest compressed blob, in v1 of
	// the protocol: a registry verifies a whole pack in memory.
	MaxSize = 64 << 20
	// maxIndexBytes bounds the framed index of one upload.
	maxIndexBytes = 16 << 20
)

// Entry locates one blob inside a pack.
type Entry struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

// Pack is one finished pack and its index.
type Pack struct {
	Digest  string
	Data    []byte
	Entries []Entry
}

// Blob is an input to a Writer.
type Blob struct {
	Digest string
	Size   int64
	Open   func() (io.ReadCloser, error)
}

// Writer compresses blobs into packs of about TargetSize.
type Writer struct {
	enc  *zstd.Encoder
	cur  bytes.Buffer
	idx  []Entry
	done []Pack
}

// NewWriter returns a writer compressing at the given zstd level.
func NewWriter(level zstd.EncoderLevel) (*Writer, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("pack: encoder: %w", err)
	}
	return &Writer{enc: enc}, nil
}

// Add compresses a blob into the current pack, starting a new pack when the
// current one would grow past TargetSize. The blob's bytes are hashed while
// they are read and must match b.Digest and b.Size.
func (w *Writer) Add(b Blob) error {
	rc, err := b.Open()
	if err != nil {
		return fmt.Errorf("pack: open %s: %w", b.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	var frame bytes.Buffer
	w.enc.Reset(&frame)
	n, err := io.Copy(w.enc, io.TeeReader(rc, h))
	if err != nil {
		return fmt.Errorf("pack: compress %s: %w", b.Digest, err)
	}
	if err := w.enc.Close(); err != nil {
		return fmt.Errorf("pack: compress %s: %w", b.Digest, err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != b.Digest || n != b.Size {
		return fmt.Errorf("pack: blob %s changed while it was packed (got %s, %d bytes)", b.Digest, got, n)
	}
	if frame.Len() > MaxSize {
		return fmt.Errorf("pack: blob %s compresses to %d bytes, over the %d-byte limit", b.Digest, frame.Len(), MaxSize)
	}
	if w.cur.Len() > 0 && w.cur.Len()+frame.Len() > TargetSize {
		w.flush()
	}
	w.idx = append(w.idx, Entry{Digest: b.Digest, Size: n, Offset: int64(w.cur.Len()), Length: int64(frame.Len())})
	w.cur.Write(frame.Bytes())
	return nil
}

// Close finishes the last pack and returns every pack not yet taken.
func (w *Writer) Close() []Pack {
	w.flush()
	return w.Take()
}

// Take returns the packs finished so far and forgets them, so a caller can
// upload packs as they fill instead of holding a whole push in memory.
func (w *Writer) Take() []Pack {
	done := w.done
	w.done = nil
	return done
}

func (w *Writer) flush() {
	if w.cur.Len() == 0 {
		return
	}
	data := append([]byte(nil), w.cur.Bytes()...)
	sum := sha256.Sum256(data)
	w.done = append(w.done, Pack{Digest: "sha256:" + hex.EncodeToString(sum[:]), Data: data, Entries: w.idx})
	w.cur.Reset()
	w.idx = nil
}

// Index is the upload-side description of a pack.
type Index struct {
	Digest string  `json:"digest"`
	Size   int64   `json:"size"`
	Blobs  []Entry `json:"blobs"`
}

// EncodeUpload frames a pack for upload: a 4-byte big-endian index length,
// the JSON index, then the pack bytes.
func EncodeUpload(p Pack) ([]byte, error) {
	idx, err := json.Marshal(Index{Digest: p.Digest, Size: int64(len(p.Data)), Blobs: p.Entries})
	if err != nil {
		return nil, fmt.Errorf("pack: encode index: %w", err)
	}
	out := make([]byte, 4, 4+len(idx)+len(p.Data))
	binary.BigEndian.PutUint32(out, uint32(len(idx)))
	out = append(out, idx...)
	return append(out, p.Data...), nil
}

// DecodeUpload parses an upload body and verifies the pack completely: its
// digest and size, an index that tiles the pack exactly with in-order,
// non-overlapping frames, and every blob's uncompressed size and digest.
func DecodeUpload(body []byte) (Pack, error) {
	if len(body) < 4 {
		return Pack{}, errors.New("pack: upload too short")
	}
	n := binary.BigEndian.Uint32(body)
	if n > maxIndexBytes || int(n) > len(body)-4 {
		return Pack{}, errors.New("pack: invalid index length")
	}
	var idx Index
	dec := json.NewDecoder(bytes.NewReader(body[4 : 4+n]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&idx); err != nil {
		return Pack{}, fmt.Errorf("pack: index: %w", err)
	}
	p := Pack{Digest: idx.Digest, Data: body[4+n:], Entries: idx.Blobs}
	if err := Verify(p); err != nil {
		return Pack{}, err
	}
	return p, nil
}

// Verify checks a pack against its own index.
func Verify(p Pack) error {
	if len(p.Data) == 0 || len(p.Data) > MaxSize {
		return fmt.Errorf("pack: size %d outside 1..%d", len(p.Data), MaxSize)
	}
	sum := sha256.Sum256(p.Data)
	if "sha256:"+hex.EncodeToString(sum[:]) != p.Digest {
		return errors.New("pack: digest does not match its bytes")
	}
	var next int64
	seen := map[string]bool{}
	for _, e := range p.Entries {
		if e.Offset != next || e.Length <= 0 || e.Offset+e.Length > int64(len(p.Data)) {
			return fmt.Errorf("pack: blob %s frame does not follow the previous one", e.Digest)
		}
		if seen[e.Digest] {
			return fmt.Errorf("pack: blob %s listed twice", e.Digest)
		}
		seen[e.Digest] = true
		if _, err := ReadBlob(p.Data[e.Offset:e.Offset+e.Length], e); err != nil {
			return err
		}
		next = e.Offset + e.Length
	}
	if next != int64(len(p.Data)) {
		return errors.New("pack: bytes after the last blob")
	}
	return nil
}

// ReadBlob decompresses one blob's frame and verifies its size and digest.
// It never produces more than e.Size bytes, whatever the frame claims.
func ReadBlob(frame []byte, e Entry) ([]byte, error) {
	if e.Size < 0 || e.Size > 1<<31 {
		return nil, fmt.Errorf("pack: blob %s size %d out of range", e.Digest, e.Size)
	}
	r, err := zstd.NewReader(bytes.NewReader(frame), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("pack: blob %s: %w", e.Digest, err)
	}
	defer r.Close()
	out := make([]byte, 0, e.Size)
	buf := bytes.NewBuffer(out)
	n, err := io.Copy(buf, io.LimitReader(r, e.Size+1))
	if err != nil {
		return nil, fmt.Errorf("pack: blob %s: %w", e.Digest, err)
	}
	if n != e.Size {
		return nil, fmt.Errorf("pack: blob %s is %d bytes, index says %d", e.Digest, n, e.Size)
	}
	sum := sha256.Sum256(buf.Bytes())
	if "sha256:"+hex.EncodeToString(sum[:]) != e.Digest {
		return nil, fmt.Errorf("pack: blob %s does not match its digest", e.Digest)
	}
	return buf.Bytes(), nil
}

// Range is one HTTP range request covering consecutive frames of a pack.
type Range struct {
	Pack    string
	Offset  int64
	Length  int64
	Entries []Entry
}

// Location is where a registry keeps a blob.
type Location struct {
	Pack   string `json:"pack"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

// PlanRanges groups the frames to fetch into as few range requests as
// possible, merging frames of a pack that are at most maxGap bytes apart:
// fetching a small unneeded gap is cheaper than another request.
func PlanRanges(want map[string]Location, sizes map[string]int64, maxGap int64) []Range {
	byPack := map[string][]Entry{}
	for d, l := range want {
		byPack[l.Pack] = append(byPack[l.Pack], Entry{Digest: d, Size: sizes[d], Offset: l.Offset, Length: l.Length})
	}
	packs := make([]string, 0, len(byPack))
	for p := range byPack {
		packs = append(packs, p)
	}
	sort.Strings(packs)
	var out []Range
	for _, p := range packs {
		es := byPack[p]
		sort.Slice(es, func(i, j int) bool { return es[i].Offset < es[j].Offset })
		cur := Range{Pack: p, Offset: es[0].Offset, Length: es[0].Length, Entries: es[:1:1]}
		for _, e := range es[1:] {
			end := cur.Offset + cur.Length
			if e.Offset-end <= maxGap {
				if e.Offset+e.Length > end {
					cur.Length = e.Offset + e.Length - cur.Offset
				}
				cur.Entries = append(cur.Entries, e)
				continue
			}
			out = append(out, cur)
			cur = Range{Pack: p, Offset: e.Offset, Length: e.Length, Entries: []Entry{e}}
		}
		out = append(out, cur)
	}
	return out
}
