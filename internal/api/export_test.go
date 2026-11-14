package api

import "time"

// SetHandshakeTimeout replaces the handshake deadline and returns a function
// that restores it.
func SetHandshakeTimeout(d time.Duration) func() {
	old := handshakeTimeout
	handshakeTimeout = d
	return func() { handshakeTimeout = old }
}
