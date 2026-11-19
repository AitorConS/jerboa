package httpauth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Failure budget per source address: a burst of failureBurst wrong tokens,
// then one more every failureRefill. An honest client with a stale token
// fails a few times and stops; anything faster is guessing.
const (
	failureBurst  = 10
	failureRefill = time.Second
	// maxFailureSources bounds the table. When it is full and no entry has
	// recovered its whole budget, new sources share one overflow budget, so
	// an attacker spreading over many addresses gains nothing by filling it.
	//
	// The trade-off, accepted: an attacker who keeps maxFailureSources
	// sources each failing about once a second (an entry recovers its budget
	// a second after one failure) also uses up the overflow budget of every
	// other source, which is then refused, the right token included, until
	// the attack stops. Keying IPv6 sources by /56 (sourceOf) puts that out of
	// reach of one customer's prefix; it needs thousands of IPv4 addresses or
	// separate IPv6 allocations. Guessing a token of 32 or more characters is
	// out of reach either way; the limiter keeps failures from being cheap.
	maxFailureSources = 4096
)

// overflowSource is the key every source shares once the table is full.
const overflowSource = "\x00overflow"

// FailureLimiter limits how fast one source address can try tokens. It never
// sleeps or holds a request: a source over its budget is answered at once,
// before its token is checked, so the limiter cannot be used to tie up
// goroutines, and its memory is bounded by maxFailureSources.
type FailureLimiter struct {
	mu      sync.Mutex
	sources map[string]*failureBudget
	now     func() time.Time
}

type failureBudget struct {
	tokens float64
	at     time.Time
}

// NewFailureLimiter returns an empty limiter.
func NewFailureLimiter() *FailureLimiter {
	return &FailureLimiter{sources: make(map[string]*failureBudget), now: time.Now}
}

// ipv6SourceBits is the prefix an IPv6 source is counted by: one customer
// usually gets a /48 or a /56, so counting each address, or each /64, would
// let one customer spread over the whole table.
const ipv6SourceBits = 56

// sourceOf is the connection's source address, never a forwarded header: an
// IPv4 address, or the /56 an IPv6 address lies in.
func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(ipv6SourceBits, 128)).String() + "/56"
}

// refill brings b up to date at now and returns it.
func refill(b *failureBudget, now time.Time) *failureBudget {
	b.tokens += float64(now.Sub(b.at)) / float64(failureRefill)
	if b.tokens > failureBurst {
		b.tokens = failureBurst
	}
	b.at = now
	return b
}

// budgetLocked returns the budget of source, or nil when the source has none
// recorded (a full budget). With create, it records one, making room by
// dropping sources that have recovered their whole budget, or falling back to
// the shared overflow budget.
func (l *FailureLimiter) budgetLocked(source string, now time.Time, create bool) *failureBudget {
	if b, ok := l.sources[source]; ok {
		return refill(b, now)
	}
	if !create {
		if l.fullLocked() {
			if b, ok := l.sources[overflowSource]; ok {
				return refill(b, now)
			}
		}
		return nil
	}
	if l.fullLocked() {
		for k, b := range l.sources {
			if k != overflowSource && refill(b, now).tokens >= failureBurst {
				delete(l.sources, k)
			}
		}
	}
	if l.fullLocked() {
		source = overflowSource
		if b, ok := l.sources[source]; ok {
			return refill(b, now)
		}
	}
	b := &failureBudget{tokens: failureBurst, at: now}
	l.sources[source] = b
	return b
}

// fullLocked reports whether every per-source slot is taken; the overflow
// budget has a slot of its own, so the table never exceeds maxFailureSources.
func (l *FailureLimiter) fullLocked() bool {
	n := len(l.sources)
	if _, ok := l.sources[overflowSource]; ok {
		n--
	}
	return n >= maxFailureSources-1
}

// Allow reports whether r's source may have its token checked. A false answer
// means the source used up its budget of failures: refuse the request without
// checking the token (Refuse writes the usual answer).
func (l *FailureLimiter) Allow(r *http.Request) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.budgetLocked(sourceOf(r), l.now(), false)
	return b == nil || b.tokens >= 1
}

// Fail records a wrong token from r's source.
func (l *FailureLimiter) Fail(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.budgetLocked(sourceOf(r), l.now(), true)
	if b.tokens >= 1 {
		b.tokens--
	} else {
		b.tokens = 0
	}
}

// Take charges one attempt to r's source before its token is checked, and
// reports false, charging nothing, when the source has none left: refuse the
// request without checking the token. Checking and charging under one lock
// means parallel attempts from one source cannot all be checked before any is
// charged. Call Refund when the token was right.
func (l *FailureLimiter) Take(r *http.Request) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if b := l.budgetLocked(sourceOf(r), now, false); b != nil && b.tokens < 1 {
		return false
	}
	b := l.budgetLocked(sourceOf(r), now, true)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Refund gives back the attempt Take charged to r's source, whose token was
// right.
func (l *FailureLimiter) Refund(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.budgetLocked(sourceOf(r), l.now(), false); b != nil {
		b.tokens = min(b.tokens+1, failureBurst)
	}
}

// Refuse answers a request from a source over its budget.
func Refuse(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "too many failed attempts; try again later", http.StatusTooManyRequests)
}
