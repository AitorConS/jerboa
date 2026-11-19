package httpauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func from(remote, token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func serve(h http.Handler, req *http.Request) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// /metrics answered every wrong token at once, so a token could be guessed as
// fast as the network allowed. A source over its budget is now refused before
// its token is looked at, and that costs the server nothing but a map entry.
func TestBearer_LimitsWrongTokensPerSource(t *testing.T) {
	h := Bearer("s3cret", okHandler())
	for i := range failureBurst {
		require.Equal(t, http.StatusUnauthorized, serve(h, from("192.0.2.7:1000", fmt.Sprintf("guess-%d", i))))
	}
	require.Equal(t, http.StatusTooManyRequests, serve(h, from("192.0.2.7:1001", "guess")))
	// Even the right token: the source cannot learn which guess was right.
	require.Equal(t, http.StatusTooManyRequests, serve(h, from("192.0.2.7:1002", "s3cret")))
	// Another source is not affected.
	require.Equal(t, http.StatusOK, serve(h, from("192.0.2.8:1000", "s3cret")))
}

func TestFailureLimiter_Refills(t *testing.T) {
	l := NewFailureLimiter()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	req := from("192.0.2.7:1000", "")
	for range failureBurst {
		require.True(t, l.Allow(req))
		l.Fail(req)
	}
	require.False(t, l.Allow(req))
	now = now.Add(failureRefill)
	require.True(t, l.Allow(req), "one more try after the refill period")
	l.Fail(req)
	require.False(t, l.Allow(req))
}

func TestFailureLimiter_TableIsBounded(t *testing.T) {
	l := NewFailureLimiter()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	for i := range maxFailureSources + 100 {
		req := from(fmt.Sprintf("10.%d.%d.%d:1", i>>16&0xff, i>>8&0xff, i&0xff), "")
		l.Fail(req)
	}
	require.LessOrEqual(t, len(l.sources), maxFailureSources)

	// Spreading over fresh addresses once the table is full only drains the
	// shared overflow budget.
	for i := range failureBurst {
		l.Fail(from(fmt.Sprintf("172.16.0.%d:1", i), ""))
	}
	require.False(t, l.Allow(from("172.16.1.1:1", "")))

	// Sources that recovered their whole budget make room again.
	now = now.Add(failureBurst * failureRefill)
	l.Fail(from("172.16.1.2:1", ""))
	require.True(t, l.Allow(from("172.16.1.3:1", "")))
	require.LessOrEqual(t, len(l.sources), maxFailureSources)
}

// One customer's IPv6 prefix is one source: spreading guesses over its
// addresses, or its /64s, shares one budget; another /56 has its own.
func TestFailureLimiter_IPv6SourcesShareTheirPrefix(t *testing.T) {
	l := NewFailureLimiter()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	for i := range failureBurst {
		l.Fail(from(fmt.Sprintf("[2001:db8:0:%x::%x]:1", i, i+1), ""))
	}
	require.False(t, l.Allow(from("[2001:db8:0:ff::1]:1", "")), "another /64 of the same /56")
	require.True(t, l.Allow(from("[2001:db8:0:100::1]:1", "")), "the next /56")
	require.Len(t, l.sources, 1)
	require.Equal(t, "192.0.2.7", sourceOf(from("[::ffff:192.0.2.7]:1", "")))
}

// Parallel attempts from one source are charged as they are checked: no more
// than the burst get through, however many run at once.
func TestFailureLimiter_TakeIsAtomic(t *testing.T) {
	l := NewFailureLimiter()
	var taken atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Take(from("192.0.2.7:1000", "")) {
				taken.Add(1)
			}
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, taken.Load(), int32(failureBurst+1), "one refill may land during the run")
	require.Positive(t, taken.Load())
}

// A right token costs nothing: Refund gives back what Take charged.
func TestFailureLimiter_RefundRestoresTheBudget(t *testing.T) {
	l := NewFailureLimiter()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	req := from("192.0.2.7:1000", "")
	for range 3 * failureBurst {
		require.True(t, l.Take(req))
		l.Refund(req)
	}
	for range failureBurst {
		require.True(t, l.Take(req))
	}
	require.False(t, l.Take(req))
}
