// SPDX-License-Identifier: Apache-2.0

// Package abi defines the frozen Prism Identity Bus ABI contract shared by the
// userspace daemon (Go), every BPF consumer (C), and the on-disk pinned map.
//
// This file is the single source of truth for the Go side. Its C twin is
// bpf/prism_maps.bpf.h — the struct field order, widths and constants MUST stay
// byte-identical (little-endian) so a value written by prismd is read correctly
// by any BPF program (sched_ext, tc, observer).
package abi

import "fmt"

// Map identity (pinned location + name) — see bpf/prism_maps.bpf.h.
const (
	MapName = "prism_identity"
	// PinPath is the bpffs pin location. With LIBBPF_PIN_BY_NAME the map pins as
	// <dir>/<MapName>, so the basename equals MapName — keep them in lockstep so
	// the daemon and any BPF consumer resolve the exact same path.
	PinPath    = "/sys/fs/bpf/prism/prism_identity"
	MaxEntries = 1 << 20 // 1,048,576 workloads
)

// PrismIdentity is the per-workload value stored in the prism_identity map.
//
// Binary layout (x86-64 / little-endian), total 24 bytes, must match
// `struct prism_identity` in C exactly:
//
//	offset 0  u32 Identity   (24-bit numeric identity in the low bits)
//	offset 4  u32 Flags      (subsystem facet bits, see Flag*)
//	offset 8  u64 LabelHash  (FNV-1a/64 of the canonical label set)
//	offset 16 u64 UpdatedNs  (wall-clock nanoseconds of last write)
type PrismIdentity struct {
	Identity  uint32
	Flags     uint32
	LabelHash uint64
	UpdatedNs uint64
}

// Subsystem facet flags. One identity, multiple consumers — each BPF subsystem
// sets/reads its own bit, which is the 3-way composability claim made concrete.
const (
	FlagNetPolicy    uint32 = 1 << 0 // network policy engine has a rule for this identity
	FlagSchedManaged uint32 = 1 << 1 // sched_ext is actively managing this identity
	FlagObserved     uint32 = 1 << 2 // an observer (Tetragon-style) is tracking it
)

// ---- Scheduling-policy sub-fields packed into Flags (the "thick" bus) --------
//
// The bus carries identity AND a per-identity scheduling-policy class, written
// by prismd from a pod label and consumed NATIVELY by any scx scheduler (mapped
// to that scheduler's own knobs). So the operator sets scheduling policy
// centrally and every consumer agrees on it — "thick in meaning, thin in bytes".
//
// This does NOT grow the value: it stays 24 B, byte-identical. The policy is
// encoded in previously-reserved bits of the existing Flags u32, DISJOINT from
// the facet bits [0:2] above (which are frozen and untouched):
//
//	bits [0:2]   facet flags (Flag* above)            — frozen
//	bits [3:7]   reserved (MUST be 0)
//	bits [8:10]  SchedClass   (3 bits, see SchedClass*)
//	bits [11:17] SchedWeight  (7 bits: 0 = unset, else 1..127)
//	bits [18:31] reserved (MUST be 0)
//
// Keep these byte-identical with the PRISM_SCHED_* macros in
// bpf/prism_maps.bpf.h. Escape hatch if the descriptor ever exceeds the spare
// bits: a side map keyed by identity, leaving this 24 B value frozen.
const (
	SchedClassShift         = 8
	SchedClassMask   uint32 = 0x7 << SchedClassShift // bits 8..10
	SchedWeightShift        = 11
	SchedWeightMask  uint32 = 0x7F << SchedWeightShift // bits 11..17
	// SchedWeightMax is the largest weight the 7-bit field holds.
	SchedWeightMax = 0x7F // 127
	// SchedWeightNeutral is the weight a consumer treats as "1x" (no scaling);
	// operators raise it above neutral to amplify a class's effect. It is a
	// consumer-side convention, documented here so producer and consumer agree.
	SchedWeightNeutral = 64
)

// SchedClass is the per-identity latency class carried on the bus. It says HOW a
// scheduler should treat the workload, decoupled from WHICH workload it is (the
// numeric identity). Unset means "no policy — use the scheduler's own
// heuristic", so an identity with no class behaves exactly as before this field
// existed: the thick bus is strictly opt-in and backward compatible.
type SchedClass uint32

const (
	SchedClassUnset    SchedClass = 0 // no policy; scheduler uses its own heuristic
	SchedClassCritical SchedClass = 1 // latency-critical: maximum boost
	SchedClassNormal   SchedClass = 2 // explicitly normal: heuristic, no boost
	SchedClassBatch    SchedClass = 3 // batch: deprioritize (heuristic-gaming-proof)
	// 4..7 reserved for future classes.
)

var schedClassNames = map[SchedClass]string{
	SchedClassUnset: "unset", SchedClassCritical: "critical",
	SchedClassNormal: "normal", SchedClassBatch: "batch",
}

func (c SchedClass) String() string {
	if n, ok := schedClassNames[c]; ok {
		return n
	}
	return fmt.Sprintf("class(%d)", uint32(c))
}

// IsValid reports whether c fits the 3-bit class field.
func (c SchedClass) IsValid() bool {
	return c <= SchedClass(SchedClassMask>>SchedClassShift)
}

// EncodeSchedPolicy packs a latency class and weight into the Flags sub-fields
// they own, with every other bit (the facet bits especially) zero — so the
// result is safe to OR into a Flags value that already carries facet bits.
// weight is clamped to [0, SchedWeightMax]; the class is masked to 3 bits.
func EncodeSchedPolicy(class SchedClass, weight uint32) uint32 {
	if weight > SchedWeightMax {
		weight = SchedWeightMax
	}
	return ((uint32(class) << SchedClassShift) & SchedClassMask) |
		((weight << SchedWeightShift) & SchedWeightMask)
}

// SchedClass extracts the latency class from a value's Flags.
func (v PrismIdentity) SchedClass() SchedClass {
	return SchedClass((v.Flags & SchedClassMask) >> SchedClassShift)
}

// SchedWeight extracts the scheduling weight (0 = unset) from a value's Flags.
func (v PrismIdentity) SchedWeight() uint32 {
	return (v.Flags & SchedWeightMask) >> SchedWeightShift
}

// Workload key — what identifies a workload in the map.
//
// On a real cgroup-v2 kernel this is the cgroup id (bpf_get_current_cgroup_id).
// In simulation/bench it is a synthetic stable u64 per pod. The width is fixed
// by the ABI regardless of source.
type WorkloadKey = uint64

// Sink is the abstraction the daemon writes identities through. Implemented by
// the real BPF map sink (cilium/ebpf) and the userspace simulation sink.
type Sink interface {
	Upsert(key WorkloadKey, id PrismIdentity) error
	Delete(key WorkloadKey) error
	Lookup(key WorkloadKey) (PrismIdentity, bool, error)
	Len() int
	Kind() string // "bpf" | "sim"
	Close() error
	// Range iterates every LIVE entry, calling fn for each (key, value). It stops
	// early — without visiting the rest — if fn returns false. Iteration order is
	// unspecified. The pinned BPF map outlives the daemon, so Range over a BPF
	// sink at startup is how prismd rediscovers the identities a prior incarnation
	// wrote (restart-stability reseed) and reconciles leaked entries (GC).
	Range(fn func(key WorkloadKey, id PrismIdentity) bool)
}
