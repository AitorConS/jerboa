package httpclient

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// What must hold when a download goes wrong: the connection drops, the server
// stalls, lies about what it sends or changes the resource half-way. Whatever
// happens, Fetch never reports success for bytes it did not write, the size
// and digest it returns describe the file on disk, and bytes of two versions
// of a resource are never joined behind a validator that vouches for them.

// request is what one request of a download asked for.
type request struct{ Range, IfRange string }

// origin is a scripted download server. Each request is recorded and handed
// to handle together with its index (0 is the first).
type origin struct {
	*httptest.Server
	mu       sync.Mutex
	requests []request
}

func newOrigin(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) *origin {
	t.Helper()
	o := &origin{}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		n := len(o.requests)
		o.requests = append(o.requests, request{Range: r.Header.Get("Range"), IfRange: r.Header.Get("If-Range")})
		o.mu.Unlock()
		handle(n, w, r)
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *origin) seen() []request {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]request(nil), o.requests...)
}

// serveHonest answers as a correct server does: the whole body, or the tail a
// Range asks for when If-Range names the current validator. With cut >= 0 the
// connection is dropped after that many body bytes of this response.
func serveHonest(w http.ResponseWriter, r *http.Request, body []byte, validator string, cut int) {
	if strings.HasPrefix(validator, `"`) || strings.HasPrefix(validator, "W/") {
		w.Header().Set("ETag", validator)
	} else if validator != "" {
		w.Header().Set("Last-Modified", validator)
	}
	out := body
	if rg := r.Header.Get("Range"); rg != "" && r.Header.Get("If-Range") == validator && validator != "" {
		start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
		if err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start >= len(body) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		out = body[start:]
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(body)-1)+"/"+strconv.Itoa(len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(http.StatusOK)
	}
	if cut >= 0 && cut < len(out) {
		_, _ = w.Write(out[:cut])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(out)
}

// raw takes over the connection and writes a hand-made response, for answers
// a well-behaved server cannot produce.
func raw(t *testing.T, w http.ResponseWriter, response []byte) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_, _ = conn.Write(response)
	_ = conn.Close()
}

func partialPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "artifact.bin"+PartialSuffix)
}

func leftover(t *testing.T, partial string, data []byte, validator string) {
	t.Helper()
	require.NoError(t, os.WriteFile(partial, data, 0o644))
	require.NoError(t, os.WriteFile(partial+ValidatorSuffix, []byte(validator), 0o644))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// fetchUntilDone calls Fetch as a caller that retries does, and fails the test
// if the download does not complete within a few calls.
func fetchUntilDone(t *testing.T, r Resumable, url, partial string) (size int64, digest []byte, calls int) {
	t.Helper()
	var err error
	for calls = 1; calls <= 20; calls++ {
		size, digest, err = r.Fetch(context.Background(), url, partial)
		if err == nil {
			return size, digest, calls
		}
	}
	require.NoError(t, err, "the download never completed")
	return 0, nil, calls
}

// The connection drops after every possible number of body bytes. The
// download completes on a later attempt, byte for byte, and bytes already on
// disk are not asked for again.
func TestFaults_ConnectionDroppedAtEveryOffset(t *testing.T) {
	body := testBody(301)
	for cut := 0; cut <= len(body); cut++ {
		o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 0 {
				serveHonest(w, r, body, `"v1"`, cut)
				return
			}
			serveHonest(w, r, body, `"v1"`, -1)
		})
		partial := partialPath(t)
		size, digest, _ := fetchUntilDone(t, Resumable{}, o.URL, partial)
		require.Equal(t, int64(len(body)), size, "cut at %d", cut)
		require.Equal(t, sum(body), digest, "cut at %d", cut)
		require.Equal(t, body, readFile(t, partial), "cut at %d", cut)

		seen := o.seen()
		if cut > 0 && cut < len(body) {
			require.Len(t, seen, 2, "cut at %d", cut)
			require.Equal(t, request{Range: "bytes=" + strconv.Itoa(cut) + "-", IfRange: `"v1"`}, seen[1], "cut at %d", cut)
		}
		o.Close()
	}
}

// A connection that drops on every single response: one Fetch gives up after
// a bounded number of reconnects, but each call adds to the partial file, so a
// caller that retries finishes the download without ever starting over.
func TestFaults_EveryResponseIsCutShort(t *testing.T) {
	body := testBody(2000)
	o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		serveHonest(w, r, body, `"v1"`, 150)
	})
	partial := partialPath(t)

	_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.Error(t, err)
	require.Len(t, o.seen(), maxResumeAttempts, "one Fetch reconnects a bounded number of times")
	require.Equal(t, body[:150*maxResumeAttempts], readFile(t, partial), "what arrived is kept for the next call")

	size, digest, _ := fetchUntilDone(t, Resumable{}, o.URL, partial)
	require.Equal(t, int64(len(body)), size)
	require.Equal(t, sum(body), digest)
	require.Equal(t, body, readFile(t, partial))
	for i, req := range o.seen() {
		if i > 0 {
			require.Equal(t, "bytes="+strconv.Itoa(150*i)+"-", req.Range, "request %d must continue where the last one stopped", i)
		}
	}
}

// The server announces more bytes than it sends and closes. A short body must
// be an error, never a complete download.
func TestFaults_ContentLengthLongerThanBody(t *testing.T) {
	body := testBody(4000)
	lie := func(t *testing.T, w http.ResponseWriter, etag string) {
		head := "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
		if etag != "" {
			head += "ETag: " + etag + "\r\n"
		}
		raw(t, w, append([]byte(head+"\r\n"), body[:1000]...))
	}

	t.Run("then honest", func(t *testing.T) {
		o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 0 {
				lie(t, w, `"v1"`)
				return
			}
			serveHonest(w, r, body, `"v1"`, -1)
		})
		partial := partialPath(t)
		size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
		require.NoError(t, err)
		require.Equal(t, int64(len(body)), size)
		require.Equal(t, sum(body), digest)
		require.Equal(t, "bytes=1000-", o.seen()[1].Range)
	})

	t.Run("always", func(t *testing.T) {
		o := newOrigin(t, func(_ int, w http.ResponseWriter, _ *http.Request) { lie(t, w, "") })
		partial := partialPath(t)
		_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.Len(t, o.seen(), maxResumeAttempts)
		require.Equal(t, body[:1000], readFile(t, partial))
	})
}

// A body with no announced length that the server cuts short cannot be told
// from a complete one. Fetch must then report exactly what it wrote, so the
// caller's size and digest check rejects it.
func TestFaults_UnannouncedBodyCutShortIsReportedAsIs(t *testing.T) {
	body := testBody(4000)
	o := newOrigin(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		raw(t, w, append([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"), body[:1234]...))
	})
	partial := partialPath(t)
	size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(1234), size)
	require.Equal(t, sum(body[:1234]), digest)
	require.Equal(t, body[:1234], readFile(t, partial))
}

// The server answers a resume with a range other than the one asked for (or
// with none it names). Appending it would corrupt the file: the partial is
// thrown away and the download starts over.
func TestFaults_ResumeAnsweredWithWrongRange(t *testing.T) {
	body := testBody(4000)
	for name, contentRange := range map[string]string{
		"from the start":  "bytes 0-3999/4000",
		"earlier offset":  "bytes 999-3999/4000",
		"later offset":    "bytes 1001-3999/4000",
		"missing":         "",
		"not bytes":       "items 1000-3999/4000",
		"no start":        "bytes -3999/4000",
		"unsatisfied":     "bytes */4000",
		"not a number":    "bytes abc-3999/4000",
		"number overflow": "bytes 99999999999999999999-3999/4000",
	} {
		t.Run(name, func(t *testing.T) {
			o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "" {
					serveHonest(w, r, body, `"v1"`, -1)
					return
				}
				if contentRange != "" {
					w.Header().Set("Content-Range", contentRange)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(body)
			})
			partial := partialPath(t)
			leftover(t, partial, body[:1000], `"v1"`)

			size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
			require.NoError(t, err)
			require.Equal(t, int64(len(body)), size)
			require.Equal(t, sum(body), digest)
			require.Equal(t, body, readFile(t, partial))
			require.Equal(t, []request{{Range: "bytes=1000-", IfRange: `"v1"`}, {}}, o.seen())
		})
	}
}

// A server that answers 206 to a request that carried no Range is not
// trusted: there is no offset the body could belong to.
func TestFaults_PartialContentWithoutRangeRequest(t *testing.T) {
	o := newOrigin(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0123456789"))
	})
	partial := partialPath(t)
	_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.ErrorContains(t, err, "HTTP 206")
	require.Len(t, o.seen(), 1)
	require.NoFileExists(t, partial)
}

// The resource is replaced while its download is interrupted. The resumed
// request must bring the whole new version; no byte of the old one survives.
func TestFaults_ResourceChangesMidDownload(t *testing.T) {
	v1, v2 := testBody(5000), bytes.Repeat([]byte("new!"), 900)
	for name, validators := range map[string][2]string{
		"etag":          {`"v1"`, `"v2"`},
		"last-modified": {"Mon, 02 Jan 2006 15:04:05 GMT", "Tue, 03 Jan 2006 15:04:05 GMT"},
	} {
		t.Run(name, func(t *testing.T) {
			o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 0 {
					serveHonest(w, r, v1, validators[0], 2000)
					return
				}
				serveHonest(w, r, v2, validators[1], -1)
			})
			partial := partialPath(t)
			size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
			require.NoError(t, err)
			require.Equal(t, int64(len(v2)), size)
			require.Equal(t, sum(v2), digest)
			require.Equal(t, v2, readFile(t, partial))
			require.Equal(t, request{Range: "bytes=2000-", IfRange: validators[0]}, o.seen()[1])
			require.Equal(t, validators[1], string(readFile(t, partial+ValidatorSuffix)))
		})
	}
}

// A server that ignores If-Range and sends the tail of a new version produces
// a file that is neither version. Fetch cannot know, so the digest it returns
// must be that of the file as written: it then matches no published digest.
func TestFaults_ServerIgnoringIfRangeIsCaughtByTheDigest(t *testing.T) {
	v1, v2 := testBody(5000), bytes.Repeat([]byte("new!"), 1250)
	o := newOrigin(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Content-Range", "bytes 2000-4999/5000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(v2[2000:])
	})
	partial := partialPath(t)
	leftover(t, partial, v1[:2000], `"v1"`)

	size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	onDisk := readFile(t, partial)
	require.Equal(t, int64(len(onDisk)), size)
	require.Equal(t, sum(onDisk), digest)
	require.NotEqual(t, sum(v1), digest)
	require.NotEqual(t, sum(v2), digest)
}

// An error status is not a download: nothing is written, the request is not
// repeated, and the call after the server recovers succeeds.
func TestFaults_ErrorStatusThenSuccess(t *testing.T) {
	body := testBody(3000)
	for _, status := range []int{
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusTooManyRequests, http.StatusNotFound, http.StatusForbidden,
		http.StatusNoContent, http.StatusNotModified,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 0 {
					w.Header().Set("ETag", `"error-page"`)
					w.WriteHeader(status)
					if status >= 400 {
						_, _ = w.Write([]byte("<html>this is not the artifact</html>"))
					}
					return
				}
				serveHonest(w, r, body, `"v1"`, -1)
			})
			partial := partialPath(t)
			_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
			require.ErrorContains(t, err, "HTTP "+strconv.Itoa(status))
			require.Len(t, o.seen(), 1, "an error status is not retried by Fetch itself")
			require.NoFileExists(t, partial, "an error page must never become the partial file")
			require.NoFileExists(t, partial+ValidatorSuffix)

			size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
			require.NoError(t, err)
			require.Equal(t, int64(len(body)), size)
			require.Equal(t, sum(body), digest)
		})
	}
}

// The server fails while a download is being resumed. The bytes already on
// disk and their validator survive the failure and are continued afterwards.
func TestFaults_ErrorStatusWhileResuming(t *testing.T) {
	body := testBody(3000)
	o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 0:
			serveHonest(w, r, body, `"v1"`, 1200)
		case 1:
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
		default:
			serveHonest(w, r, body, `"v1"`, -1)
		}
	})
	partial := partialPath(t)
	_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.ErrorContains(t, err, "HTTP 503")
	require.Equal(t, body[:1200], readFile(t, partial))

	size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)
	require.Equal(t, sum(body), digest)
	require.Equal(t, request{Range: "bytes=1200-", IfRange: `"v1"`}, o.seen()[2])
}

// The server goes silent in the middle of the body without closing. Fetch
// must give up with a StallError within a bounded time instead of hanging,
// keep what it received, and continue from there once the server answers.
func TestFaults_ServerStallsMidBody(t *testing.T) {
	body := testBody(3000)
	var healed sync.WaitGroup
	stalling := true
	var mu sync.Mutex
	o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stall := stalling
		mu.Unlock()
		if !stall {
			serveHonest(w, r, body, `"v1"`, -1)
			return
		}
		healed.Add(1)
		defer healed.Done()
		// Announce everything, send 400 more bytes, then say nothing until
		// the client hangs up.
		start := n * 400
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
		if n > 0 {
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-2999/3000")
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write(body[start : start+400])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	partial := partialPath(t)
	dl := Resumable{StallTimeout: 100 * time.Millisecond}

	began := time.Now()
	_, _, err := dl.Fetch(context.Background(), o.URL, partial)
	var stalled *StallError
	require.ErrorAs(t, err, &stalled)
	require.Equal(t, 100*time.Millisecond, stalled.Idle)
	require.Less(t, time.Since(began), 20*time.Second, "a silent server must not hold the download")
	require.Len(t, o.seen(), maxResumeAttempts)
	require.Equal(t, body[:400*maxResumeAttempts], readFile(t, partial))

	healed.Wait()
	mu.Lock()
	stalling = false
	mu.Unlock()
	size, digest, err := dl.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)
	require.Equal(t, sum(body), digest)
}

// A server that keeps sending, a few bytes at a time with pauses shorter than
// the stall window, is slow but alive: the download completes however long
// the whole of it takes.
func TestFaults_TrickledBodyCompletes(t *testing.T) {
	body := testBody(40)
	o := newOrigin(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		for i := 0; i < len(body); i += 2 {
			_, _ = w.Write(body[i : i+2])
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	})
	partial := partialPath(t)
	// The whole body takes at least 100ms; the window only bounds each pause.
	size, digest, err := Resumable{StallTimeout: 5 * time.Second}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)
	require.Equal(t, sum(body), digest)
	require.Len(t, o.seen(), 1)
}

// The server accepts the connection and never answers. The client's header
// timeout ends the wait, and a partial file from an earlier run is untouched.
func TestFaults_ServerNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	partial := partialPath(t)
	leftover(t, partial, []byte("earlier bytes"), `"v1"`)

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}}
	defer client.CloseIdleConnections()
	began := time.Now()
	_, _, err := Resumable{Client: client}.Fetch(context.Background(), o.URL, partial)
	require.Error(t, err)
	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	require.True(t, netErr.Timeout(), "got %v", err)
	require.Less(t, time.Since(began), 20*time.Second)
	require.Len(t, o.seen(), 1, "nothing arrived, so there is nothing to resume")
	require.Equal(t, "earlier bytes", string(readFile(t, partial)))
	require.Equal(t, `"v1"`, string(readFile(t, partial+ValidatorSuffix)))
}

// Nobody listens at the address. Fetch fails at once and leaves an earlier
// partial download as it was.
func TestFaults_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	url := "http://" + ln.Addr().String()
	require.NoError(t, ln.Close())

	partial := partialPath(t)
	leftover(t, partial, []byte("earlier bytes"), `"v1"`)
	_, _, err = Resumable{}.Fetch(context.Background(), url, partial)
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
	require.Equal(t, "earlier bytes", string(readFile(t, partial)))
}

// The server closes the connection without sending a response at all.
func TestFaults_ConnectionClosedBeforeResponse(t *testing.T) {
	body := testBody(500)
	o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			raw(t, w, nil)
			return
		}
		serveHonest(w, r, body, `"v1"`, -1)
	})
	partial := partialPath(t)
	_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.Error(t, err)
	require.NoFileExists(t, partial)

	size, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)
}

// The server answers with something that is not HTTP.
func TestFaults_GarbageResponse(t *testing.T) {
	for name, response := range map[string]string{
		"binary":         "\x00\x01\x02\xff\xfe garbage \r\n\r\n",
		"bad status":     "HTTP/1.1 two-hundred OK\r\n\r\n",
		"bad length":     "HTTP/1.1 200 OK\r\nContent-Length: -5\r\n\r\nhello",
		"two lengths":    "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 9\r\n\r\nhello",
		"broken chunked": "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nhello\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			o := newOrigin(t, func(_ int, w http.ResponseWriter, _ *http.Request) { raw(t, w, []byte(response)) })
			partial := partialPath(t)
			_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
			require.Error(t, err)
			if data, rerr := os.ReadFile(partial); rerr == nil {
				require.Empty(t, data, "nothing of a malformed response may be kept")
			}
		})
	}
}

// The caller gives up (Ctrl-C) while the body is arriving. Fetch returns
// promptly without reconnecting, and what arrived can be resumed later.
func TestFaults_CancelledMidBody(t *testing.T) {
	body := testBody(3000)
	sent := make(chan struct{}, 1)
	o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n > 0 {
			serveHonest(w, r, body, `"v1"`, -1)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body[:700])
		w.(http.Flusher).Flush()
		sent <- struct{}{}
		<-r.Context().Done()
	})
	partial := partialPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sent
		// Let the bytes reach the file before giving up; canceling earlier
		// only makes the resume below start from fewer bytes.
		for range 200 {
			if info, err := os.Stat(partial); err == nil && info.Size() == 700 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	_, _, err := Resumable{}.Fetch(ctx, o.URL, partial)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, o.seen(), 1, "a canceled download must not reconnect")
	require.Equal(t, body[:700], readFile(t, partial))

	size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)
	require.Equal(t, sum(body), digest)
	require.Equal(t, "bytes=700-", o.seen()[1].Range)
}

// A context that is already done sends nothing.
func TestFaults_AlreadyCancelled(t *testing.T) {
	o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) { serveHonest(w, r, testBody(10), "", -1) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	partial := partialPath(t)
	_, _, err := Resumable{}.Fetch(ctx, o.URL, partial)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, o.seen())
	require.NoFileExists(t, partial)
}

// A weak ETag does not identify bytes, so it must not be used to resume: with
// nothing stronger the download starts over, and Last-Modified is used when
// the server gives one.
func TestFaults_WeakETagIsNotUsedToResume(t *testing.T) {
	body := testBody(3000)
	t.Run("alone", func(t *testing.T) {
		o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
			cut := -1
			if n == 0 {
				cut = 1000
			}
			serveHonest(w, r, body, `W/"v1"`, cut)
		})
		partial := partialPath(t)
		// A validator left by an earlier, resumable version must not survive.
		leftover(t, partial, nil, `"stale"`)
		size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
		require.NoError(t, err)
		require.Equal(t, int64(len(body)), size)
		require.Equal(t, sum(body), digest)
		require.Equal(t, []request{{}, {}}, o.seen())
		require.NoFileExists(t, partial+ValidatorSuffix)
	})
	t.Run("with last-modified", func(t *testing.T) {
		const modified = "Mon, 02 Jan 2006 15:04:05 GMT"
		o := newOrigin(t, func(n int, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `W/"v1"`)
			cut := -1
			if n == 0 {
				cut = 1000
			}
			serveHonest(w, r, body, modified, cut)
		})
		partial := partialPath(t)
		_, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
		require.NoError(t, err)
		require.Equal(t, sum(body), digest)
		require.Equal(t, request{Range: "bytes=1000-", IfRange: modified}, o.seen()[1])
	})
}

// State left on disk by a killed process or a full disk: each damaged
// combination either resumes correctly or starts over, never both.
func TestFaults_DamagedPartialState(t *testing.T) {
	body := testBody(3000)
	cases := map[string]struct {
		prepare func(t *testing.T, partial string)
		want    request // the single request the download needs
	}{
		"validator without partial": {
			prepare: func(t *testing.T, p string) {
				require.NoError(t, os.WriteFile(p+ValidatorSuffix, []byte(`"v1"`), 0o644))
			},
		},
		"partial without validator": {
			prepare: func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, body[:500], 0o644)) },
		},
		"empty validator": {
			prepare: func(t *testing.T, p string) { leftover(t, p, body[:500], "") },
		},
		"empty partial": {
			prepare: func(t *testing.T, p string) { leftover(t, p, nil, `"v1"`) },
		},
		"both intact": {
			prepare: func(t *testing.T, p string) { leftover(t, p, body[:500], `"v1"`) },
			want:    request{Range: "bytes=500-", IfRange: `"v1"`},
		},
		"validator of another version": {
			prepare: func(t *testing.T, p string) { leftover(t, p, []byte("older bytes"), `"v0"`) },
			want:    request{Range: "bytes=11-", IfRange: `"v0"`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) { serveHonest(w, r, body, `"v1"`, -1) })
			partial := partialPath(t)
			tc.prepare(t, partial)

			size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
			require.NoError(t, err)
			require.Equal(t, int64(len(body)), size)
			require.Equal(t, sum(body), digest)
			require.Equal(t, body, readFile(t, partial))
			require.Equal(t, []request{tc.want}, o.seen())
			require.Equal(t, `"v1"`, string(readFile(t, partial+ValidatorSuffix)))
		})
	}
}

// A leftover partial longer than the resource (junk, or a newer and shorter
// version) is thrown away rather than truncated and trusted.
func TestFaults_PartialLongerThanResource(t *testing.T) {
	body := testBody(1000)
	o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) { serveHonest(w, r, body, `"v1"`, -1) })
	partial := partialPath(t)
	leftover(t, partial, bytes.Repeat([]byte{0xee}, 5000), `"v1"`)

	size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)
	require.Equal(t, sum(body), digest)
	require.Equal(t, body, readFile(t, partial))
}

// The partial file cannot be created or opened. That is a local failure:
// Fetch reports it without asking the server again and again.
func TestFaults_PartialCannotBeWritten(t *testing.T) {
	body := testBody(1000)
	serve := func(_ int, w http.ResponseWriter, r *http.Request) { serveHonest(w, r, body, `"v1"`, -1) }

	t.Run("directory missing", func(t *testing.T) {
		o := newOrigin(t, serve)
		partial := filepath.Join(t.TempDir(), "gone", "a.partial")
		_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.Len(t, o.seen(), 1)
	})
	t.Run("path is a directory", func(t *testing.T) {
		o := newOrigin(t, serve)
		partial := partialPath(t)
		require.NoError(t, os.Mkdir(partial, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(partial, "keep"), []byte("x"), 0o644))
		_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
		require.Error(t, err)
		require.Len(t, o.seen(), 1)
		require.FileExists(t, filepath.Join(partial, "keep"), "a directory in the way is not deleted")
	})
	t.Run("read-only directory", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a directory the test cannot write to")
		}
		o := newOrigin(t, serve)
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		_, _, err := Resumable{}.Fetch(context.Background(), o.URL, filepath.Join(dir, "a.partial"))
		require.ErrorIs(t, err, os.ErrPermission)
		require.Len(t, o.seen(), 1)
	})
}

// The resource changed, the server sends the new version in full, and the
// partial file holding the old bytes cannot be rewritten. The validator on
// disk must keep describing the bytes beside it: were it replaced by the new
// one, the next resume would join old bytes to the new version's tail.
func TestFaults_ValidatorNeverVouchesForOlderBytes(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file the test cannot write to")
	}
	v1, v2 := testBody(3000), bytes.Repeat([]byte("new!"), 750)
	o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) { serveHonest(w, r, v2, `"v2"`, -1) })
	partial := partialPath(t)
	leftover(t, partial, v1[:1000], `"v1"`)
	require.NoError(t, os.Chmod(partial, 0o400))

	_, _, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.Error(t, err)
	if offset, validator := resumePoint(partial); offset > 0 {
		require.Equal(t, `"v1"`, validator, "%d old bytes are kept, so the validator must still be theirs", offset)
	}

	// Once the file is writable again the download completes with the new
	// version only.
	require.NoError(t, os.Chmod(partial, 0o600))
	size, digest, err := Resumable{}.Fetch(context.Background(), o.URL, partial)
	require.NoError(t, err)
	require.Equal(t, int64(len(v2)), size)
	require.Equal(t, sum(v2), digest)
	require.Equal(t, v2, readFile(t, partial))
}

// MaxSize bounds what is written even when the server sends far more or the
// download is resumed: the result is at most one byte over, which is what
// tells the caller the artifact is too large.
func TestFaults_MaxSize(t *testing.T) {
	body := testBody(5000)
	serve := func(_ int, w http.ResponseWriter, r *http.Request) { serveHonest(w, r, body, `"v1"`, -1) }

	t.Run("exact size is not over", func(t *testing.T) {
		o := newOrigin(t, serve)
		partial := partialPath(t)
		size, digest, err := Resumable{MaxSize: int64(len(body))}.Fetch(context.Background(), o.URL, partial)
		require.NoError(t, err)
		require.Equal(t, int64(len(body)), size)
		require.Equal(t, sum(body), digest)
	})
	t.Run("resumed download stops one byte over", func(t *testing.T) {
		o := newOrigin(t, serve)
		partial := partialPath(t)
		leftover(t, partial, body[:1000], `"v1"`)
		size, digest, err := Resumable{MaxSize: 2000}.Fetch(context.Background(), o.URL, partial)
		require.NoError(t, err)
		require.Equal(t, int64(2001), size)
		require.Equal(t, sum(body[:2001]), digest)
		require.Len(t, readFile(t, partial), 2001)
	})
	t.Run("leftover already over the limit", func(t *testing.T) {
		o := newOrigin(t, serve)
		partial := partialPath(t)
		leftover(t, partial, body[:3000], `"v1"`)
		size, _, err := Resumable{MaxSize: 2000}.Fetch(context.Background(), o.URL, partial)
		require.NoError(t, err)
		require.Greater(t, size, int64(2000), "the caller must see that the artifact is over its limit")
		require.Len(t, readFile(t, partial), 3000, "nothing more is written past the limit")
	})
	t.Run("endless body", func(t *testing.T) {
		o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			chunk := bytes.Repeat([]byte{'x'}, 4096)
			for r.Context().Err() == nil {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		})
		partial := partialPath(t)
		size, _, err := Resumable{MaxSize: 10000}.Fetch(context.Background(), o.URL, partial)
		require.NoError(t, err)
		require.Equal(t, int64(10001), size)
		require.Len(t, readFile(t, partial), 10001)
	})
}

func TestFaults_CommitPartial(t *testing.T) {
	dir := t.TempDir()
	partial, dest := filepath.Join(dir, "a.partial"), filepath.Join(dir, "a.bin")

	// Nothing was downloaded: the installed file stays as it is.
	require.NoError(t, os.WriteFile(dest, []byte("installed"), 0o644))
	require.Error(t, CommitPartial(partial, dest))
	require.Equal(t, "installed", string(readFile(t, dest)))

	// The destination directory is gone: the download is kept for a retry.
	leftover(t, partial, []byte("downloaded"), `"v1"`)
	require.Error(t, CommitPartial(partial, filepath.Join(dir, "gone", "a.bin")))
	require.Equal(t, "downloaded", string(readFile(t, partial)))
	require.FileExists(t, partial+ValidatorSuffix)

	// A commit replaces the installed file in one step and leaves no state.
	require.NoError(t, CommitPartial(partial, dest))
	require.Equal(t, "downloaded", string(readFile(t, dest)))
	require.NoFileExists(t, partial)
	require.NoFileExists(t, partial+ValidatorSuffix)

	// Discarding what is not there is not an error path.
	DiscardPartial(partial)
}

// A server that redirects to itself for ever is cut off instead of followed.
func TestFaults_RedirectLoop(t *testing.T) {
	o := newOrigin(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	})
	client := &http.Client{CheckRedirect: noDowngrade}
	_, _, err := Resumable{Client: client}.Fetch(context.Background(), o.URL, partialPath(t))
	require.ErrorContains(t, err, "stopped after 10 redirects")
	require.Len(t, o.seen(), 10)
}

type failingWriter struct {
	room int
	err  error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.room {
		n := w.room
		w.room = 0
		return n, w.err
	}
	w.room -= len(p)
	return len(p), nil
}

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

// The disk fills up, or the connection breaks, while the body is copied: the
// cause reaches the caller as it is (not as a stall) with the bytes that made
// it, and the request is not canceled behind the caller's back.
func TestFaults_CopyWithStallReportsTheCause(t *testing.T) {
	canceled := false
	cancel := func() { canceled = true }

	n, err := CopyWithStall(&failingWriter{room: 10, err: syscall.ENOSPC}, bytes.NewReader(make([]byte, 100)), time.Minute, cancel)
	require.ErrorIs(t, err, syscall.ENOSPC)
	require.Equal(t, int64(10), n)

	n, err = CopyWithStall(&failingWriter{room: 10}, bytes.NewReader(make([]byte, 100)), time.Minute, cancel)
	require.ErrorIs(t, err, io.ErrShortWrite, "a write that silently takes fewer bytes is an error")
	require.Equal(t, int64(10), n)

	var sink bytes.Buffer
	n, err = CopyWithStall(&sink, &failingReader{data: []byte("abc"), err: syscall.ECONNRESET}, time.Minute, cancel)
	require.ErrorIs(t, err, syscall.ECONNRESET)
	var stalled *StallError
	require.NotErrorAs(t, err, &stalled)
	require.Equal(t, int64(3), n)
	require.Equal(t, "abc", sink.String())
	require.False(t, canceled)

	// A non-positive window means the default, not "stalled at once".
	n, err = CopyWithStall(&sink, strings.NewReader("xyz"), 0, cancel)
	require.NoError(t, err)
	require.Equal(t, int64(3), n)
	require.False(t, canceled)
}

func TestContentRangeStart(t *testing.T) {
	for in, want := range map[string]int64{
		"bytes 0-9/10":      0,
		"bytes 1000-1999/*": 1000,
		"bytes 5-":          5,
		"bytes 9223372036854775807-9223372036854775807/9223372036854775807": 1<<63 - 1,
	} {
		got, ok := contentRangeStart(in)
		require.True(t, ok, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []string{
		"", "bytes", "bytes ", "bytes 5", "bytes */10", "bytes -5/10", "bytes  5-9/10", "bytes 5 -9/10",
		"BYTES 5-9/10", "bytes=5-9", " bytes 5-9/10", "bytes 0x10-20/30", "bytes 9223372036854775808-/1", "bytes 1.5-2/3",
	} {
		_, ok := contentRangeStart(in)
		require.False(t, ok, "%q", in)
	}
}

// Whatever a server puts in Content-Range, the offset taken from it is one a
// file can have; anything else is refused.
func FuzzContentRangeStart(f *testing.F) {
	for _, seed := range []string{
		"bytes 0-9/10", "bytes 5-", "bytes */10", "bytes -1-2/3", "bytes +7-9/10", "", "bytes ", "items 1-2/3",
		"bytes 99999999999999999999-1/2", "bytes \x00-", "bytes 5-9/10\r\nX: y",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v string) {
		n, ok := contentRangeStart(v)
		if !ok {
			return
		}
		if n < 0 {
			t.Fatalf("contentRangeStart(%q) = %d: a negative offset", v, n)
		}
		if !strings.HasPrefix(v, "bytes ") {
			t.Fatalf("contentRangeStart(%q) accepted a unit other than bytes", v)
		}
	})
}
