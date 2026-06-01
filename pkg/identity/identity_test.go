// SPDX-License-Identifier: Apache-2.0

package identity

import "testing"

func TestCanonicalStableAcrossVolatileLabels(t *testing.T) {
	a := map[string]string{"app": "api", "tier": "frontend", "pod-template-hash": "abc123"}
	b := map[string]string{"tier": "frontend", "app": "api", "pod-template-hash": "zzz999"}
	if Canonical(a) != Canonical(b) {
		t.Fatalf("canonical differs across volatile labels/order: %q vs %q", Canonical(a), Canonical(b))
	}
	if got := Canonical(a); got != "app=api;tier=frontend" {
		t.Fatalf("unexpected canonical: %q", got)
	}
}

// TestWorkloadCanonicalSpoofResistance verifies the spoof-resistant canonical
// form: identical labels collapse to one identity only when namespace AND
// service account also match (replicas of one Deployment), while a different
// namespace or a different service account yields a DIFFERENT canonical form
// (so a workload cannot inherit another's identity just by copying its labels).
func TestWorkloadCanonicalSpoofResistance(t *testing.T) {
	labels := map[string]string{"app": "api", "tier": "frontend"}

	base := WorkloadCanonical(labels, "team-a", "api-sa")

	// Same labels + same ns + same SA (e.g. two replicas of one Deployment):
	// MUST be identical so dedup still folds them to one identity.
	if same := WorkloadCanonical(map[string]string{"tier": "frontend", "app": "api"}, "team-a", "api-sa"); same != base {
		t.Fatalf("replicas (same ns+sa+labels) must share canonical: %q vs %q", same, base)
	}

	// Different NAMESPACE, same labels + SA: MUST differ (cross-namespace spoof
	// is prevented because namespace is API-assigned, not workload-controlled).
	if other := WorkloadCanonical(labels, "team-b", "api-sa"); other == base {
		t.Fatalf("different namespace must change canonical, got identical: %q", other)
	}

	// Different SERVICE ACCOUNT, same labels + ns: MUST differ (SA is RBAC-bound).
	if other := WorkloadCanonical(labels, "team-a", "other-sa"); other == base {
		t.Fatalf("different service account must change canonical, got identical: %q", other)
	}

	// The authoritative prefix must be present and ordered ns;sa;labels, and the
	// trailing label portion must equal the unchanged Canonical(labels).
	want := "io.prism/namespace=team-a;io.prism/serviceaccount=api-sa;" + Canonical(labels)
	if base != want {
		t.Fatalf("canonical shape: got %q want %q", base, want)
	}

	// Empty ns/sa still emit the authoritative prefix (so a label-only spoof
	// can't collide with a real namespaced workload's bare-label canonical).
	if empty := WorkloadCanonical(labels, "", ""); empty == Canonical(labels) {
		t.Fatalf("empty ns/sa must still carry the authoritative prefix, got %q", empty)
	}
}

func TestAllocatorDeterministicAndDense(t *testing.T) {
	al := NewAllocator()
	id1, created, err := al.Allocate("app=api")
	if err != nil || !created || id1 != MinDynamicID {
		t.Fatalf("first alloc: id=%v created=%v err=%v (want id=%d created=true)", id1, created, err, MinDynamicID)
	}
	// same label set -> same id, not created
	id1b, created, _ := al.Allocate("app=api")
	if id1b != id1 || created {
		t.Fatalf("determinism broken: %v created=%v", id1b, created)
	}
	id2, _, _ := al.Allocate("app=db")
	if id2 != MinDynamicID+1 {
		t.Fatalf("second id = %v, want %d", id2, MinDynamicID+1)
	}
	// release app=api twice (refcount 2) then it frees; smallest-free reused
	al.Release("app=api") // refcount 2->1
	if _, ok := al.Lookup("app=api"); !ok {
		t.Fatal("released too early")
	}
	al.Release("app=api") // 1->0, frees MinDynamicID
	id3, created, _ := al.Allocate("app=cache")
	if id3 != MinDynamicID || !created {
		t.Fatalf("expected reuse of freed %d, got %v created=%v", MinDynamicID, id3, created)
	}
}

func TestReservedAndValidity(t *testing.T) {
	if !IDHost.IsReserved() || IDHost.IsValid() != true {
		t.Fatal("host reserved/valid")
	}
	if MinDynamicID.IsReserved() {
		t.Fatal("MinDynamicID must not be reserved")
	}
	if (MaxID + 1).IsValid() {
		t.Fatal("MaxID+1 must be invalid")
	}
	if IDHost.String() != "1(host)" {
		t.Fatalf("string: %q", IDHost.String())
	}
}

func TestLabelHashStable(t *testing.T) {
	if LabelHash("app=api;tier=frontend") != LabelHash("app=api;tier=frontend") {
		t.Fatal("hash unstable")
	}
	if LabelHash("a") == LabelHash("b") {
		t.Fatal("hash collision on trivial input")
	}
}

// TestReserveNeverMinted verifies a Reserved id is never handed out by mint, that
// the smaller ids below it stay available (smallest-free density preserved), and
// that minting skips straight past the reservation.
func TestReserveNeverMinted(t *testing.T) {
	al := NewAllocator()
	const reserved = MinDynamicID + 5
	al.Reserve(reserved)

	// The first fresh allocations fill MinDynamicID..reserved-1 (the gap below the
	// reservation), then jump over the reserved id.
	seen := make(map[NumericIdentity]bool)
	for i := 0; i < 7; i++ {
		id, _, err := al.Allocate(string(rune('a' + i)))
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if id == reserved {
			t.Fatalf("mint handed out reserved id %d", reserved)
		}
		seen[id] = true
	}
	// The five ids below the reservation must have been used (dense), and the id
	// just past the reservation too.
	for id := MinDynamicID; id < reserved; id++ {
		if !seen[id] {
			t.Fatalf("smallest-free density broken: %d not minted", id)
		}
	}
	if !seen[reserved+1] {
		t.Fatalf("mint did not continue past reservation: %d not minted", reserved+1)
	}
}

// TestAdoptBindsAndRefcounts verifies Adopt binds canon->id with refcount 1, is
// reusable after a full Release, and refuses to rebind an already-mapped canon.
func TestAdoptBindsAndRefcounts(t *testing.T) {
	al := NewAllocator()
	const id = MinDynamicID + 100
	al.Reserve(id)

	if !al.Adopt("app=web", id) {
		t.Fatal("Adopt of fresh canon returned false")
	}
	got, ok := al.Lookup("app=web")
	if !ok || got != id {
		t.Fatalf("Lookup after Adopt = %v,%v want %d,true", got, ok, id)
	}
	// Adopting the same canon again must fail (already mapped).
	if al.Adopt("app=web", id+1) {
		t.Fatal("Adopt of already-mapped canon returned true")
	}
	// Allocate of the adopted canon returns the same id (created=false), bumping
	// the refcount to 2 — so it survives one Release.
	gid, created, _ := al.Allocate("app=web")
	if gid != id || created {
		t.Fatalf("Allocate of adopted canon = %v,created=%v want %d,false", gid, created, id)
	}
	al.Release("app=web") // 2->1
	if _, ok := al.Lookup("app=web"); !ok {
		t.Fatal("released too early")
	}
	al.Release("app=web") // 1->0 frees id
	if _, ok := al.Lookup("app=web"); ok {
		t.Fatal("not released after last ref")
	}
	// After release the id is back in the reuse pool. Reserving id pushed the gap
	// MinDynamicID..id-1 into the freed pool too, so smallest-free hands those out
	// first; the formerly-adopted id is reused once the gap drains. Verify it is
	// reusable (no longer reserved, no longer mapped) by draining up to it.
	reused := false
	for i := 0; i < 200; i++ {
		gotID, _, err := al.Allocate(string(rune('A')) + string(rune('0'+i%10)) + string(rune('a'+i/10)))
		if err != nil {
			t.Fatalf("drain allocate %d: %v", i, err)
		}
		if gotID == id {
			reused = true
			break
		}
	}
	if !reused {
		t.Fatalf("formerly-adopted id %d was never reusable after release", id)
	}
}
