package apiserver

import (
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

func TestDaemonVersionReportsTheHypervisor(t *testing.T) {
	s := &Server{version: "1.2.3"}
	got, rpcErr := s.handleDaemonVersion()
	require.Nil(t, rpcErr)
	require.Equal(t, map[string]string{"version": "1.2.3"}, got)

	s.SetHypervisor(api.DaemonHypervisor{Name: "qemu", Reason: "chosen in config.toml", FirecrackerAvailable: true})
	got, rpcErr = s.handleDaemonVersion()
	require.Nil(t, rpcErr)
	require.Equal(t, map[string]string{
		"version": "1.2.3", "hypervisor": "qemu", "hypervisor_reason": "chosen in config.toml", "firecracker_available": "true",
	}, got)
}
