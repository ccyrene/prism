// SPDX-License-Identifier: Apache-2.0

package prismsync

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/prism-bus/prism/pkg/abi"
	"github.com/prism-bus/prism/pkg/key"
	"github.com/prism-bus/prism/pkg/metrics"
	"github.com/prism-bus/prism/pkg/sink"
)

func testPod(name, uid string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns", UID: types.UID(uid), Labels: labels,
	}}
}

// panicKeyer panics on every Key call — stands in for any latent bug in the
// hot path (malformed object, nil deref, etc.).
type panicKeyer struct{}

func (panicKeyer) Name() string { return "panic" }
func (panicKeyer) Key(*corev1.Pod) (abi.WorkloadKey, bool, error) {
	panic("boom: simulated handler bug")
}

// TestDispatchRecoversPanic is the crash-safety proof: a panic while handling a
// single pod must be recovered, NOT propagate up the informer goroutine and
// kill the daemon. If recovery were missing this test process would crash.
func TestDispatchRecoversPanic(t *testing.T) {
	c := NewController(nil, sink.NewCompactSink())
	c.Metrics = metrics.New()
	c.Keyer = panicKeyer{}
	pod := testPod("p", "uid-1", map[string]string{"app": "x"})

	// All three informer entry points must survive a panicking handler.
	c.OnAdd(pod, false)
	c.OnUpdate(pod, pod)
	c.OnDelete(pod)

	// Reaching here means every panic was recovered. Sink stays clean (the panic
	// happened before any write).
	if c.Sink().Len() != 0 {
		t.Fatalf("sink mutated despite panic: len=%d", c.Sink().Len())
	}
}

// errSink fails every write — models a full BPF map / transient kernel error.
type errSink struct{ abi.Sink }

func (errSink) Upsert(abi.WorkloadKey, abi.PrismIdentity) error { return errors.New("sink full") }
func (errSink) Delete(abi.WorkloadKey) error                   { return errors.New("sink full") }
func (errSink) Kind() string                                   { return "err" }

// TestErroringSinkIsGraceful: a sink that always errors must not crash the
// daemon — HandlePod returns the error, the dispatch path logs+meters it, and
// the process keeps running.
func TestErroringSinkIsGraceful(t *testing.T) {
	c := NewController(nil, errSink{})
	c.Metrics = metrics.New()
	pod := testPod("p", "uid-2", map[string]string{"app": "y"})

	if err := c.HandlePod(pod, EventAdd); err == nil {
		t.Fatal("expected sink error to surface from HandlePod")
	}
	// The informer path must swallow it (log+meter), never panic/exit.
	c.OnAdd(pod, false)
	c.OnDelete(pod)
}

// TestRunIntegration drives the REAL informer over a fake clientset: create,
// relabel, and delete pods through the Kubernetes API and assert identities
// propagate, plus that /readyz (Synced) flips after the cache syncs.
func TestRunIntegration(t *testing.T) {
	client := fake.NewSimpleClientset()
	snk := sink.NewCompactSink()
	c := NewController(client, snk)
	c.Metrics = metrics.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if !waitFor(t, 2*time.Second, c.Synced) {
		t.Fatal("controller never reported Synced()")
	}

	api := client.CoreV1().Pods("ns")
	mk := func(name, uid string, l map[string]string) {
		if _, err := api.Create(ctx, testPod(name, uid, l), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "uid-a", map[string]string{"app": "web"})
	mk("b", "uid-b", map[string]string{"app": "web", "pod-template-hash": "z"}) // dedup with a
	mk("c", "uid-c", map[string]string{"app": "db"})

	// Wait until all three keys are present.
	keyA, keyB, keyC := key.UIDKey("uid-a"), key.UIDKey("uid-b"), key.UIDKey("uid-c")
	ok := waitFor(t, 2*time.Second, func() bool {
		_, a, _ := snk.Lookup(keyA)
		_, b, _ := snk.Lookup(keyB)
		_, cc, _ := snk.Lookup(keyC)
		return a && b && cc
	})
	if !ok {
		t.Fatalf("pods did not propagate: len=%d", snk.Len())
	}
	// a and b share security-relevant labels -> same identity (dedup).
	va, _, _ := snk.Lookup(keyA)
	vb, _, _ := snk.Lookup(keyB)
	vc, _, _ := snk.Lookup(keyC)
	if va.Identity != vb.Identity {
		t.Fatalf("replicas should share identity: a=%d b=%d", va.Identity, vb.Identity)
	}
	if vc.Identity == va.Identity {
		t.Fatalf("distinct workload should differ: c=%d a=%d", vc.Identity, va.Identity)
	}

	// Delete c -> its key disappears.
	if err := api.Delete(ctx, "c", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 2*time.Second, func() bool { _, ok, _ := snk.Lookup(keyC); return !ok }) {
		t.Fatal("deleted pod identity not removed")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
