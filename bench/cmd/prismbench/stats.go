// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"math/rand"
	"sort"
)

// Stats is the rigorous summary of one measured distribution (per module-07
// methodology: report the distribution, not just the mean; lead with tails).
type Stats struct {
	Scenario string  `json:"scenario"`
	Unit     string  `json:"unit"`
	N        int     `json:"n_trials"`
	Mean     float64 `json:"mean"`
	Median   float64 `json:"p50"`
	P90      float64 `json:"p90"`
	P99      float64 `json:"p99"`
	P999     float64 `json:"p99_9"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Stddev   float64 `json:"stddev"`
	CoV      float64 `json:"cov"`         // coefficient of variation = stddev/mean
	CILow    float64 `json:"ci95_low"`    // bootstrap 95% CI on the median
	CIHigh   float64 `json:"ci95_high"`
}

// percentile returns the p-th percentile (0..100) of an already-sorted slice
// using linear interpolation between closest ranks.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := (p / 100) * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// summarize computes the full Stats for a sample set. samples is copied+sorted.
func summarize(scenario, unit string, samples []float64) Stats {
	n := len(samples)
	s := make([]float64, n)
	copy(s, samples)
	sort.Float64s(s)

	var sum float64
	for _, v := range s {
		sum += v
	}
	mean := sum / float64(n)

	var ss float64
	for _, v := range s {
		d := v - mean
		ss += d * d
	}
	// sample stddev (n-1)
	std := 0.0
	if n > 1 {
		std = math.Sqrt(ss / float64(n-1))
	}
	cov := 0.0
	if mean != 0 {
		cov = std / mean
	}
	lo, hi := bootstrapMedianCI(s, 2000, 0.95)
	return Stats{
		Scenario: scenario, Unit: unit, N: n,
		Mean: mean, Median: percentile(s, 50), P90: percentile(s, 90),
		P99: percentile(s, 99), P999: percentile(s, 99.9),
		Min: s[0], Max: s[n-1], Stddev: std, CoV: cov,
		CILow: lo, CIHigh: hi,
	}
}

// bootstrapMedianCI returns a percentile-bootstrap confidence interval on the
// median (heavy-tail-safe; module 07's recommended tool). Deterministic seed so
// the report is reproducible.
func bootstrapMedianCI(sorted []float64, resamples int, conf float64) (lo, hi float64) {
	n := len(sorted)
	if n < 3 {
		return sorted[0], sorted[n-1]
	}
	rng := rand.New(rand.NewSource(0x123456789ABCDEF))
	meds := make([]float64, resamples)
	buf := make([]float64, n)
	for r := 0; r < resamples; r++ {
		for i := 0; i < n; i++ {
			buf[i] = sorted[rng.Intn(n)]
		}
		sort.Float64s(buf)
		meds[r] = percentile(buf, 50)
	}
	sort.Float64s(meds)
	alpha := (1 - conf) / 2
	return percentile(meds, alpha*100), percentile(meds, (1-alpha)*100)
}
