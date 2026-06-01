// SPDX-License-Identifier: Apache-2.0

package prismsync

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/prism-bus/prism/pkg/abi"
	"github.com/prism-bus/prism/pkg/identity"
	"github.com/prism-bus/prism/pkg/sink"
)

// pod is a tiny helper to build a Pod with a UID and label set.
func pod(uid, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(uid),
			Labels:    labels,
		},
	}
}

// TestController_ThreePods drives three fake pods through the controller's
// synchronous path and asserts the sim sink ends up holding exactly the right
// identities: distinct label sets get distinct ids, and an identical label set
// shares one id (Cilium-style identity dedup).
func TestController_ThreePods(t *testing.T) {
	snk := sink.NewSimSink()
	c := NewController(nil, snk) // nil client: we never call Run, only HandlePod.

	// p1 and p2 are two replicas of the SAME workload (same security-relevant
	// labels, differing only in the volatile pod-template-hash). They must share
	// one identity. p3 is a different workload.
	p1 := pod("uid-1", "web-aaa", map[string]string{"app": "web", "pod-template-hash": "aaa"})
	p2 := pod("uid-2", "web-bbb", map[string]string{"app": "web", "pod-template-hash": "bbb"})
	p3 := pod("uid-3", "db-ccc", map[string]string{"app": "db"})

	for _, p := range []*corev1.Pod{p1, p2, p3} {
		if err := c.HandlePod(p, EventAdd); err != nil {
			t.Fatalf("HandlePod(add) %s: %v", p.Name, err)
		}
	}

	// Sink should hold one entry per pod (keyed by UID-derived workload key).
	if got := snk.Len(); got != 3 {
		t.Fatalf("sink len = %d, want 3", got)
	}

	id1 := lookupIdentity(t, snk, p1)
	id2 := lookupIdentity(t, snk, p2)
	id3 := lookupIdentity(t, snk, p3)

	// p1 and p2 share an identity (same canonical labels).
	if id1 != id2 {
		t.Errorf("replicas of same workload got different identities: %d vs %d", id1, id2)
	}
	// p3 is a distinct workload.
	if id3 == id1 {
		t.Errorf("distinct workload reused identity %d", id3)
	}
	// Both ids must be in the dynamic range.
	for _, id := range []identity.NumericIdentity{id1, id3} {
		if id < identity.MinDynamicID {
			t.Errorf("identity %d is below MinDynamicID %d", id, identity.MinDynamicID)
		}
	}

	// The allocator should hold exactly 2 live identities (web + db) even though
	// 3 pods are present — refcounting collapses the two web replicas.
	if got := c.Allocator().Len(); got != 2 {
		t.Errorf("allocator len = %d, want 2", got)
	}

	// Verify the stored value carries the right label hash for p3. The hash is
	// over the spoof-resistant canonical (labels + namespace + service account),
	// which is exactly what the controller derives via WorkloadCanonical.
	wantHash := identity.LabelHash(identity.WorkloadCanonical(p3.Labels, p3.Namespace, p3.Spec.ServiceAccountName))
	v, ok, _ := snk.Lookup(WorkloadKey(p3.UID))
	if !ok {
		t.Fatalf("p3 missing from sink")
	}
	if v.LabelHash != wantHash {
		t.Errorf("p3 label hash = %#x, want %#x", v.LabelHash, wantHash)
	}
}

// TestController_DeleteReleases checks that deleting a pod removes its sink
// entry and releases its identity refcount.
func TestController_DeleteReleases(t *testing.T) {
	snk := sink.NewSimSink()
	c := NewController(nil, snk)

	p1 := pod("uid-1", "web-aaa", map[string]string{"app": "web"})
	p2 := pod("uid-2", "web-bbb", map[string]string{"app": "web"})

	for _, p := range []*corev1.Pod{p1, p2} {
		if err := c.HandlePod(p, EventAdd); err != nil {
			t.Fatalf("add %s: %v", p.Name, err)
		}
	}
	if c.Allocator().Len() != 1 {
		t.Fatalf("after 2 web replicas allocator len = %d, want 1", c.Allocator().Len())
	}

	// Delete one replica: the shared identity must survive (other replica holds
	// a reference) but the deleted pod's sink key must be gone.
	if err := c.HandlePod(p1, EventDelete); err != nil {
		t.Fatalf("delete p1: %v", err)
	}
	if _, ok, _ := snk.Lookup(WorkloadKey(p1.UID)); ok {
		t.Errorf("p1 sink key still present after delete")
	}
	if snk.Len() != 1 {
		t.Errorf("sink len = %d after one delete, want 1", snk.Len())
	}
	if c.Allocator().Len() != 1 {
		t.Errorf("allocator len = %d after one replica deleted, want 1 (other replica holds ref)", c.Allocator().Len())
	}

	// Delete the last replica: identity is now fully released.
	if err := c.HandlePod(p2, EventDelete); err != nil {
		t.Fatalf("delete p2: %v", err)
	}
	if snk.Len() != 0 {
		t.Errorf("sink len = %d after all deletes, want 0", snk.Len())
	}
	if c.Allocator().Len() != 0 {
		t.Errorf("allocator len = %d after all deletes, want 0", c.Allocator().Len())
	}
}

// TestController_UpdateRelabels verifies that changing a pod's
// security-relevant labels re-allocates its identity and updates the sink value.
func TestController_UpdateRelabels(t *testing.T) {
	snk := sink.NewSimSink()
	c := NewController(nil, snk)

	// A pinning pod keeps the "web" identity referenced so it is NOT freed when
	// our subject pod relabels away from it. Without this, the smallest-free
	// allocator would legitimately hand the freed id straight back, masking the
	// re-allocation; pinning it forces a genuinely new id and makes the
	// "identity changed" assertion meaningful.
	pin := pod("uid-pin", "web-pinner", map[string]string{"app": "web"})
	if err := c.HandlePod(pin, EventAdd); err != nil {
		t.Fatalf("add pinner: %v", err)
	}

	p := pod("uid-1", "svc", map[string]string{"app": "web"})
	if err := c.HandlePod(p, EventAdd); err != nil {
		t.Fatalf("add: %v", err)
	}
	idBefore := lookupIdentity(t, snk, p)

	// Relabel to a different workload identity.
	p.Labels = map[string]string{"app": "api"}
	if err := c.HandlePod(p, EventUpdate); err != nil {
		t.Fatalf("update: %v", err)
	}
	idAfter := lookupIdentity(t, snk, p)

	if idAfter == idBefore {
		t.Errorf("identity unchanged after relabel: %d", idAfter)
	}
	// Two live identities now: "web" (still held by the pinner) and "api".
	if c.Allocator().Len() != 2 {
		t.Errorf("allocator len = %d after relabel, want 2", c.Allocator().Len())
	}
	wantHash := identity.LabelHash(identity.WorkloadCanonical(p.Labels, p.Namespace, p.Spec.ServiceAccountName))
	v, _, _ := snk.Lookup(WorkloadKey(p.UID))
	if v.LabelHash != wantHash {
		t.Errorf("label hash not updated: got %#x want %#x", v.LabelHash, wantHash)
	}
}

// TestController_SchedPolicyPropagates verifies the thick bus end-to-end: a pod
// carrying prism.io/ policy labels lands in the sink with the right
// scheduling-policy sub-fields, the daemon's facet flags are preserved, and the
// policy does NOT perturb identity (a differently-classed replica shares the id
// but carries its own class — "who you are" vs "how you're treated" decoupled).
func TestController_SchedPolicyPropagates(t *testing.T) {
	snk := sink.NewSimSink()
	c := NewController(nil, snk)
	c.Flags = abi.FlagSchedManaged // daemon stamps its facet on every write

	crit := pod("uid-crit", "web-crit", map[string]string{
		"app": "web", "prism.io/latency-class": "critical", "prism.io/weight": "100",
	})
	// Same workload labels (app=web), but classed batch instead of critical.
	batch := pod("uid-batch", "web-batch", map[string]string{
		"app": "web", "prism.io/latency-class": "batch",
	})
	for _, p := range []*corev1.Pod{crit, batch} {
		if err := c.HandlePod(p, EventAdd); err != nil {
			t.Fatalf("add %s: %v", p.Name, err)
		}
	}

	vc, ok, _ := snk.Lookup(WorkloadKey(crit.UID))
	if !ok {
		t.Fatal("critical pod missing from sink")
	}
	if vc.SchedClass() != abi.SchedClassCritical || vc.SchedWeight() != 100 {
		t.Errorf("critical pod: class=%v weight=%d, want critical/100", vc.SchedClass(), vc.SchedWeight())
	}
	if vc.Flags&abi.FlagSchedManaged == 0 {
		t.Errorf("daemon facet flag dropped: flags=%#x", vc.Flags)
	}

	vb, _, _ := snk.Lookup(WorkloadKey(batch.UID))
	if vb.SchedClass() != abi.SchedClassBatch {
		t.Errorf("batch pod: class=%v, want batch", vb.SchedClass())
	}
	// Decoupling: identity is shared (same app=web canonical) despite differing
	// policy classes; only the per-instance policy differs.
	if vc.Identity != vb.Identity {
		t.Errorf("policy label split identity: %d vs %d (should share)", vc.Identity, vb.Identity)
	}
	if c.Allocator().Len() != 1 {
		t.Errorf("allocator len = %d, want 1 (one workload, differing policy)", c.Allocator().Len())
	}
}

// TestController_SchedPolicyRetuneNoRenumber checks that flipping a pod's policy
// label propagates a new class on the unchanged-canon fast path WITHOUT changing
// the identity number.
func TestController_SchedPolicyRetuneNoRenumber(t *testing.T) {
	snk := sink.NewSimSink()
	c := NewController(nil, snk)

	p := pod("uid-1", "svc", map[string]string{"app": "api", "prism.io/latency-class": "normal"})
	if err := c.HandlePod(p, EventAdd); err != nil {
		t.Fatalf("add: %v", err)
	}
	v0, _, _ := snk.Lookup(WorkloadKey(p.UID))
	if v0.SchedClass() != abi.SchedClassNormal {
		t.Fatalf("initial class = %v, want normal", v0.SchedClass())
	}

	// Retune to critical; security-relevant labels (app=api) are unchanged, so
	// this exercises the unchanged-canon fast path.
	p.Labels["prism.io/latency-class"] = "critical"
	if err := c.HandlePod(p, EventUpdate); err != nil {
		t.Fatalf("update: %v", err)
	}
	v1, _, _ := snk.Lookup(WorkloadKey(p.UID))
	if v1.SchedClass() != abi.SchedClassCritical {
		t.Errorf("retuned class = %v, want critical", v1.SchedClass())
	}
	if v1.Identity != v0.Identity {
		t.Errorf("retune renumbered identity: %d -> %d", v0.Identity, v1.Identity)
	}
}

func lookupIdentity(t *testing.T, snk *sink.SimSink, p *corev1.Pod) identity.NumericIdentity {
	t.Helper()
	v, ok, err := snk.Lookup(WorkloadKey(p.UID))
	if err != nil {
		t.Fatalf("lookup %s: %v", p.Name, err)
	}
	if !ok {
		t.Fatalf("pod %s missing from sink", p.Name)
	}
	return identity.NumericIdentity(v.Identity)
}
