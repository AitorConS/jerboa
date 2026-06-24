package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePortMap(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    PortMapSpec
		wantErr bool
	}{
		{"tcp default", "8080:80", PortMapSpec{HostPort: 8080, GuestPort: 80, Protocol: "tcp"}, false},
		{"explicit tcp", "8080:80/tcp", PortMapSpec{HostPort: 8080, GuestPort: 80, Protocol: "tcp"}, false},
		{"udp", "53:53/udp", PortMapSpec{HostPort: 53, GuestPort: 53, Protocol: "udp"}, false},
		{"uppercase proto", "9000:90/UDP", PortMapSpec{HostPort: 9000, GuestPort: 90, Protocol: "udp"}, false},
		{"max port", "65535:65535", PortMapSpec{HostPort: 65535, GuestPort: 65535, Protocol: "tcp"}, false},
		{"unknown proto", "8080:80/sctp", PortMapSpec{}, true},
		{"missing colon", "8080", PortMapSpec{}, true},
		{"zero host", "0:80", PortMapSpec{}, true},
		{"zero guest", "80:0", PortMapSpec{}, true},
		{"overflow host", "70000:80", PortMapSpec{}, true},
		{"non-numeric host", "abc:80", PortMapSpec{}, true},
		{"non-numeric guest", "80:abc", PortMapSpec{}, true},
		{"empty", "", PortMapSpec{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePortMap(tt.spec)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParsePortMaps(t *testing.T) {
	got, err := ParsePortMaps([]string{"8080:80", "53:53/udp"})
	require.NoError(t, err)
	require.Equal(t, []PortMapSpec{
		{HostPort: 8080, GuestPort: 80, Protocol: "tcp"},
		{HostPort: 53, GuestPort: 53, Protocol: "udp"},
	}, got)
}

func TestParsePortMaps_Empty(t *testing.T) {
	got, err := ParsePortMaps(nil)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestParsePortMaps_PropagatesError(t *testing.T) {
	_, err := ParsePortMaps([]string{"8080:80", "bad"})
	require.Error(t, err)
}
