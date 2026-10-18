package httpclient

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// PartialSuffix is appended to a destination path while its download is in
// progress. The validator (ETag or Last-Modified) that the partial bytes came
// from is kept next to it in PartialSuffix+ValidatorSuffix.
const (
	PartialSuffix   = ".partial"
	ValidatorSuffix = ".validator"
)

// maxResumeAttempts bounds how many times one Fetch reconnects after an
// interrupted transfer. A reconnect only happens when the previous attempt
// wrote at least one byte, so a dead origin fails fast.
const maxResumeAttempts = 3

// Resumable downloads a URL into a ".partial" file, continuing an earlier
// interrupted transfer with Range + If-Range instead of starting over. The
// partial file is only resumed when the server gave a strong validator for it;
// If-Range makes the server send the whole body again (200) if the resource
// changed since, so stale bytes are never combined with new ones.
type Resumable struct {
	// Client performs the requests. Nil means Streaming.
	Client *http.Client
	// StallTimeout aborts an attempt that receives no bytes for this long.
	// Zero means DefaultStallTimeout.
	StallTimeout time.Duration
	// MaxSize caps the total size of the download. Zero means unlimited. The
	// limit is MaxSize+1 bytes so an oversized body is reported by the caller's
	// size check instead of being silently truncated.
	MaxSize int64
}

// Fetch downloads url into partial and returns the total size and SHA-256 of
// the complete file. On error the partial file and its validator are kept so
// the next Fetch can resume; callers that reject the result (size or digest
// mismatch) must call DiscardPartial.
func (r Resumable) Fetch(ctx context.Context, url, partial string) (int64, []byte, error) {
	var lastErr error
	for range maxResumeAttempts {
		size, sum, progressed, err := r.attempt(ctx, url, partial)
		if err == nil {
			return size, sum, nil
		}
		lastErr = err
		if !progressed || ctx.Err() != nil {
			break
		}
	}
	return 0, nil, lastErr
}

// DiscardPartial removes a partial download and its validator.
func DiscardPartial(partial string) {
	_ = os.Remove(partial)
	_ = os.Remove(partial + ValidatorSuffix)
}

// CommitPartial moves a complete partial download to dest.
func CommitPartial(partial, dest string) error {
	if err := os.Rename(partial, dest); err != nil {
		return fmt.Errorf("install %s: %w", dest, err)
	}
	_ = os.Remove(partial + ValidatorSuffix)
	return nil
}

func (r Resumable) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return Streaming
}

// attempt runs one request. progressed reports whether any body bytes were
// written, which is what makes another attempt worthwhile.
func (r Resumable) attempt(ctx context.Context, url, partial string) (size int64, sum []byte, progressed bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	offset, validator := resumePoint(partial)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, false, fmt.Errorf("build request: %w", err)
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		req.Header.Set("If-Range", validator)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return 0, nil, false, fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		start, ok := contentRangeStart(resp.Header.Get("Content-Range"))
		if !ok || start != offset {
			// The server answered a different range than asked; the partial
			// file cannot be trusted to line up with it.
			DiscardPartial(partial)
			return 0, nil, true, fmt.Errorf("download %s: unexpected Content-Range %q", url, resp.Header.Get("Content-Range"))
		}
	case resp.StatusCode == http.StatusOK:
		offset = 0
		if err := writeValidator(partial, resp.Header); err != nil {
			return 0, nil, false, err
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		// The partial file is at least as long as the resource: it is either
		// complete from a run that died before committing, or junk. Start over
		// on the next attempt; the digest check decides either way.
		DiscardPartial(partial)
		return 0, nil, true, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	default:
		return 0, nil, false, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, h, err := openPartial(partial, offset)
	if err != nil {
		return 0, nil, false, err
	}
	var body io.Reader = resp.Body
	if r.MaxSize > 0 {
		body = io.LimitReader(body, max(r.MaxSize-offset+1, 0))
	}
	n, copyErr := CopyWithStall(io.MultiWriter(f, h), body, r.StallTimeout, cancel)
	closeErr := f.Close()
	if copyErr != nil {
		return 0, nil, n > 0, fmt.Errorf("download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		return 0, nil, n > 0, fmt.Errorf("close %s: %w", partial, closeErr)
	}
	return offset + n, h.Sum(nil), n > 0, nil
}

// resumePoint returns how many bytes of partial can be resumed and the
// validator they came from. A partial file without a validator is not resumed.
func resumePoint(partial string) (int64, string) {
	v, err := os.ReadFile(partial + ValidatorSuffix)
	if err != nil || len(v) == 0 {
		return 0, ""
	}
	info, err := os.Stat(partial)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, ""
	}
	return info.Size(), string(v)
}

// writeValidator records the strong validator of a fresh response, or removes
// a stale one when the response has none. Weak ETags are not allowed in If-Range.
func writeValidator(partial string, h http.Header) error {
	v := h.Get("ETag")
	if v == "" || strings.HasPrefix(v, "W/") {
		v = h.Get("Last-Modified")
	}
	path := partial + ValidatorSuffix
	if v == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// openPartial opens partial for appending at offset, with h already holding the
// digest of the bytes before offset. offset 0 truncates the file.
func openPartial(partial string, offset int64) (*os.File, hash.Hash, error) {
	h := sha256.New()
	if offset == 0 {
		f, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("create %s: %w", partial, err)
		}
		return f, h, nil
	}
	f, err := os.OpenFile(partial, os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", partial, err)
	}
	if _, err := io.CopyN(h, f, offset); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("hash %s: %w", partial, err)
	}
	// Drop anything past offset (a write that landed after the size we read).
	if err := f.Truncate(offset); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("truncate %s: %w", partial, err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("seek %s: %w", partial, err)
	}
	return f, h, nil
}

// contentRangeStart parses the first byte position of "bytes start-end/total".
func contentRangeStart(v string) (int64, bool) {
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, false
	}
	start, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(start, 10, 64)
	return n, err == nil
}
