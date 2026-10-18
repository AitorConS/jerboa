package httpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rangeServer serves body with an ETag and honors Range + If-Range. When
// cutAfter > 0 the first full or ranged response is cut off after that many
// bytes, simulating a dropped connection.
type rangeServer struct {
	body      []byte
	etag      string
	cutAfter  atomic.Int64
	ranges    atomic.Int32
	fullGets  atomic.Int32
	lastRange atomic.Value
}

func (s *rangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.etag != "" {
		w.Header().Set("ETag", s.etag)
	}
	start := int64(0)
	if rg := r.Header.Get("Range"); rg != "" && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == s.etag) {
		s.lastRange.Store(rg)
		n, err := strconv.ParseInt(rg[len("bytes="):len(rg)-1], 10, 64)
		if err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if n >= int64(len(s.body)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = n
		s.ranges.Add(1)
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+
			strconv.Itoa(len(s.body)-1)+"/"+strconv.Itoa(len(s.body)))
		w.Header().Set("Content-Length", strconv.FormatInt(int64(len(s.body))-start, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		s.fullGets.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
		w.WriteHeader(http.StatusOK)
	}
	out := s.body[start:]
	if cut := s.cutAfter.Swap(0); cut > 0 && cut < int64(len(out)) {
		_, _ = w.Write(out[:cut])
		// Abort the connection mid-body.
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(out)
}

func testBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func TestResumableFetch_Fresh(t *testing.T) {
	srv := &rangeServer{body: testBody(1 << 16), etag: `"v1"`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	size, got, err := Resumable{}.Fetch(context.Background(), ts.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(srv.body)), size)
	require.Equal(t, sum(srv.body), got)
	require.Equal(t, int32(1), srv.fullGets.Load())
	require.Equal(t, int32(0), srv.ranges.Load())

	dest := filepath.Join(filepath.Dir(partial), "a.bin")
	require.NoError(t, CommitPartial(partial, dest))
	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, srv.body, data)
	require.NoFileExists(t, partial+ValidatorSuffix)
}

// A dropped connection is resumed within the same Fetch with a Range request,
// and the digest covers the bytes from both attempts.
func TestResumableFetch_ResumesAfterDrop(t *testing.T) {
	srv := &rangeServer{body: testBody(1 << 16), etag: `"v1"`}
	srv.cutAfter.Store(10000)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	size, got, err := Resumable{}.Fetch(context.Background(), ts.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(srv.body)), size)
	require.Equal(t, sum(srv.body), got)
	require.Equal(t, int32(1), srv.fullGets.Load())
	require.Equal(t, int32(1), srv.ranges.Load())
	require.Equal(t, "bytes=10000-", srv.lastRange.Load())
}

// A partial file left by an earlier process is resumed rather than refetched.
func TestResumableFetch_ResumesLeftoverPartial(t *testing.T) {
	srv := &rangeServer{body: testBody(1 << 16), etag: `"v1"`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	require.NoError(t, os.WriteFile(partial, srv.body[:4096], 0o644))
	require.NoError(t, os.WriteFile(partial+ValidatorSuffix, []byte(`"v1"`), 0o644))

	_, got, err := Resumable{}.Fetch(context.Background(), ts.URL, partial)
	require.NoError(t, err)
	require.Equal(t, sum(srv.body), got)
	require.Equal(t, int32(0), srv.fullGets.Load())
	require.Equal(t, int32(1), srv.ranges.Load())
}

// If the resource changed, If-Range makes the server send the full new body
// and the stale prefix is discarded.
func TestResumableFetch_ChangedResourceRestarts(t *testing.T) {
	srv := &rangeServer{body: testBody(1 << 16), etag: `"v2"`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	require.NoError(t, os.WriteFile(partial, bytes.Repeat([]byte{0xff}, 4096), 0o644))
	require.NoError(t, os.WriteFile(partial+ValidatorSuffix, []byte(`"v1"`), 0o644))

	size, got, err := Resumable{}.Fetch(context.Background(), ts.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(srv.body)), size)
	require.Equal(t, sum(srv.body), got)
	require.Equal(t, int32(1), srv.fullGets.Load())
	v, err := os.ReadFile(partial + ValidatorSuffix)
	require.NoError(t, err)
	require.Equal(t, `"v2"`, string(v))
}

// Without a validator the partial bytes cannot be matched to a version of the
// resource, so the download starts over.
func TestResumableFetch_NoValidatorDoesNotResume(t *testing.T) {
	srv := &rangeServer{body: testBody(1 << 16)}
	srv.cutAfter.Store(10000)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	_, got, err := Resumable{}.Fetch(context.Background(), ts.URL, partial)
	require.NoError(t, err)
	require.Equal(t, sum(srv.body), got)
	require.Equal(t, int32(2), srv.fullGets.Load())
	require.Equal(t, int32(0), srv.ranges.Load())
}

// A leftover partial as long as the resource (a run that died before
// committing) gets 416 and is downloaded again from scratch.
func TestResumableFetch_RangeNotSatisfiableRestarts(t *testing.T) {
	srv := &rangeServer{body: testBody(4096), etag: `"v1"`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	require.NoError(t, os.WriteFile(partial, srv.body, 0o644))
	require.NoError(t, os.WriteFile(partial+ValidatorSuffix, []byte(`"v1"`), 0o644))

	size, got, err := Resumable{}.Fetch(context.Background(), ts.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(srv.body)), size)
	require.Equal(t, sum(srv.body), got)
	require.Equal(t, int32(1), srv.fullGets.Load())
}

// A failure with no bytes received is not retried, and the partial is kept for
// the next call.
func TestResumableFetch_KeepsPartialOnFailure(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	require.NoError(t, os.WriteFile(partial, []byte("abc"), 0o644))
	require.NoError(t, os.WriteFile(partial+ValidatorSuffix, []byte(`"v1"`), 0o644))

	_, _, err := Resumable{StallTimeout: time.Second}.Fetch(context.Background(), ts.URL, partial)
	require.ErrorContains(t, err, "HTTP 503")
	require.Equal(t, int32(1), calls.Load())
	require.FileExists(t, partial)

	DiscardPartial(partial)
	require.NoFileExists(t, partial)
	require.NoFileExists(t, partial+ValidatorSuffix)
}

// MaxSize stops an oversized body one byte past the limit so the caller's size
// check rejects it.
func TestResumableFetch_MaxSize(t *testing.T) {
	srv := &rangeServer{body: testBody(1 << 16), etag: `"v1"`}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	partial := filepath.Join(t.TempDir(), "a.bin.partial")
	size, _, err := Resumable{MaxSize: 100}.Fetch(context.Background(), ts.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(101), size)
}
