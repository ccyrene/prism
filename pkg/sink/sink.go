// SPDX-License-Identifier: Apache-2.0

// Package sink provides the write side of the Prism identity bus: concrete
// abi.Sink implementations and a factory that selects between them.
//
//   - BPFSink writes the real pinned BPF hash map (needs root/CAP_BPF + bpffs).
//   - FastSink is a lock-free-read userspace table that runs anywhere and backs
//     the benchmarks (the optimized default fallback).
//   - SimSink is the simpler map+RWMutex reference kept for comparison/tests.
//
// The daemon writes identities through whichever sink New returns; the ABI it
// writes is identical either way, so a benchmark against the sim sink exercises
// the same control path that production runs against the kernel map.
package sink

import (
	"log"

	"github.com/ccyrene/prism/pkg/abi"
)

// New returns a Sink. When preferBPF is true it first tries the kernel-backed
// BPFSink; if that fails (no root, unprivileged BPF disabled, no bpffs — as on
// this 5.15 WSL2 host) it logs the reason and transparently falls back to the
// SimSink so the daemon stays runnable. When preferBPF is false it goes straight
// to the SimSink.
//
// New never returns a nil Sink with a nil error: the SimSink fallback always
// succeeds, so callers always get a usable bus.
func New(preferBPF bool) (abi.Sink, error) {
	if preferBPF {
		s, err := NewBPFSink()
		if err == nil {
			log.Printf("sink: using bpf sink (pinned map %q at %s)", abi.MapName, abi.PinPath)
			return s, nil
		}
		log.Printf("sink: bpf sink unavailable, falling back to sim sink: %v", err)
	}
	return NewCompactSink(), nil
}
