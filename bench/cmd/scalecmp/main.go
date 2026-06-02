// SPDX-License-Identifier: Apache-2.0

// Command scalecmp measures lookup latency of the three userspace sink
// implementations as the live population grows toward millions, to quantify
// scale CONSISTENCY (how flat the latency curve stays):
//
//	SimSink     map[u64] + RWMutex          (reference)
//	FastSink    pointer-per-bucket, lock-free (1 indirection -> 2 cache misses at scale)
//	CompactSink inline slot, seqlock lock-free (1 cache miss at scale)
//
//	go run ./bench/cmd/scalecmp
package main

import (
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/ccyrene/prism/pkg/abi"
	"github.com/ccyrene/prism/pkg/sink"
)

var sink_ uint64 // anti-DCE

func k(i int) abi.WorkloadKey { return abi.WorkloadKey(uint64(i)*0x9E3779B97F4A7C15 + 1) }

func timeLookups(s abi.Sink, n, ops int, idx []int) float64 {
	best := 1e18
	for rep := 0; rep < 3; rep++ { // median-ish: take best of 3 (least noise)
		t := time.Now()
		for i := 0; i < ops; i++ {
			v, _, _ := s.Lookup(k(idx[i]))
			sink_ += uint64(v.Identity)
		}
		ns := float64(time.Since(t).Nanoseconds()) / float64(ops)
		if ns < best {
			best = ns
		}
	}
	return best
}

func main() {
	sizes := []int{1000, 10000, 100000, 1000000, 4000000}
	const ops = 500000
	rng := rand.New(rand.NewSource(1))

	build := map[string]func() abi.Sink{
		"SimSink(map+RWMutex)": func() abi.Sink { return sink.NewSimSink() },
		"FastSink(ptr,lockfree)": func() abi.Sink { return sink.NewFastSink() },
		"CompactSink(inline)":  func() abi.Sink { return sink.NewCompactSink() },
	}
	order := []string{"SimSink(map+RWMutex)", "FastSink(ptr,lockfree)", "CompactSink(inline)"}

	fmt.Printf("%-12s %-22s %-22s %-22s\n", "population", order[0], order[1], order[2])
	csv, _ := os.Create("bench/results/scale_sinks.csv")
	fmt.Fprintln(csv, "population,sim_ns,fast_ns,compact_ns")
	for _, n := range sizes {
		idx := make([]int, ops)
		for i := range idx {
			idx[i] = rng.Intn(n)
		}
		row := make(map[string]float64)
		for _, name := range order {
			s := build[name]()
			for i := 0; i < n; i++ {
				s.Upsert(k(i), abi.PrismIdentity{Identity: uint32(i + 256)})
			}
			row[name] = timeLookups(s, n, ops, idx)
			s.Close()
		}
		fmt.Printf("%-12d %-22.1f %-22.1f %-22.1f\n", n, row[order[0]], row[order[1]], row[order[2]])
		fmt.Fprintf(csv, "%d,%.2f,%.2f,%.2f\n", n, row[order[0]], row[order[1]], row[order[2]])
	}
	csv.Close()
	fmt.Printf("\n(anti-dce %d) wrote bench/results/scale_sinks.csv\n", sink_)
}
