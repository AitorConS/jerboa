package apiserver

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSig(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    syscall.Signal
		wantErr bool
	}{
		{"SIGTERM", "SIGTERM", syscall.SIGTERM, false},
		{"SIGINT", "SIGINT", syscall.SIGINT, false},
		{"SIGKILL", "SIGKILL", syscall.SIGKILL, false},
		{"SIGHUP", "SIGHUP", syscall.SIGHUP, false},
		{"SIGQUIT", "SIGQUIT", syscall.SIGQUIT, false},
		{"SIGUSR1", "SIGUSR1", syscall.Signal(10), false},
		{"SIGUSR2", "SIGUSR2", syscall.Signal(12), false},
		{"numeric 15", "15", syscall.Signal(15), false},
		{"numeric 9", "9", syscall.Signal(9), false},
		{"numeric 0", "0", syscall.Signal(0), false},
		{"unknown name", "SIGBADMAGIC", 0, true},
		{"empty string", "", 0, true},
		{"non-numeric string", "abc", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSig(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
