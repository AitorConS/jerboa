package main

import (
	"math"
	"math/rand"
	"sort"
)

// summary describes one metric over the measured (non-warmup) runs.
type summary struct {
	N      int     `json:"n"`
	Mean   float64 `json:"mean"`
	Stddev float64 `json:"stddev"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	P99    float64 `json:"p99"`
	Max    float64 `json:"max"`
	// MedianCI95 is a percentile-bootstrap 95% confidence interval of the
	// median, so two runtimes can be compared without eyeballing noise.
	MedianCI95 [2]float64 `json:"median_ci95"`
}

// bootstrapResamples trades CI precision for report time.
const bootstrapResamples = 2000

// summarize computes a summary; seed makes the bootstrap reproducible.
func summarize(values []float64, seed int64) summary {
	if len(values) == 0 {
		return summary{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var sum, sq float64
	for _, v := range sorted {
		sum += v
	}
	mean := sum / float64(len(sorted))
	for _, v := range sorted {
		sq += (v - mean) * (v - mean)
	}
	s := summary{
		N:      len(sorted),
		Mean:   mean,
		Min:    sorted[0],
		Median: percentile(sorted, 50),
		P90:    percentile(sorted, 90),
		P99:    percentile(sorted, 99),
		Max:    sorted[len(sorted)-1],
	}
	if len(sorted) > 1 {
		s.Stddev = math.Sqrt(sq / float64(len(sorted)-1))
	}
	s.MedianCI95 = bootstrapMedianCI(sorted, seed)
	return s
}

// percentile returns the p-th percentile of sorted values using linear
// interpolation between closest ranks.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	frac := rank - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

func bootstrapMedianCI(values []float64, seed int64) [2]float64 {
	if len(values) < 2 {
		return [2]float64{values[0], values[0]}
	}
	// #nosec G404 -- resampling must be reproducible from the report's seed.
	r := rand.New(rand.NewSource(seed))
	medians := make([]float64, bootstrapResamples)
	sample := make([]float64, len(values))
	for i := range medians {
		for j := range sample {
			sample[j] = values[r.Intn(len(values))]
		}
		sort.Float64s(sample)
		medians[i] = percentile(sample, 50)
	}
	sort.Float64s(medians)
	return [2]float64{percentile(medians, 2.5), percentile(medians, 97.5)}
}
