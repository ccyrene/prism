// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"sync"
	"sync/atomic"

	"github.com/prism-bus/prism/pkg/abi"
)

// FastSink is a lock-free-read userspace identity sink: an open-addressing hash
// table whose buckets are atomic pointers, swapped atomically on resize. Reads
// (Lookup) take NO lock — they load the table snapshot and probe with plain
// atomic loads — which is exactly the access pattern of the in-kernel BPF hash
// map a sched_ext consumer hits on every decision. Writes serialize on a mutex,
// which matches reality: the daemon is the single writer of the bus.
//
// This is the optimized stand-in the benchmarks drive; it closes most of the
// gap to the native C / BPF-map lookup that the Go map + RWMutex SimSink left.
type FastSink struct {
	mu    sync.Mutex // writers only (single-writer daemon)
	table atomic.Pointer[fastTable]
}

type fastEntry struct {
	key abi.WorkloadKey
	val abi.PrismIdentity
}

type fastTable struct {
	buckets []atomic.Pointer[fastEntry]
	mask    uint64
	live    int // live entries
	used    int // live + tombstones (probe-sequence occupancy)
}

// tombstone marks a deleted slot so probing continues past it.
var tombstone = &fastEntry{}

const fastInitSize = 1024 // power of two

// NewFastSink returns an empty lock-free-read sink.
func NewFastSink() *FastSink {
	s := &FastSink{}
	s.table.Store(newFastTable(fastInitSize))
	return s
}

func newFastTable(size int) *fastTable {
	return &fastTable{buckets: make([]atomic.Pointer[fastEntry], size), mask: uint64(size - 1)}
}

// mix is splitmix64 finalizer — spreads FNV/inode keys across buckets well.
func mix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// Lookup is lock-free: load the current table, then probe with atomic loads.
func (s *FastSink) Lookup(key abi.WorkloadKey) (abi.PrismIdentity, bool, error) {
	t := s.table.Load()
	i := mix(uint64(key)) & t.mask
	for {
		e := t.buckets[i].Load()
		if e == nil {
			return abi.PrismIdentity{}, false, nil
		}
		if e != tombstone && e.key == key {
			return e.val, true, nil
		}
		i = (i + 1) & t.mask
	}
}

// Upsert inserts or overwrites under the writer lock.
func (s *FastSink) Upsert(key abi.WorkloadKey, id abi.PrismIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.table.Load()
	if (t.used+1)*4 >= len(t.buckets)*3 { // grow at 0.75 occupancy
		t = s.growLocked(t)
	}
	t.insertLocked(key, id)
	return nil
}

// Delete tombstones the slot under the writer lock (idempotent).
func (s *FastSink) Delete(key abi.WorkloadKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.table.Load()
	i := mix(uint64(key)) & t.mask
	for {
		e := t.buckets[i].Load()
		if e == nil {
			return nil // absent
		}
		if e != tombstone && e.key == key {
			t.buckets[i].Store(tombstone)
			t.live--
			return nil
		}
		i = (i + 1) & t.mask
	}
}

// insertLocked must be called with the writer lock held.
func (t *fastTable) insertLocked(key abi.WorkloadKey, id abi.PrismIdentity) {
	i := mix(uint64(key)) & t.mask
	firstTomb := -1
	for {
		e := t.buckets[i].Load()
		if e == nil {
			slot := i
			if firstTomb >= 0 {
				slot = uint64(firstTomb)
			} else {
				t.used++
			}
			t.buckets[slot].Store(&fastEntry{key: key, val: id})
			t.live++
			return
		}
		if e == tombstone {
			if firstTomb < 0 {
				firstTomb = int(i)
			}
		} else if e.key == key {
			t.buckets[i].Store(&fastEntry{key: key, val: id}) // overwrite, live unchanged
			return
		}
		i = (i + 1) & t.mask
	}
}

// growLocked builds a larger table (dropping tombstones) and publishes it.
func (s *FastSink) growLocked(old *fastTable) *fastTable {
	size := len(old.buckets) * 2
	nt := newFastTable(size)
	for i := range old.buckets {
		e := old.buckets[i].Load()
		if e != nil && e != tombstone {
			nt.insertLocked(e.key, e.val)
		}
	}
	s.table.Store(nt) // atomic publish; in-flight readers keep the old snapshot
	return nt
}

// Len returns the live entry count.
func (s *FastSink) Len() int {
	s.mu.Lock()
	n := s.table.Load().live
	s.mu.Unlock()
	return n
}

// Range iterates every live entry, calling fn for each and stopping early if fn
// returns false. It snapshots the current table and walks its buckets, skipping
// empty (nil) and tombstoned slots. Reads are lock-free like Lookup, so a
// concurrent writer may grow into a new table mid-walk; the snapshot keeps this
// consistent for the single-writer daemon, where Range is only called at
// quiescent points (reseed/reconcile).
func (s *FastSink) Range(fn func(key abi.WorkloadKey, id abi.PrismIdentity) bool) {
	t := s.table.Load()
	for i := range t.buckets {
		e := t.buckets[i].Load()
		if e == nil || e == tombstone {
			continue
		}
		if !fn(e.key, e.val) {
			return
		}
	}
}

// Kind reports "sim" (this is the optimized simulation sink).
func (s *FastSink) Kind() string { return "sim" }

// Close drops the table.
func (s *FastSink) Close() error {
	s.mu.Lock()
	s.table.Store(newFastTable(fastInitSize))
	s.mu.Unlock()
	return nil
}
