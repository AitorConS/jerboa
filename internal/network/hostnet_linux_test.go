//go:build linux

package network

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const procRoute = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
jerboa-br-a	0000640A	00000000	0001	0	0	0	00FFFFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
`

func TestRouteConflict(t *testing.T) {
	check := func(cidr string) string {
		_, n, err := net.ParseCIDR(cidr)
		require.NoError(t, err)
		return routeConflict(strings.NewReader(procRoute), n)
	}
	require.Equal(t, "route 192.168.1.0/24 on eth0", check("192.168.1.128/25"))
	require.Equal(t, "route 192.168.1.0/24 on eth0", check("192.168.0.0/16"))
	require.Equal(t, "route 172.17.0.0/16 on docker0", check("172.17.5.0/24"))
	require.Empty(t, check("10.100.0.0/24"), "Jerboa's own bridges are not a conflict")
	require.Empty(t, check("10.20.0.0/24"), "the default route is not a conflict")
}

func TestCheckBridgeCIDR(t *testing.T) {
	require.NoError(t, checkBridgeCIDR("10.100.0.1/24"))
	require.NoError(t, checkBridgeCIDR("192.168.1.129/25"))
	require.Error(t, checkBridgeCIDR("1.1.0.1/16"), "public subnet")
	require.Error(t, checkBridgeCIDR("10.100.0.5/24"), "gateway not first address")
	require.Error(t, checkBridgeCIDR("192.168.1.1/25x"))
	withHostRoute(t, "10.100.0.0/24")
	require.Error(t, checkBridgeCIDR("10.100.0.1/24"), "host route")
}
