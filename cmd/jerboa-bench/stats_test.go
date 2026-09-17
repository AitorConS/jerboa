package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSummarize(t *testing.T) {
	s := summarize([]float64{5, 1, 4, 2, 3}, 1)
	require.Equal(t, 5, s.N)
	require.InDelta(t, 3, s.Mean, 1e-9)
	require.InDelta(t, 3, s.Median, 1e-9)
	require.InDelta(t, 1, s.Min, 1e-9)
	require.InDelta(t, 5, s.Max, 1e-9)
	require.InDelta(t, 4.6, s.P90, 1e-9)
	require.InDelta(t, 1.5811, s.Stddev, 1e-3)
	require.LessOrEqual(t, s.MedianCI95[0], s.Median)
	require.GreaterOrEqual(t, s.MedianCI95[1], s.Median)
	require.Equal(t, s, summarize([]float64{5, 1, 4, 2, 3}, 1), "seeded bootstrap is reproducible")
}

func TestSummarizeEdgeCases(t *testing.T) {
	require.Equal(t, summary{}, summarize(nil, 1))
	one := summarize([]float64{7}, 1)
	require.Equal(t, 7.0, one.Median)
	require.Equal(t, [2]float64{7, 7}, one.MedianCI95)
}
