package apiserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBakedPortsPublishOnLocalhost(t *testing.T) {
	specs, err := bakedPortMaps([]string{"8080:80", "0.0.0.0:8443:443", "192.168.1.10:53:53/udp"})
	require.NoError(t, err)
	require.Len(t, specs, 3)
	for _, s := range specs {
		require.Equal(t, "127.0.0.1", s.BindAddr, "an image cannot choose where its ports publish")
	}
	require.Equal(t, "udp", specs[2].Protocol)

	_, err = bakedPortMaps([]string{"not-a-port"})
	require.Error(t, err)
}
