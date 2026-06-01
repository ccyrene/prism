// SPDX-License-Identifier: Apache-2.0

package prismsync

import (
	"testing"

	"github.com/prism-bus/prism/pkg/abi"
	"github.com/prism-bus/prism/pkg/identity"
	"github.com/prism-bus/prism/pkg/sink"
)

// TestReseedReclaimsIdentity proves restart-stability: a workload that survives a
// daemon restart RECLAIMS the exact numeric identity a prior incarnation wrote
// into the (surviving) sink, and a different new workload is NOT minted that
// reserved number.
func TestReseedReclaimsIdentity(t *testing.T) {
	// Pre-populate a sink as if a prior daemon had written one identity for a
	// workload whose labels canonicalize to `canon`. The map only stores the FNV
	// label hash (not the canonical string), so reclaim is keyed on that hash.
	// The hash is over the spoof-resistant WorkloadCanonical form (labels +
	// namespace + service account), matching what HandlePod re-derives on
	// re-observation. The survivor pod below uses the `pod` helper, which sets
	// Namespace="default" and an empty ServiceAccountName, so we seed with the
	// same ns/SA a prior incarnation would have written.
	const survivor = identity.NumericIdentity(256)
	canon := identity.WorkloadCanonical(map[string]string{"app": "web"}, "default", "")
	survivorKey := abi.WorkloadKey(0xDEADBEEF)

	snk := sink.NewCompactSink()
	if err := snk.Upsert(survivorKey, abi.PrismIdentity{
		Identity:  uint32(survivor),
		LabelHash: identity.LabelHash(canon),
	}); err != nil {
		t.Fatal(err)
	}

	// Fresh controller over that surviving sink, then Reseed (what Run does first).
	c := NewController(nil, snk)
	c.Reseed()

	// The surviving workload is re-observed: HandlePod must reclaim 256, not mint
	// a fresh id. We use the same labels so canon matches.
	survivorPod := pod("uid-web", "web-xyz", map[string]string{"app": "web", "pod-template-hash": "new"})
	if err := c.HandlePod(survivorPod, EventAdd); err != nil {
		t.Fatalf("HandlePod survivor: %v", err)
	}
	v, ok, _ := snk.Lookup(WorkloadKey(survivorPod.UID))
	if !ok {
		t.Fatal("survivor pod missing from sink after re-add")
	}
	if identity.NumericIdentity(v.Identity) != survivor {
		t.Fatalf("survivor did NOT reclaim its identity: got %d want %d", v.Identity, survivor)
	}

	// A different brand-new workload must get an id that is NOT the reserved one.
	otherPod := pod("uid-db", "db-abc", map[string]string{"app": "db"})
	if err := c.HandlePod(otherPod, EventAdd); err != nil {
		t.Fatalf("HandlePod other: %v", err)
	}
	ov, ok, _ := snk.Lookup(WorkloadKey(otherPod.UID))
	if !ok {
		t.Fatal("other pod missing from sink")
	}
	if identity.NumericIdentity(ov.Identity) == survivor {
		t.Fatalf("new workload was minted the reserved survivor id %d", survivor)
	}
}

// TestReconcileEvictsStale proves the map-GC sweep: an entry present in the sink
// but absent from byUID (a delete missed while the daemon was down) is removed by
// Reconcile, while live keys are left untouched.
func TestReconcileEvictsStale(t *testing.T) {
	snk := sink.NewCompactSink()
	c := NewController(nil, snk)

	// A genuinely live pod: HandlePod records it in byUID and writes the sink.
	livePod := pod("uid-live", "live", map[string]string{"app": "live"})
	if err := c.HandlePod(livePod, EventAdd); err != nil {
		t.Fatalf("add live: %v", err)
	}
	liveKey := WorkloadKey(livePod.UID)

	// A stale, orphaned entry: present in the sink but never tracked in byUID
	// (simulating a pod deleted while prismd was down — the delete was missed).
	staleKey := abi.WorkloadKey(0x5741_4C45) // arbitrary, distinct from liveKey
	if staleKey == liveKey {
		t.Fatal("test setup: stale key collides with live key")
	}
	if err := snk.Upsert(staleKey, abi.PrismIdentity{Identity: 999, LabelHash: 0xABCD}); err != nil {
		t.Fatal(err)
	}
	if snk.Len() != 2 {
		t.Fatalf("setup: sink len = %d, want 2", snk.Len())
	}

	if evicted := c.Reconcile(); evicted != 1 {
		t.Fatalf("Reconcile evicted %d, want 1", evicted)
	}

	// The stale key is gone; the live key remains.
	if _, ok, _ := snk.Lookup(staleKey); ok {
		t.Errorf("stale key survived reconcile")
	}
	if _, ok, _ := snk.Lookup(liveKey); !ok {
		t.Errorf("live key wrongly evicted by reconcile")
	}
	if snk.Len() != 1 {
		t.Errorf("after reconcile sink len = %d, want 1", snk.Len())
	}

	// Reconcile is idempotent: a second run with no orphans evicts nothing.
	if evicted := c.Reconcile(); evicted != 0 {
		t.Fatalf("second Reconcile evicted %d, want 0", evicted)
	}
}
