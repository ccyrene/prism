// SPDX-License-Identifier: Apache-2.0

package prismsync

import (
	"log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
)

// This file adds the standard controller workqueue pattern to Run's event path:
// the informer handlers no longer dispatch synchronously — they enqueue work
// into a rate-limiting queue, and a single worker goroutine processes it. A
// transient sink write failure is RETRIED with exponential backoff
// (AddRateLimited) instead of being logged-and-dropped; a success or a
// non-retryable failure Forgets the item so the limiter stops tracking it.
//
// IMPORTANT scope: the queue is INTERNAL to Run only. HandlePod / OnAdd /
// OnUpdate / OnDelete remain directly callable and unchanged in behaviour — the
// benchmark drives HandlePod synchronously and must keep working. enqueue is
// only ever wired up by Run.
//
// Work-item design. The queue item is the pod's types.UID: the workqueue dedups
// by item, so a burst of updates to one pod collapses to a single unit of work,
// and a retry in flight coalesces with newer events for the same pod. Because
// the UID alone can't tell the worker WHAT to do (and the pod may already be
// gone from any lister by the time a delete is processed), the controller keeps
// a small mutex-guarded `pending` map: UID -> the latest (pod snapshot, event)
// to apply. The worker reads and clears the freshest snapshot for the UID, then
// runs it through the very same dispatchItem/HandlePod path. This keeps dedup,
// relabel, and delete-release all correct:
//   - dedup/relabel: HandlePod is idempotent per UID and re-derives canonical
//     labels from the snapshot, so collapsing N events into one latest snapshot
//     is exactly what we want.
//   - delete-release: the delete snapshot carries the UID, and HandlePod's
//     delete path releases+deletes via the controller's byUID record (or the
//     snapshot's own key as a fallback) — it never needs a live lister entry.

// workItem keys a unit of queued work. It is just the pod UID so the workqueue
// coalesces repeated events for the same pod into one item (and folds an
// in-flight retry together with newer events). The actual pod/event to apply is
// looked up from the controller's pending map at processing time.
type workItem = types.UID

// pendingWork is the freshest snapshot the worker should apply for a UID.
type pendingWork struct {
	pod   *corev1.Pod
	event EventType
}

// enqueue records the latest (pod, event) for the pod's UID and adds its UID to
// the queue. It is called from the informer callbacks (via Run) instead of a
// synchronous dispatch. A nil pod is ignored. Later events for the same UID
// overwrite the snapshot, so the worker always applies the most recent state —
// this is the desired collapse for bursty updates and avoids stale writes.
func (c *Controller) enqueue(pod *corev1.Pod, event EventType) {
	if pod == nil || c.queue == nil {
		return
	}
	c.pendingMu.Lock()
	c.pending[pod.UID] = pendingWork{pod: pod, event: event}
	c.pendingMu.Unlock()
	c.queue.Add(pod.UID)
}

// takePending returns and clears the freshest snapshot for a UID. ok is false if
// nothing is pending (e.g. the item was already drained by a coalesced run).
func (c *Controller) takePending(uid workItem) (pendingWork, bool) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	w, ok := c.pending[uid]
	if ok {
		delete(c.pending, uid)
	}
	return w, ok
}

// restorePending puts a snapshot back for a UID only if no newer event arrived
// while the item was being processed. This is used before a rate-limited retry
// so the requeued item has something to apply, without clobbering a fresher
// snapshot that a concurrent informer event may have just stored.
func (c *Controller) restorePending(uid workItem, w pendingWork) {
	c.pendingMu.Lock()
	if _, newer := c.pending[uid]; !newer {
		c.pending[uid] = w
	}
	c.pendingMu.Unlock()
}

// runWorker pumps the queue until it is shut down (which Run triggers on ctx
// cancellation). Each item is processed exactly once per Get/Done cycle; a
// retryable failure requeues it with backoff, anything else Forgets it.
func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

// processNextItem blocks for one queued UID, applies its freshest pending
// snapshot through the (panic-recovering, metered) dispatch path, and decides
// retry vs forget. Returns false only when the queue has shut down.
func (c *Controller) processNextItem() bool {
	uid, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(uid)

	w, ok := c.takePending(uid)
	if !ok {
		// Nothing to do: a coalesced run already drained this UID. Forget so the
		// limiter doesn't keep tracking it.
		c.queue.Forget(uid)
		return true
	}

	err := c.dispatchItem(w.pod, w.event)
	if err == nil {
		c.queue.Forget(uid)
		return true
	}

	// A sink write failed. Treat it as transient and retry with backoff: put the
	// snapshot back (unless a newer event superseded it) and requeue. The
	// allocator state for this UID was already updated by HandlePod, so the retry
	// re-derives and re-writes idempotently. This is the whole point of the
	// workqueue: a transient BPF-map / kernel error is retried, not dropped. The
	// failure itself is already metered as a sink error inside HandlePod/upsert.
	c.restorePending(uid, w)
	log.Printf("prismsync: retrying %v after sink error (requeue #%d): %v",
		uid, c.queue.NumRequeues(uid)+1, err)
	c.queue.AddRateLimited(uid)
	return true
}

// newQueue builds the controller's rate-limiting queue. Split out so Run stays
// readable and tests could swap it if needed.
func newQueue() workqueue.TypedRateLimitingInterface[workItem] {
	return workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[workItem](),
	)
}

// initQueueState lazily initialises the queue + pending map (Run-only state).
func (c *Controller) initQueueState() {
	c.queue = newQueue()
	c.pending = make(map[types.UID]pendingWork)
}
