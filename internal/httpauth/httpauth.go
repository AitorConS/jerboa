// Package httpauth provides a small, constant-time bearer-token middleware for
// the daemon's optional-auth HTTP surfaces (Prometheus metrics, web dashboard).
// Centralizing the check keeps every endpoint's authentication behavior
// identical instead of re-implementing it per server.
package httpauth

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// Bearer wraps next so that, when token is non-empty, every request must carry a
// matching "Authorization: Bearer <token>" header, compared in constant time.
// A cookie is never accepted: the browser sends one to every port of the
// host, so a cookie holding the token would reach other listeners there. The
// dashboard keeps its own session cookie instead.
//
// A source address that keeps sending wrong tokens is answered 429 without its
// token being checked (see FailureLimiter).
//
// An empty token disables the check and returns next unchanged, so a caller can
// pass an operator-supplied token through unconditionally: no token configured
// means the endpoint stays open exactly as it was before.
func Bearer(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	limiter := NewFailureLimiter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Take(r) {
			Refuse(w)
			return
		}
		if !Authorized(token, r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		limiter.Refund(r)
		next.ServeHTTP(w, r)
	})
}

// Authorized reports whether r carries token in its Authorization header.
func Authorized(token string, r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// LoopbackHost wraps next so that, when addr binds only the loopback
// interface, a request must name a loopback host. A web page cannot then read
// the endpoint by pointing a domain it controls at 127.0.0.1 (DNS
// rebinding): its requests carry that domain in Host and get 421. Endpoints
// bound to other addresses are left alone; those require a token instead.
func LoopbackHost(addr string, next http.Handler) http.Handler {
	if !IsLoopbackAddr(addr) {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if !strings.EqualFold(host, "localhost") {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				http.Error(w, "misdirected request: use localhost or 127.0.0.1", http.StatusMisdirectedRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// IsLoopbackAddr reports whether addr's host is the loopback interface. A bare
// port (":9090") or a wildcard host ("0.0.0.0", "::") is treated as
// non-loopback because it is reachable from off-host.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}
