package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "")
	require.Empty(t, sourceDateEpoch())
	t.Setenv("SOURCE_DATE_EPOCH", "1767225600")
	require.Equal(t, "2026-01-01T00:00:00Z", sourceDateEpoch())
	t.Setenv("SOURCE_DATE_EPOCH", "yesterday")
	require.Empty(t, sourceDateEpoch())
	t.Setenv("SOURCE_DATE_EPOCH", "-5")
	require.Empty(t, sourceDateEpoch())
}
