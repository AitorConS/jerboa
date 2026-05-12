package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewRootCmd_Help(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--help"})
	err := cmd.Execute()
	require.NoError(t, err)
}

func TestNewRootCmd_HasGCCmd(t *testing.T) {
	cmd := newRootCmd()
	gcCmd, _, err := cmd.Find([]string{"gc"})
	require.NoError(t, err)
	require.Equal(t, "gc", gcCmd.Use)
}

func TestNewRootCmd_Flags(t *testing.T) {
	cmd := newRootCmd()
	flags := cmd.Flags()
	_, err := flags.GetString("addr")
	require.NoError(t, err)
	_, err = flags.GetString("store")
	require.NoError(t, err)
	_, err = flags.GetString("token")
	require.NoError(t, err)
	_, err = flags.GetString("jwt-secret")
	require.NoError(t, err)
	_, err = flags.GetString("jwt-issuer")
	require.NoError(t, err)
	_, err = flags.GetString("jwt-audience")
	require.NoError(t, err)
	_, err = flags.GetString("tls-cert")
	require.NoError(t, err)
	_, err = flags.GetString("tls-key")
	require.NoError(t, err)
	_, err = flags.GetBool("no-auto-tls")
	require.NoError(t, err)
}

func TestValidateTLSConfig(t *testing.T) {
	tests := []struct {
		name     string
		cert     string
		key      string
		wantErr bool
	}{
		{"both empty", "", "", false},
		{"both set", "cert.pem", "key.pem", false},
		{"cert only", "cert.pem", "", true},
		{"key only", "", "key.pem", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTLSConfig(tt.cert, tt.key)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDefaultStorePath(t *testing.T) {
	path := defaultStorePath()
	require.Contains(t, path, ".uni")
}