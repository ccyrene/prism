// SPDX-License-Identifier: Apache-2.0

package abi

import (
	"testing"
	"unsafe"
)

// TestValueSizeFrozen pins the ABI: the value is 24 bytes and the thick-bus
// policy fields must NOT have grown it (they live in spare bits of Flags).
func TestValueSizeFrozen(t *testing.T) {
	if got := unsafe.Sizeof(PrismIdentity{}); got != 24 {
		t.Fatalf("PrismIdentity size = %d bytes, want 24 (frozen ABI)", got)
	}
}

// TestSchedFieldBitsDisjoint checks the policy sub-fields occupy exactly the
// documented bits and never overlap the frozen facet bits [0:2] or each other.
// These literal values are the contract the C twin (PRISM_SCHED_*_MASK in
// bpf/prism_maps.bpf.h) must match byte-for-byte.
func TestSchedFieldBitsDisjoint(t *testing.T) {
	if SchedClassShift != 8 || SchedWeightShift != 11 {
		t.Fatalf("shifts drifted: class=%d weight=%d, want 8/11", SchedClassShift, SchedWeightShift)
	}
	if SchedClassMask != 0x700 {
		t.Errorf("SchedClassMask = %#x, want 0x700 (bits 8..10)", SchedClassMask)
	}
	if SchedWeightMask != 0x3F800 {
		t.Errorf("SchedWeightMask = %#x, want 0x3F800 (bits 11..17)", SchedWeightMask)
	}
	facets := FlagNetPolicy | FlagSchedManaged | FlagObserved // 0x7
	if SchedClassMask&facets != 0 {
		t.Errorf("class field overlaps facet bits: %#x", SchedClassMask&facets)
	}
	if SchedWeightMask&facets != 0 {
		t.Errorf("weight field overlaps facet bits: %#x", SchedWeightMask&facets)
	}
	if SchedClassMask&SchedWeightMask != 0 {
		t.Errorf("class and weight fields overlap: %#x", SchedClassMask&SchedWeightMask)
	}
}

// TestEncodeDecodeRoundTrip exercises every class and a range of weights through
// the encoder and the value-side accessors.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	classes := []SchedClass{SchedClassUnset, SchedClassCritical, SchedClassNormal, SchedClassBatch}
	weights := []uint32{0, 1, 64, 127}
	for _, c := range classes {
		for _, w := range weights {
			v := PrismIdentity{Flags: EncodeSchedPolicy(c, w)}
			if got := v.SchedClass(); got != c {
				t.Errorf("class round-trip: encode(%v,%d) -> %v", c, w, got)
			}
			if got := v.SchedWeight(); got != w {
				t.Errorf("weight round-trip: encode(%v,%d) -> %d", c, w, got)
			}
		}
	}
}

// TestEncodePreservesFacets is the composability invariant: OR-ing policy bits
// into a Flags word that already carries facet bits must leave BOTH readable.
func TestEncodePreservesFacets(t *testing.T) {
	facets := FlagSchedManaged | FlagObserved
	v := PrismIdentity{Flags: facets | EncodeSchedPolicy(SchedClassCritical, 100)}

	if v.Flags&FlagSchedManaged == 0 || v.Flags&FlagObserved == 0 {
		t.Errorf("facet bits lost after OR-ing policy: flags=%#x", v.Flags)
	}
	if v.Flags&FlagNetPolicy != 0 {
		t.Errorf("net facet spuriously set: flags=%#x", v.Flags)
	}
	if v.SchedClass() != SchedClassCritical {
		t.Errorf("class = %v, want critical", v.SchedClass())
	}
	if v.SchedWeight() != 100 {
		t.Errorf("weight = %d, want 100", v.SchedWeight())
	}
}

// TestEncodeClamps verifies out-of-range inputs are masked/clamped, never
// bleeding into neighboring fields.
func TestEncodeClamps(t *testing.T) {
	// Weight above the 7-bit max clamps to 127.
	if v := (PrismIdentity{Flags: EncodeSchedPolicy(SchedClassNormal, 999)}); v.SchedWeight() != SchedWeightMax {
		t.Errorf("weight 999 -> %d, want %d (clamped)", v.SchedWeight(), SchedWeightMax)
	}
	// A class beyond 3 bits is masked to its low 3 bits and must not corrupt the
	// weight field or any facet bit.
	got := EncodeSchedPolicy(SchedClass(0xFF), 0)
	if got&^SchedClassMask != 0 {
		t.Errorf("oversized class leaked outside class field: %#x", got)
	}
}

func TestSchedClassValidityAndString(t *testing.T) {
	for _, c := range []SchedClass{SchedClassUnset, SchedClassCritical, SchedClassNormal, SchedClassBatch} {
		if !c.IsValid() {
			t.Errorf("%v reported invalid", c)
		}
		if c.String() == "" {
			t.Errorf("class %d has empty String()", uint32(c))
		}
	}
	if SchedClass(8).IsValid() {
		t.Errorf("class 8 should not fit the 3-bit field")
	}
}
