//go:build linux

package vm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePortMap(t *testing.T) {
	cases := []struct {
		input   string
		want    PortMap
		wantErr bool
	}{
		{
			input: "8080:80",
			want:  PortMap{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP},
		},
		{
			input: "8080:80/tcp",
			want:  PortMap{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP},
		},
		{
			input: "5353:53/udp",
			want:  PortMap{HostPort: 5353, GuestPort: 53, Protocol: ProtocolUDP},
		},
		{
			input: "443:443/TCP", // uppercase protocol normalised
			want:  PortMap{HostPort: 443, GuestPort: 443, Protocol: ProtocolTCP},
		},
		{
			input: "3000:3000",
			want:  PortMap{HostPort: 3000, GuestPort: 3000, Protocol: ProtocolTCP},
		},
		// error cases
		{input: "80", wantErr: true},
		{input: "0:80", wantErr: true},
		{input: "80:0", wantErr: true},
		{input: "abc:80", wantErr: true},
		{input: "80:abc", wantErr: true},
		{input: "8080:80/sctp", wantErr: true},
		{input: "65536:80", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParsePortMap(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestPortMap_String(t *testing.T) {
	pm := PortMap{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}
	require.Equal(t, "8080:80/tcp", pm.String())
}

func TestParsePortMaps_multiple(t *testing.T) {
	specs := []string{"8080:80", "5353:53/udp", "443:443/tcp"}
	got, err := ParsePortMaps(specs)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, PortMap{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}, got[0])
	require.Equal(t, PortMap{HostPort: 5353, GuestPort: 53, Protocol: ProtocolUDP}, got[1])
	require.Equal(t, PortMap{HostPort: 443, GuestPort: 443, Protocol: ProtocolTCP}, got[2])
}

func TestParsePortMaps_error_propagates(t *testing.T) {
	_, err := ParsePortMaps([]string{"8080:80", "bad"})
	require.Error(t, err)
}
