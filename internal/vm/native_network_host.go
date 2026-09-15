//go:build darwin && arm64

package vm

import "sync"

// Both VMMs use the same host switch lifecycle on macOS.
type nativeNetworkHost struct {
	nativeMu     sync.Mutex
	nativeState  nativeNetworkState
	guestDNS     func([]byte, string) ([]byte, error)
	nativeEgress func(string, string, uint16, bool) bool
}
