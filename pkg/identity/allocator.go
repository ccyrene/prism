// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"container/heap"
	"errors"
	"sync"
)

// ErrSpaceExhausted is returned when the 24-bit dynamic identity space is full.
var ErrSpaceExhausted = errors.New("identity: dynamic 24-bit space exhausted")

// idRef holds a numeric identity and its live reference count. Storing both
// behind one map entry means Allocate/Release touch a single hash bucket (the
// previous design hashed the canonical string into one map and the id into a
// second); the dedup hot path is now one lookup + an int bump.
type idRef struct {
	id   NumericIdentity
	refs int
}

// Allocator maps canonical label sets to numeric identities using the
// Cilium-style "smallest free integer" policy:
//   - the same label set always returns the same identity (deterministic);
//   - a brand-new label set gets the smallest currently-free id >= MinDynamicID;
//   - released ids are reused (smallest-first) so the space stays dense.
//
// It is safe for concurrent use. This is the in-process local allocator; a
// production deployment would back this with a kvstore/CRD, but the allocation
// policy and ABI are identical.
type Allocator struct {
	mu       sync.RWMutex
	byLabels map[string]*idRef        // canonical -> {id, refs}
	freed    minHeap                  // released ids available for reuse
	next     NumericIdentity          // high-water mark for never-used ids
	reserved map[NumericIdentity]bool // ids claimed (e.g. reseeded from a surviving map) that mint must never hand out
}

// NewAllocator returns an empty allocator primed at MinDynamicID.
func NewAllocator() *Allocator {
	return &Allocator{
		byLabels: make(map[string]*idRef),
		next:     MinDynamicID,
		reserved: make(map[NumericIdentity]bool),
	}
}

// Allocate returns the identity for a canonical label set, creating one if the
// set is new. created reports whether a fresh id was minted (vs. an existing
// mapping reused). Each Allocate adds one reference; pair with Release.
func (a *Allocator) Allocate(canonical string) (id NumericIdentity, created bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if e := a.byLabels[canonical]; e != nil {
		e.refs++
		return e.id, false, nil
	}

	id, err = a.mintLocked()
	if err != nil {
		return 0, false, err
	}
	a.byLabels[canonical] = &idRef{id: id, refs: 1}
	return id, true, nil
}

// mintLocked picks the smallest free id (reused first, else high-water), never
// handing out a reserved id. Reserved entries that surface in the freed heap or
// at the high-water mark are skipped; they are owned by a reseeded workload that
// has not been re-observed yet and will be Adopted (not minted) when it is.
func (a *Allocator) mintLocked() (NumericIdentity, error) {
	for a.freed.Len() > 0 {
		id := heap.Pop(&a.freed).(NumericIdentity)
		if a.reserved[id] {
			continue // owned by a reseeded workload; drop it from the reuse pool
		}
		return id, nil
	}
	for {
		if a.next > MaxID {
			return 0, ErrSpaceExhausted
		}
		id := a.next
		a.next++
		if a.reserved[id] {
			continue // skip past a reserved high-water id
		}
		return id, nil
	}
}

// Reserve marks id as in-use so mint never hands it out. It is the restart-
// stability primitive: prismd Ranges the surviving (pinned) map at startup and
// Reserves every identity it finds, so a fresh, not-yet-re-observed workload can
// never be minted the number a surviving workload still owns. Reserving an id
// that is currently in the freed reuse pool removes it from reuse; reserving an
// id at/above the high-water mark advances mint past it. Reserving an id already
// bound to a live label set is a no-op (it is already protected by its refcount).
func (a *Allocator) Reserve(id NumericIdentity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reserved[id] = true
	if id >= a.next {
		// Promote everything below the reserved id (and not itself reserved) into
		// the reuse pool so smallest-free density is preserved, then advance the
		// high-water mark past it.
		for n := a.next; n < id; n++ {
			if !a.reserved[n] {
				heap.Push(&a.freed, n)
			}
		}
		a.next = id + 1
	}
	// If id < next it may sit in the freed heap; mintLocked drops reserved ids it
	// pops, so no eager heap surgery is needed.
}

// Adopt binds canonical to id with refcount 1, claiming the reseeded number for
// the workload that has now been re-observed. It returns false (binding nothing)
// if canonical is already mapped — the caller must fall back to Allocate. Adopt
// clears any reservation on id (the workload now holds it via its refcount) so a
// later Release frees it normally. It does NOT validate that id was previously
// reserved; the controller only calls Adopt with an id it pulled from the
// reseed table, and an unreserved id is simply taken as-is.
func (a *Allocator) Adopt(canonical string, id NumericIdentity) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byLabels[canonical] != nil {
		return false
	}
	a.byLabels[canonical] = &idRef{id: id, refs: 1}
	delete(a.reserved, id) // now owned by a live refcount, not a bare reservation
	if id >= a.next {
		for n := a.next; n < id; n++ {
			if !a.reserved[n] {
				heap.Push(&a.freed, n)
			}
		}
		a.next = id + 1
	}
	return true
}

// Release drops one reference to the identity for a canonical label set. When
// the last reference goes away the id is freed for reuse. Returns true if the
// id became free.
func (a *Allocator) Release(canonical string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	e := a.byLabels[canonical]
	if e == nil {
		return false
	}
	e.refs--
	if e.refs > 0 {
		return false
	}
	delete(a.byLabels, canonical)
	heap.Push(&a.freed, e.id)
	return true
}

// Lookup returns the identity for a canonical label set without allocating.
func (a *Allocator) Lookup(canonical string) (NumericIdentity, bool) {
	a.mu.RLock()
	e := a.byLabels[canonical]
	a.mu.RUnlock()
	if e == nil {
		return 0, false
	}
	return e.id, true
}

// Len returns the number of live (allocated) identities.
func (a *Allocator) Len() int {
	a.mu.RLock()
	n := len(a.byLabels)
	a.mu.RUnlock()
	return n
}

// minHeap is a min-heap of freed identities (smallest reused first).
type minHeap []NumericIdentity

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(NumericIdentity)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
