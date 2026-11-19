package api

import "time"

// SetHandshakeTimeout replaces the handshake deadline and returns a function
// that restores it.
func SetHandshakeTimeout(d time.Duration) func() {
	old := handshakeTimeout
	handshakeTimeout = d
	return func() { handshakeTimeout = old }
}

// SetWatchdog replaces how long a call waits before asking the daemon
// whether it is still there, and how often it asks again; it returns a
// function that restores both.
func SetWatchdog(delay, interval time.Duration) func() {
	oldDelay, oldInterval := watchdogDelay, watchdogInterval
	watchdogDelay, watchdogInterval = delay, interval
	return func() { watchdogDelay, watchdogInterval = oldDelay, oldInterval }
}

// SetBusyRetryDelays replaces the waits between attempts when the daemon is
// busy and returns a function that restores them.
func SetBusyRetryDelays(d ...time.Duration) func() {
	old := busyRetryDelays
	busyRetryDelays = d
	return func() { busyRetryDelays = old }
}
