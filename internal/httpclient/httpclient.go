// Package httpclient centralizes the HTTP clients Jerboa uses so timeout policy
// is set in one place.
//
// Two clients exist because small metadata fetches and large artifact downloads
// need opposite timeout policies:
//
//   - Default bounds the WHOLE request (headers + body) with http.Client.Timeout.
//     That is correct for a small JSON manifest or signature: if the peer stalls,
//     the read must not hang forever, and 30s is plenty to transfer a few KiB.
//
//   - Streaming sets NO total deadline. http.Client.Timeout counts the entire
//     body read against a single clock, so a large but perfectly healthy download
//     (a ~75 MiB rootfs, a 190 MiB runtime package) is aborted the moment it
//     exceeds the deadline on any connection slower than size/deadline — a
//     deterministic failure on ordinary broadband. Instead the transport bounds
//     the parts that SHOULD be quick (dial, TLS handshake, waiting for response
//     headers) and callers guard the body with CopyWithStall, which aborts only
//     when the transfer actually stalls, never when it is merely long.
package httpclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Default bounds an entire request with a total deadline. Use it for small
// metadata fetches (manifests, signatures, index JSON), never for streaming a
// large artifact to disk — see the package doc and Streaming.
var Default = &http.Client{Timeout: 30 * time.Second, CheckRedirect: noDowngrade}

// noDowngrade follows redirects as net/http does by default, except that a
// request which started over https is never sent on over plain http.
func noDowngrade(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from %s to non-https %s", via[0].URL.Host, req.URL.Redacted())
	}
	return nil
}

// Streaming is for large, streamed downloads. It deliberately sets no
// Client.Timeout: liveness comes from the transport's connection-setup timeouts
// plus caller-side stall detection (CopyWithStall), so a slow-but-progressing
// transfer of any size completes instead of being killed by a total deadline.
var Streaming = &http.Client{
	CheckRedirect: noDowngrade,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
		// Bound only the wait for the first response byte (headers), not the body.
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// DefaultStallTimeout is how long CopyWithStall waits with zero bytes read
// before treating a streamed download as dead. It is a var so tests can lower it
// and a future flag can tune it.
var DefaultStallTimeout = 60 * time.Second

// StallError reports that a streamed transfer made no progress within the
// allowed idle window. It is distinct from an ordinary I/O error so callers and
// tests can classify a stall specifically.
type StallError struct {
	Idle time.Duration
}

func (e *StallError) Error() string {
	return fmt.Sprintf("download stalled: no data received for %s", e.Idle)
}

// progressReader wraps a reader and reports the size of each successful read so
// a watchdog can tell whether a transfer is still making progress.
type progressReader struct {
	r      io.Reader
	onRead func(n int)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.onRead != nil {
		p.onRead(n)
	}
	// Return the underlying error verbatim: progressReader is an io.Reader and
	// io.Copy detects end-of-stream by comparing the error to io.EOF by identity
	// (==), so wrapping it would make io.Copy treat a healthy EOF as a failure.
	return n, err //nolint:wrapcheck // io.EOF sentinel must reach io.Copy unwrapped
}

// CopyWithStall copies src into dst and aborts the transfer if no bytes arrive
// for idle. Unlike a total deadline it never interrupts a healthy transfer,
// however large: the clock only advances while the connection is idle.
//
// cancel MUST abort src's underlying read — pass the CancelFunc of the context
// used to build the *http.Request whose Body is src. A read blocked on a dead
// TCP socket does not return on its own; canceling the request context is what
// unblocks it so io.Copy can return. When the abort is triggered by a stall,
// CopyWithStall returns a *StallError (not the resulting "context canceled").
func CopyWithStall(dst io.Writer, src io.Reader, idle time.Duration, cancel context.CancelFunc) (int64, error) {
	if idle <= 0 {
		idle = DefaultStallTimeout
	}
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	pr := &progressReader{r: src, onRead: func(int) {
		lastProgress.Store(time.Now().UnixNano())
	}}

	done := make(chan struct{})
	var stalled atomic.Bool
	go func() {
		// Poll at a fraction of the idle window so a stall is detected within
		// ~idle, not up to 2*idle later.
		tick := idle / 4
		if tick <= 0 {
			tick = idle
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				last := time.Unix(0, lastProgress.Load())
				if time.Since(last) >= idle {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	n, err := io.Copy(dst, pr)
	close(done)
	if stalled.Load() {
		return n, &StallError{Idle: idle}
	}
	if err != nil {
		return n, fmt.Errorf("copy download stream: %w", err)
	}
	return n, nil
}
