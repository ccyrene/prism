// SPDX-License-Identifier: Apache-2.0

// Package prismsync is the control plane of the Prism identity bus: it watches
// Kubernetes Pods, derives a stable workload identity from each Pod's
// security-relevant labels, and propagates that identity into an abi.Sink (the
// pinned BPF map in production, a userspace map in simulation/bench).
//
// The package is deliberately importable and the propagation path is exposed as
// directly-callable methods (HandlePod / OnAdd / OnUpdate / OnDelete) so the
// benchmark harness can drive Pod events synchronously without spinning up the
// informer goroutines. Run wires those same methods to a client-go informer for
// the real daemon.
package prismsync

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ccyrene/prism/pkg/abi"
	"github.com/ccyrene/prism/pkg/identity"
	"github.com/ccyrene/prism/pkg/key"
	"github.com/ccyrene/prism/pkg/metrics"
)

// EventType names the three Pod lifecycle transitions the controller handles.
type EventType int

const (
	// EventAdd is a Pod creation (or initial-list sync).
	EventAdd EventType = iota
	// EventUpdate is a Pod modification; labels may or may not have changed.
	EventUpdate
	// EventDelete is a Pod removal.
	EventDelete
)

// defaultResync is how often the informer re-lists; 0 disables periodic resync.
// We keep it modest so a missed delete eventually self-heals without flooding.
const defaultResync = 10 * time.Minute

// Controller turns Pod events into identity-bus writes. It owns the allocator
// (label-set -> numeric identity) and writes through the injected sink. It is
// safe for concurrent use: the informer delivers events from a single goroutine
// but bench harnesses may call the Handle methods from several, so per-pod
// bookkeeping is mutex-guarded.
type Controller struct {
	client kubernetes.Interface
	sink   abi.Sink
	alloc  *identity.Allocator

	mu sync.Mutex
	// byUID remembers, per pod UID, the canonical label set last applied and the
	// bus key derived for it — in ONE value-typed map entry (no per-pod pointer
	// allocation). canon lets Update detect label changes and Delete release the
	// exact canonical form Allocated; key lets Delete remove the exact entry even
	// after the pod's cgroup dir is gone (the cgroup keyer can't re-stat it).
	byUID map[types.UID]podRec

	// Flags is the facet bitmask stamped onto every identity this controller
	// writes. The daemon sets it from its enabled subsystems; tests leave it 0.
	Flags uint32

	// Keyer derives the bus key for a pod. Default UIDKeyer (sim/bench parity);
	// set key.CgroupKeyer for real-kernel cgroup-id parity. Set before Run.
	Keyer key.Keyer

	// NodeName, when non-empty, scopes the informer to pods on this node via a
	// spec.nodeName field selector — the correct mode for a per-node DaemonSet.
	NodeName string

	// TrimCache (default true) installs an informer TransformFunc that keeps only
	// the fields identity derivation needs (UID, namespace/name, labels, QoS) and
	// drops spec/status/managedFields BEFORE caching — the single biggest RAM win
	// for an informer daemon (a real Pod is multi-KB; the trimmed copy is a few
	// hundred bytes). Set false only to measure the difference.
	TrimCache bool

	// Metrics is the optional Prometheus surface. nil in tests/bench (every
	// metrics call is nil-receiver-safe, so the hot path pays nothing).
	Metrics *metrics.Metrics

	// synced flips true once the informer's cache has synced; it backs the
	// daemon's /readyz probe so Kubernetes only routes/keeps the pod once Prism
	// is actually propagating identities.
	synced atomic.Bool

	// queue is the rate-limiting workqueue that backs Run's event path: informer
	// callbacks enqueue pod UIDs, a worker goroutine drains them, and a transient
	// sink failure is retried with backoff instead of dropped. It is nil outside
	// Run — HandlePod and the direct Handle* methods never touch it, so the
	// benchmark's synchronous HandlePod path is unaffected. See workqueue.go.
	queue workqueue.TypedRateLimitingInterface[workItem]
	// pending holds, per UID, the freshest (pod snapshot, event) the worker should
	// apply. Guarded by pendingMu (separate from mu so an in-flight HandlePod does
	// not contend with enqueue). Run-only, like queue.
	pendingMu sync.Mutex
	pending   map[types.UID]pendingWork

	// reseedByHash maps a surviving identity's 64-bit FNV label hash (read out of
	// the pinned map by Reseed at startup) to the numeric identity it held in the
	// previous daemon incarnation. HandlePod's allocate branch consults it so a
	// re-observed workload RECLAIMS its original number instead of being minted a
	// fresh one — the entries are deleted as they are reclaimed. Guarded by mu
	// (only touched under mu in Reseed and HandlePod). Empty outside the restart
	// window, so the steady-state hot path pays a single nil/len-0 map probe.
	//
	// Caveat: the map value stores only the FNV label hash, not the canonical
	// label string, so reclaim is keyed on that 64-bit hash. A hash collision
	// between two DIFFERENT canonical label sets (astronomically unlikely for
	// FNV-1a/64) could hand a reclaimed number to the wrong workload; the only
	// consequence is a benign identity renumber, self-corrected on the next
	// resync. This is the documented trade-off of not persisting canon in the map.
	reseedByHash map[uint64]identity.NumericIdentity
}

// Synced reports whether the informer cache has completed its initial sync.
// It is the readiness signal for the daemon's /readyz endpoint.
func (c *Controller) Synced() bool { return c.synced.Load() }

// NewController returns a Controller that writes Pod identities into sink using
// a fresh allocator. client may be a real clientset or a fake one; it is only
// used by Run (the informer), so bench/tests that call HandlePod directly can
// pass any (even nil) client.
func NewController(client kubernetes.Interface, sink abi.Sink) *Controller {
	return &Controller{
		client:    client,
		sink:      sink,
		alloc:     identity.NewAllocator(),
		byUID:     make(map[types.UID]podRec),
		Keyer:     key.UIDKeyer{},
		TrimCache: true,
	}
}

// trimPod is the informer TransformFunc: keep only what identity derivation
// reads (UID, namespace/name, labels, QoS class) and drop everything else —
// spec, container statuses, conditions, and especially managedFields, which
// dominate a Pod's in-memory size. Behavior is unchanged (HandlePod only reads
// labels + UID + QOSClass); the resident informer cache shrinks roughly an order
// of magnitude. Non-Pod objects (e.g. delete tombstones) pass through untouched.
func trimPod(obj interface{}) (interface{}, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       pod.UID,
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Labels:    pod.Labels,
		},
		Status: corev1.PodStatus{QOSClass: pod.Status.QOSClass},
	}, nil
}

// podRec is the per-UID bookkeeping kept by the controller.
type podRec struct {
	canon string
	key   abi.WorkloadKey
}

// Sink exposes the underlying sink (handy for tests/bench assertions).
func (c *Controller) Sink() abi.Sink { return c.sink }

// Allocator exposes the underlying allocator (handy for tests/bench assertions).
func (c *Controller) Allocator() *identity.Allocator { return c.alloc }

// WorkloadKey derives the stable u64 bus key for a pod from its UID via FNV-1a.
// On a real cgroup-v2 kernel the BPF side keys on bpf_get_current_cgroup_id();
// here the UID-derived key is the userspace stand-in, fixed-width per the ABI.
func WorkloadKey(uid types.UID) abi.WorkloadKey {
	return key.UIDKey(uid)
}

// HandlePod is the single synchronous propagation path. Benchmarks and tests
// call it directly; OnAdd/OnUpdate/OnDelete and the informer funnel into it.
//
// Add/Update: derive canonical labels -> allocate identity -> upsert the value.
// Delete: release the identity for the canonical form we recorded -> delete the
// key. The method is idempotent per UID so duplicate events are harmless.
func (c *Controller) HandlePod(pod *corev1.Pod, event EventType) error {
	if pod == nil {
		return nil
	}

	switch event {
	case EventAdd, EventUpdate:
		wkey, ok, err := c.Keyer.Key(pod)
		if err != nil {
			return err
		}
		if !ok {
			// The pod's cgroup isn't present yet (cgroup keyer, pod not started
			// on this node). Skip; a later update/resync will re-derive it.
			return nil
		}
		// Spoof-resistant identity: fold in the pod's namespace + service
		// account (both API/RBAC-controlled, not freely settable by the
		// workload) alongside its security-relevant labels. Replicas of one
		// Deployment share ns+SA+labels, so they still dedup to one identity;
		// two workloads with identical labels in different namespaces (or with
		// different SAs) correctly get distinct identities. See
		// identity.WorkloadCanonical.
		canon := identity.WorkloadCanonical(pod.Labels, pod.Namespace, pod.Spec.ServiceAccountName)
		// Scheduling policy (the thick bus) is derived from the pod's prism.io/
		// labels and is INDEPENDENT of identity (those labels are excluded from
		// canon), so it is recomputed on every event and re-stamped even on the
		// unchanged-canon fast path below — retuning a pod's latency class
		// propagates to the bus without renumbering its identity.
		pol := identity.DeriveSchedPolicy(pod.Labels)

		c.mu.Lock()
		prev, hadPrev := c.byUID[pod.UID]
		if hadPrev && prev.canon == canon {
			// Labels (security-relevant subset) unchanged: refresh the value's
			// timestamp but don't churn the allocator refcount. Re-reading the
			// existing id keeps the upsert allocation-free.
			id, _ := c.alloc.Lookup(canon)
			c.byUID[pod.UID] = podRec{canon: canon, key: wkey}
			c.mu.Unlock()
			return c.upsert(wkey, id, canon, pol)
		}

		// New pod, or its identity-relevant labels changed: release the old
		// mapping (if any) and allocate the new one. Refcounting in the
		// allocator means shared label sets keep their id until the last pod
		// using them goes away.
		if hadPrev {
			c.alloc.Release(prev.canon)
		}

		// Restart reclaim: if a previous daemon incarnation had already minted a
		// number for THIS canonical label set (recovered from the surviving pinned
		// map by Reseed, keyed on the FNV label hash), Adopt that exact number
		// instead of minting a fresh one — so a workload keeps its identity across
		// a daemon restart. We only reach this on the FIRST observation of canon
		// (Adopt fails harmlessly if canon is already mapped, falling through to
		// Allocate); the hash entry is one-shot and deleted on use.
		var (
			id      identity.NumericIdentity
			created bool
		)
		if len(c.reseedByHash) > 0 {
			h := identity.LabelHash(canon)
			if rid, ok := c.reseedByHash[h]; ok {
				delete(c.reseedByHash, h)
				if c.alloc.Adopt(canon, rid) {
					id, created = rid, true
				}
			}
		}
		if !created {
			// Either no reseed entry, or canon was already mapped (Adopt declined):
			// take the normal allocate path. Allocate returns the existing id for an
			// already-mapped canon (created=false), which is exactly right.
			var err error
			id, created, err = c.alloc.Allocate(canon)
			if err != nil {
				c.mu.Unlock()
				c.Metrics.AllocFailure() // e.g. 24-bit space exhausted: log+meter, never crash
				return err
			}
		}
		c.byUID[pod.UID] = podRec{canon: canon, key: wkey}
		c.mu.Unlock()
		if created {
			c.Metrics.IdentityAllocated()
		}
		return c.upsert(wkey, id, canon, pol)

	case EventDelete:
		c.mu.Lock()
		rec, had := c.byUID[pod.UID]
		if had {
			delete(c.byUID, pod.UID)
			c.alloc.Release(rec.canon)
		}
		c.mu.Unlock()
		wkey := rec.key
		if !had {
			// Never recorded an add for this UID; best-effort re-derive.
			k, ok, _ := c.Keyer.Key(pod)
			if !ok {
				return nil
			}
			wkey = k
		}
		if err := c.sink.Delete(wkey); err != nil {
			c.Metrics.SinkError("delete")
			return err
		}
		return nil
	}
	return nil
}

// upsert writes the value and meters a sink failure. Callers return its error;
// the informer dispatch logs it but never treats it as fatal.
func (c *Controller) upsert(wkey abi.WorkloadKey, id identity.NumericIdentity, canon string, pol identity.SchedPolicy) error {
	if err := c.sink.Upsert(wkey, c.value(id, canon, pol)); err != nil {
		c.Metrics.SinkError("upsert")
		return err
	}
	return nil
}

// value builds the ABI map value for an allocated identity, stamping the
// controller's facet flags, the per-workload scheduling policy (the thick bus —
// see identity.DeriveSchedPolicy), and the current label hash + write time. The
// facet bits and the policy sub-fields live in disjoint bits of Flags, so OR-ing
// them is lossless (abi.EncodeSchedPolicy zeroes everything but its own field).
func (c *Controller) value(id identity.NumericIdentity, canon string, pol identity.SchedPolicy) abi.PrismIdentity {
	return abi.PrismIdentity{
		Identity:  uint32(id),
		Flags:     c.Flags | pol.Encode(),
		LabelHash: identity.LabelHash(canon),
		UpdatedNs: coarseNowNs(),
	}
}

// coarseNowNs returns wall-clock nanoseconds at ~1 ms granularity from a cached
// value refreshed by one process-wide ticker, so the propagation hot path does
// a single atomic load instead of a time.Now() (vDSO) call per write. UpdatedNs
// is a "last write" marker / change-detection aid, so ms coarseness is fine.
var (
	cachedNs   atomic.Int64
	clockOnce  sync.Once
)

func coarseNowNs() uint64 {
	clockOnce.Do(func() {
		cachedNs.Store(time.Now().UnixNano())
		go func() {
			t := time.NewTicker(time.Millisecond)
			for range t.C {
				cachedNs.Store(time.Now().UnixNano())
			}
		}()
	})
	return uint64(cachedNs.Load())
}

// eventName maps an EventType to its metrics/log label.
func eventName(event EventType) string {
	switch event {
	case EventAdd:
		return "add"
	case EventUpdate:
		return "update"
	case EventDelete:
		return "delete"
	default:
		return "unknown"
	}
}

// dispatch runs one pod event through HandlePod with PANIC RECOVERY, error
// logging, and metrics, discarding the (already-logged) error. It is the
// crash-safety boundary used by the direct OnAdd/OnUpdate/OnDelete callbacks
// (which tests exercise directly). Run's worker uses dispatchItem instead so it
// can act on the error (retry vs forget).
func (c *Controller) dispatch(pod *corev1.Pod, event EventType, name string) {
	_ = c.dispatchRecovered(pod, event, name)
}

// dispatchItem is the queue-worker variant: same panic recovery + metrics as
// dispatch, but it RETURNS the error so processNextItem can retry a transient
// sink failure with backoff instead of dropping it. A recovered panic is
// reported as a nil error (it is metered and must NOT be retried — replaying a
// pod that reliably panics would loop forever).
func (c *Controller) dispatchItem(pod *corev1.Pod, event EventType) error {
	return c.dispatchRecovered(pod, event, eventName(event))
}

// dispatchRecovered is the shared core: it runs HandlePod under panic recovery
// with the existing metrics, logs any error, and returns it. On a recovered
// panic it returns nil (the named return is reset in the deferred recover) so
// callers never treat a panic as a retryable error.
func (c *Controller) dispatchRecovered(pod *corev1.Pod, event EventType, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = nil
			c.Metrics.HandlerPanic()
			log.Printf("prismsync: RECOVERED panic handling %s %s/%s: %v",
				name, pod.Namespace, pod.Name, r)
		}
	}()

	var start time.Time
	if c.Metrics != nil {
		start = time.Now()
	}
	err = c.HandlePod(pod, event)
	c.Metrics.PodProcessed(name)
	if c.Metrics != nil {
		c.Metrics.ObservePropagation(time.Since(start).Seconds())
		c.Metrics.SetLive(c.alloc.Len())
	}
	if err != nil {
		log.Printf("prismsync: %s %s/%s: %v", name, pod.Namespace, pod.Name, err)
	}
	return err
}

// OnAdd is the informer add callback. isInInitialList is part of the client-go
// ResourceEventHandler contract and is unused here (an add is an add).
func (c *Controller) OnAdd(obj interface{}, isInInitialList bool) {
	if pod, ok := obj.(*corev1.Pod); ok {
		c.dispatch(pod, EventAdd, "add")
	}
}

// OnUpdate is the informer update callback.
func (c *Controller) OnUpdate(oldObj, newObj interface{}) {
	if pod, ok := newObj.(*corev1.Pod); ok {
		c.dispatch(pod, EventUpdate, "update")
	}
}

// OnDelete is the informer delete callback. It unwraps the tombstone that
// client-go may deliver when the final state of a deleted object was missed.
func (c *Controller) OnDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, isTomb := obj.(cache.DeletedFinalStateUnknown)
		if !isTomb {
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}
	c.dispatch(pod, EventDelete, "delete")
}

// Run starts a Pod informer wired to the Handle* callbacks and blocks until ctx
// is cancelled. It is the real-daemon entry point; bench/tests use HandlePod
// directly and never call Run. It returns nil on clean shutdown and an error
// only if the initial cache sync fails.
func (c *Controller) Run(ctx context.Context) error {
	// Restart-stability: before the informer starts, recover the identities a
	// prior daemon incarnation wrote into the (pinned, surviving) sink so they can
	// be reclaimed rather than re-minted. No-op on an empty sink / cold start.
	c.Reseed()

	var opts []informers.SharedInformerOption
	if c.NodeName != "" {
		// Node-scoped: a per-node DaemonSet should watch only its own node's
		// pods. The field selector is pushed to the apiserver list+watch.
		sel := fields.OneTermEqualSelector("spec.nodeName", c.NodeName).String()
		opts = append(opts, informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.FieldSelector = sel }))
		log.Printf("prismsync: node-scoped to %q (fieldSelector %q)", c.NodeName, sel)
	}
	if c.TrimCache {
		// Drop spec/status/managedFields before caching — the big RAM win.
		opts = append(opts, informers.WithTransform(trimPod))
		log.Printf("prismsync: cache-trim ON (informer keeps only UID/labels/QoS)")
	}
	factory := informers.NewSharedInformerFactoryWithOptions(c.client, defaultResync, opts...)
	podInformer := factory.Core().V1().Pods().Informer()

	// Wire the informer straight into the rate-limiting workqueue instead of the
	// synchronous dispatch. The handlers only enqueue (cheap, non-blocking); the
	// worker goroutine below drains the queue and RETRIES transient sink failures
	// with backoff. The directly-callable OnAdd/OnUpdate/OnDelete still dispatch
	// synchronously for tests/bench — only Run's path is queued.
	c.initQueueState()
	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o interface{}) { c.enqueueObj(o, EventAdd) },
		UpdateFunc: func(_, newObj interface{}) { c.enqueueObj(newObj, EventUpdate) },
		DeleteFunc: func(o interface{}) { c.enqueueDelete(o) },
	}); err != nil {
		return err
	}

	stop := ctx.Done()
	factory.Start(stop)

	if !cache.WaitForCacheSync(stop, podInformer.HasSynced) {
		return context.Canceled
	}
	c.synced.Store(true) // /readyz now reports ready
	log.Printf("prismsync: informer synced, propagating pod identities into %q sink", c.sink.Kind())

	// One worker goroutine processes the queue. A single worker preserves the
	// per-pod ordering the informer already gives us; the workqueue dedups bursts.
	go c.runWorker()

	// Periodic map GC: evict entries whose pod delete was missed while the daemon
	// was down. Runs every defaultResync until ctx is cancelled. It starts after
	// the cache has synced so byUID already reflects every live pod — otherwise a
	// not-yet-listed pod's key could be mistaken for an orphan.
	go c.reconcileLoop(ctx)

	<-stop
	c.queue.ShutDown() // unblocks the worker's Get(), draining cleanly
	return nil
}

// enqueueObj type-asserts a *corev1.Pod and enqueues it for add/update. Mirrors
// the OnAdd/OnUpdate guards but routes through the queue (Run-only).
func (c *Controller) enqueueObj(obj interface{}, event EventType) {
	if pod, ok := obj.(*corev1.Pod); ok {
		c.enqueue(pod, event)
	}
}

// enqueueDelete unwraps the tombstone client-go may deliver for a missed final
// state (same logic as OnDelete) and enqueues a delete. The pod snapshot is
// captured now, so the worker can release+delete the right entry even though the
// pod is already gone from the lister.
func (c *Controller) enqueueDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, isTomb := obj.(cache.DeletedFinalStateUnknown)
		if !isTomb {
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}
	c.enqueue(pod, EventDelete)
}
