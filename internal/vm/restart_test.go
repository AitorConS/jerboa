//go:build linux

package vm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRestartPolicyConstants(t *testing.T) {
	require.Equal(t, RestartNever, RestartPolicy("never"))
	require.Equal(t, RestartOnFailure, RestartPolicy("on-failure"))
	require.Equal(t, RestartAlways, RestartPolicy("always"))
}

func TestRestartConfig_Defaults(t *testing.T) {
	cfg := RestartConfig{}
	require.Equal(t, RestartPolicy(""), cfg.Policy)
	require.Equal(t, 0, cfg.MaxRetries)
}

func TestVM_SetExplicitStop(t *testing.T) {
	v := &VM{done: make(chan struct{})}
	require.False(t, v.IsExplicitStop())
	v.SetExplicitStop()
	require.True(t, v.IsExplicitStop())
}

func TestVM_GetRestartCount(t *testing.T) {
	v := &VM{done: make(chan struct{})}
	require.Equal(t, 0, v.GetRestartCount())
	v.mu.Lock()
	v.RestartCount = 3
	v.mu.Unlock()
	require.Equal(t, 3, v.GetRestartCount())
}

func TestRestartPolicy_InConfig(t *testing.T) {
	cfg := Config{
		ImagePath: "/tmp/test.img",
		Memory:    "256M",
		Restart: RestartConfig{
			Policy:     RestartOnFailure,
			MaxRetries: 5,
		},
	}
	v, err := NewMemoryStore().Create(cfg)
	require.NoError(t, err)
	require.Equal(t, RestartOnFailure, v.Cfg.Restart.Policy)
	require.Equal(t, 5, v.Cfg.Restart.MaxRetries)
}

func TestRestartVM_BackoffCalc(t *testing.T) {
	cases := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},
		{10, 30 * time.Second},
	}
	for _, tc := range cases {
		backoff := time.Duration(1<<uint(tc.attempt)) * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		require.Equal(t, tc.expected, backoff, "attempt %d", tc.attempt)
	}
}
