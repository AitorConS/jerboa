//go:build linux || (darwin && arm64)

package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sessionTTL is how long a dashboard sign-in lasts.
const sessionTTL = 12 * time.Hour

// sessions issues and checks the dashboard's sign-in cookies. A cookie is
// scoped by host name, not port, so a browser that signed in also sends it to
// every other listener on the same host (a guest's published port, another
// user's server). The cookie therefore never holds the observability token:
// it holds an expiry and an HMAC of it under a key that lives only in this
// process, which opens nothing but this dashboard, and only until it expires
// or the daemon restarts. It is not accepted as a bearer token for /metrics.
//
// The MAC also covers the address the browser signed in from, so a cookie
// that leaked to another listener is refused when that listener replays it
// from its own address (a guest on its bridge address, another machine).
type sessions struct {
	key  []byte
	name string
	now  func() time.Time
}

// newSessions returns a session issuer for the dashboard bound to addr. The
// cookie name carries the port so dashboards on different ports of one host
// do not overwrite each other's sign-in.
func newSessions(addr string) *sessions {
	key := make([]byte, 32)
	_, _ = rand.Read(key) // crypto/rand.Read does not fail
	name := "jerboa_ui"
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		name += "_" + port
	}
	return &sessions{key: key, name: name, now: time.Now}
}

// cookieName is the cookie for r. Over TLS it takes the __Host- prefix, which
// the browser only accepts from a secure origin with Path=/ and no Domain.
func (s *sessions) cookieName(r *http.Request) string {
	if r.TLS != nil {
		return "__Host-" + s.name
	}
	return s.name
}

func (s *sessions) sign(name string, expires int64, client string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(name + "|" + strconv.FormatInt(expires, 10) + "|" + client))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// clientOf is the address a session is bound to: the connection's source
// address, never a forwarded header, which the client writes itself. Every
// loopback address is one client, so a browser that reaches "localhost" over
// ::1 one time and 127.0.0.1 the next keeps its sign-in; anything able to
// connect from loopback is on this host already.
func clientOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return host
	case ip.IsLoopback():
		return "loopback"
	case ip.To4() != nil:
		return ip.To4().String()
	default:
		return ip.String()
	}
}

// issue sets a fresh sign-in cookie on w.
func (s *sessions) issue(w http.ResponseWriter, r *http.Request) {
	name := s.cookieName(r)
	expires := s.now().Add(sessionTTL).Unix()
	// Secure only over TLS: the dashboard is usually served over plain HTTP
	// on loopback, where a Secure cookie is never sent.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name:     name,
		Value:    strconv.FormatInt(expires, 10) + "." + s.sign(name, expires, clientOf(r)),
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
}

// valid reports whether r carries an unexpired sign-in cookie issued by s to
// the address r comes from.
func (s *sessions) valid(r *http.Request) bool {
	name := s.cookieName(r)
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || s.now().Unix() >= expires {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.sign(name, expires, clientOf(r))))
}
