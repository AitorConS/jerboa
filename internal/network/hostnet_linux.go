//go:build linux

package network

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// hostNetworkConflict names the first host interface address or route that
// overlaps ipNet. Jerboa's own bridges and the default route are ignored.
func hostNetworkConflict(ipNet *net.IPNet) string {
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if strings.HasPrefix(iface.Name, managedBridgePrefix) {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && subnetsOverlap(ipNet, n) {
					return fmt.Sprintf("address %s on %s", n, iface.Name)
				}
			}
		}
	}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return routeConflict(f, ipNet)
}

// routeConflict reads a /proc/net/route table and names the first route other
// than the default one, and not on a Jerboa bridge, that overlaps ipNet.
func routeConflict(r io.Reader, ipNet *net.IPNet) string {
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || strings.HasPrefix(f[0], managedBridgePrefix) {
			continue
		}
		dst, ok1 := routeAddr(f[1])
		mask, ok2 := routeAddr(f[7])
		if !ok1 || !ok2 {
			continue
		}
		route := &net.IPNet{IP: dst, Mask: net.IPMask(mask)}
		if ones, _ := route.Mask.Size(); ones == 0 {
			continue
		}
		if subnetsOverlap(ipNet, route) {
			return fmt.Sprintf("route %s on %s", route, f[0])
		}
	}
	return ""
}

// routeAddr decodes a /proc/net/route address: 8 hex digits in host (little
// endian) byte order.
func routeAddr(s string) (net.IP, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return nil, false
	}
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, binary.LittleEndian.Uint32(b))
	return ip, true
}
