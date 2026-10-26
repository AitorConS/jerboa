//go:build darwin && arm64

package network

import "net"

// hostNetworkConflict reports nothing on macOS: networks there are userspace
// switches and never add an address or route to the host.
func hostNetworkConflict(*net.IPNet) string { return "" }
