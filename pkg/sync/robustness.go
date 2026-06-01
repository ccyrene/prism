// SPDX-License-Identifier: Apache-2.0

package prismsync

import (
	"context"
	"log"
	"time"

	"github.com/prism-bus/prism/pkg/abi"
	"github.com/prism-bus/prism/pkg/identity"
)

// Reseed rebuilds the allocator's view from a surviving identity map BEFORE the
// informer starts. The pinned BPF map outlives prismd (a graceful Close detaches
// the userspace handle but leaves the map pinned in bpffs), so on restart the
// daemon inherits every (key, identity) a prior incarnation wrote. Reseed Ranges
// the sink and, for each live entry:
//
//   - Reserves the numeric identity in the allocator, so a fresh, not-yet-
//     re-observed workload can never be minted a number a survivor still owns; and
//   - records reseedByHash[LabelHash] = identity, so when the surviving workload
//     IS re-observed (its informer add arrives), HandlePod reclaims that exact
//     number via Adopt instead of minting a new one.
//
// Reseed is idempotent enough to call once at the top of Run; calling it on an
// empty sink is a no-op. It must run before any HandlePod/enqueue so reservations
// are in place before the first allocation.
//
// Reclaim is keyed on the 64-bit FNV label hash, not the canonical label string,
// because the ABI map value stores only the hash (LabelHash) — the canonical
// string is not persisted. A hash collision between two distinct label sets is
// astronomically unlikely for FNV-1a/64; if it ever happened the sole effect is a
// benign identity renumber that self-corrects on the next resync. See the
// reseedByHash field doc on Controller.
func (c *Controller) Reseed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reseedByHash == nil {
		c.reseedByHash = make(map[uint64]identity.NumericIdentity)
	}
	n := 0
	c.sink.Range(func(_ abi.WorkloadKey, val abi.PrismIdentity) bool {
		id := identity.NumericIdentity(val.Identity)
		c.alloc.Reserve(id)
		c.reseedByHash[val.LabelHash] = id
		n++
		return true
	})
	if n > 0 {
		log.Printf("prismsync: reseed reserved %d surviving identit(ies) from %q sink for reclaim",
			n, c.sink.Kind())
	}
}

// Reconcile is the map-GC sweep: it evicts entries that are in the sink but have
// no corresponding live pod in byUID. Such leaks happen when a pod delete was
// missed while the daemon was down (the informer's initial list won't replay a
// delete, so the stale key would otherwise linger in the pinned map forever). It
// builds the set of keys the controller currently believes are live, Ranges the
// sink, and Deletes every sink key NOT in that set, returning the eviction count.
//
// Ordering note: Reconcile snapshots the live-key set under mu, then Ranges and
// Deletes OUTSIDE mu so it never holds the controller lock across sink writes
// (and the sim/compact sinks must not be mutated while their Range lock is held).
// The single-writer daemon calls it from a ticker after the cache has synced, so
// byUID reflects every live pod and a key absent from it is genuinely orphaned.
func (c *Controller) Reconcile() int {
	// Snapshot the live keys the controller is tracking.
	c.mu.Lock()
	live := make(map[abi.WorkloadKey]struct{}, len(c.byUID))
	for _, rec := range c.byUID {
		live[rec.key] = struct{}{}
	}
	c.mu.Unlock()

	// Collect orphans during Range (don't Delete inside the sink's Range lock).
	var stale []abi.WorkloadKey
	c.sink.Range(func(k abi.WorkloadKey, _ abi.PrismIdentity) bool {
		if _, ok := live[k]; !ok {
			stale = append(stale, k)
		}
		return true
	})

	evicted := 0
	for _, k := range stale {
		if err := c.sink.Delete(k); err != nil {
			c.Metrics.SinkError("delete")
			log.Printf("prismsync: reconcile: evict stale key %d failed: %v", k, err)
			continue
		}
		evicted++
	}
	if evicted > 0 {
		log.Printf("prismsync: reconcile evicted %d stale sink entr(ies) (missed deletes)", evicted)
	}
	return evicted
}

// reconcileLoop runs Reconcile every defaultResync until ctx is cancelled. It is
// started by Run after the cache has synced, giving the map a periodic GC sweep
// that catches deletes missed during a daemon outage. The interval matches the
// informer resync so a leaked key self-heals within one resync window.
func (c *Controller) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(defaultResync)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Reconcile()
		}
	}
}
