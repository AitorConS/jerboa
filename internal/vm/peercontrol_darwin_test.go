package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripPeerControl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"machine-config":{"vcpu_count":1},"security":{"version":1,"unix_stream":"/p/net.sock","unix_control":"/p/net.ctl"}}`), 0o600))
	stripped, err := stripPeerControl(path)
	require.NoError(t, err)
	require.True(t, stripped)
	var doc struct {
		Machine  map[string]int `json:"machine-config"`
		Security map[string]any `json:"security"`
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &doc))
	require.Equal(t, 1, doc.Machine["vcpu_count"])
	require.Equal(t, "/p/net.sock", doc.Security["unix_stream"])
	require.NotContains(t, doc.Security, "unix_control")

	// Nothing to strip: the file is left alone and the caller sees false.
	stripped, err = stripPeerControl(path)
	require.NoError(t, err)
	require.False(t, stripped)
}
