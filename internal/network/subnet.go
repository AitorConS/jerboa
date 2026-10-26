//go:build linux || (darwin && arm64)

package network

import (
	"fmt"
	"net"

	"github.com/AitorConS/jerboa/internal/netconst"
)

// A network's subnet becomes an address and a connected route on the host's
// bridge (Linux), so it must not capture traffic the host sends elsewhere: it
// has to lie in a private range, leave the reserved DNS address alone and not
// overlap the host's own interfaces or routes.
const (
	minSubnetPrefix = 16
	maxSubnetPrefix = 29
)

var privateRanges = []*net.IPNet{
	parseFixedCIDR("10.0.0.0/8"),
	parseFixedCIDR("172.16.0.0/12"),
	parseFixedCIDR("192.168.0.0/16"),
	parseFixedCIDR("100.64.0.0/10"),
}

func parseFixedCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// hostConflict names a host interface address or route that overlaps a
// subnet, or returns "". It is a variable so tests do not depend on the
// machine's own networks.
var hostConflict = hostNetworkConflict

// checkSubnet reports why ipNet cannot be a network's subnet, or nil.
func checkSubnet(ipNet *net.IPNet) error {
	if ipNet.IP.To4() == nil {
		return fmt.Errorf("only IPv4 subnets are supported")
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 || ones < minSubnetPrefix || ones > maxSubnetPrefix {
		return fmt.Errorf("prefix length must be /%d to /%d", minSubnetPrefix, maxSubnetPrefix)
	}
	if !inPrivateRange(ipNet) {
		return fmt.Errorf("must lie in a private range (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 or 100.64.0.0/10)")
	}
	if ipNet.Contains(net.ParseIP(netconst.DNSAnycastIP)) {
		return fmt.Errorf("contains the reserved DNS address %s", netconst.DNSAnycastIP)
	}
	if conflict := hostConflict(ipNet); conflict != "" {
		return fmt.Errorf("overlaps %s on this host", conflict)
	}
	return nil
}

func inPrivateRange(ipNet *net.IPNet) bool {
	ones, _ := ipNet.Mask.Size()
	for _, r := range privateRanges {
		rOnes, _ := r.Mask.Size()
		if r.Contains(ipNet.IP) && ones >= rOnes {
			return true
		}
	}
	return false
}

// firstHost is the subnet's first usable address, the network's gateway.
func firstHost(ipNet *net.IPNet) net.IP {
	ip := make(net.IP, net.IPv4len)
	copy(ip, ipNet.IP.To4())
	ip[3]++
	return ip
}
