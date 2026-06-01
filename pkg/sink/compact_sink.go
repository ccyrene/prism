// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"sync"
	"sync/atomic"

	"github.com/prism-bus/prism/pkg/abi"
)

// CompactSink is an open-addressing identity table that stores each key+value
// INLINE in one contiguous slot array, with lock-free seqlock reads. The point
// is scale CONSISTENCY: FastSink keeps a pointer per bucket, so at populations
// larger than the CPU's last-level cache a lookup costs TWO cache misses (the
// bucket pointer, then the heap entry). CompactSink packs the 24-byte value
// beside its key in the slot, so a lookup touches ONE cache line — halving the
// miss rate and flattening the latency curve as the table grows toward millions.
//
// Reads are lock-free: a per-slot sequence counter (even = stable, odd = a write
// in progress) lets a reader detect and retry a torn read without a mutex, so
// readers never block the single-writer daemon. All slot fields are atomic, so
// it is data-race-free (go test -race clean).
//
// A hash table larger than the LLC still pays ~1 unavoidable cache miss per
// random lookup — that is a hardware floor no structure escapes — but in the
// real per-node operating point (<=~256 pods) the whole table lives in L1/L2
// and lookups are genuinely flat and fast.
type CompactSink struct {
	mu    sync.Mutex // serializes writers (single-writer daemon)
	table atomic.Pointer[compactTable]
}

type compactSlot struct {
	seq atomic.Uint32 // 0 = empty (ends probe); odd = write in progress; even>0 = stable
	_   uint32        // padding to 8-byte-align the words below
	key atomic.Uint64
	w0  atomic.Uint64 // Identity | Flags<<32
	w1  atomic.Uint64 // LabelHash
	w2  atomic.Uint64 // UpdatedNs
}

type compactTable struct {
	slots []compactSlot
	mask  uint64
	live  int
	used  int // live + tombstones (probe occupancy)
}

// tombKey marks a deleted-but-not-empty slot so probing continues past it. A
// real workload key is an FNV/inode hash; the all-ones sentinel colliding with
// a genuine key is astronomically unlikely and, if it happened, would only cause
// a benign miss (re-resolved by the daemon).
const tombKey = ^uint64(0)

func packVal(id abi.PrismIdentity) (w0, w1, w2 uint64) {
	return uint64(id.Identity) | uint64(id.Flags)<<32, id.LabelHash, id.UpdatedNs
}
func unpackVal(w0, w1, w2 uint64) abi.PrismIdentity {
	return abi.PrismIdentity{
		Identity: uint32(w0), Flags: uint32(w0 >> 32), LabelHash: w1, UpdatedNs: w2,
	}
}

// NewCompactSink returns an empty inline lock-free-read sink.
func NewCompactSink() *CompactSink {
	s := &CompactSink{}
	s.table.Store(&compactTable{slots: make([]compactSlot, fastInitSize), mask: fastInitSize - 1})
	return s
}

// Lookup is lock-free: seqlock read of an inline slot, retrying only on the rare
// concurrent write to the same slot.
func (s *CompactSink) Lookup(key abi.WorkloadKey) (abi.PrismIdentity, bool, error) {
	t := s.table.Load()
	i := mix(uint64(key)) & t.mask
	for {
		sl := &t.slots[i]
		seq1 := sl.seq.Load()
		if seq1 == 0 {
			return abi.PrismIdentity{}, false, nil // empty slot ends the probe
		}
		k := sl.key.Load()
		if k == uint64(key) && seq1&1 == 0 {
			w0, w1, w2 := sl.w0.Load(), sl.w1.Load(), sl.w2.Load()
			if sl.seq.Load() == seq1 { // no write happened during the read
				return unpackVal(w0, w1, w2), true, nil
			}
			continue // torn read: retry same slot
		}
		i = (i + 1) & t.mask // miss, tombstone, or in-progress: keep probing
	}
}

// Upsert inserts or overwrites under the writer lock, publishing via the seqlock.
func (s *CompactSink) Upsert(key abi.WorkloadKey, id abi.PrismIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.table.Load()
	if (t.used+1)*4 >= len(t.slots)*3 {
		t = s.growLocked(t)
	}
	t.insertLocked(uint64(key), id)
	return nil
}

func (t *compactTable) insertLocked(key uint64, id abi.PrismIdentity) {
	i := mix(key) & t.mask
	firstTomb := -1
	for {
		sl := &t.slots[i]
		seq := sl.seq.Load()
		if seq == 0 { // empty: insert here (or at an earlier tombstone)
			idx := i
			if firstTomb >= 0 {
				idx = uint64(firstTomb)
			} else {
				t.used++
			}
			t.writeSlot(&t.slots[idx], key, id)
			t.live++
			return
		}
		k := sl.key.Load()
		if k == tombKey {
			if firstTomb < 0 {
				firstTomb = int(i)
			}
		} else if k == key { // overwrite in place
			t.writeSlot(sl, key, id)
			return
		}
		i = (i + 1) & t.mask
	}
}

// writeSlot publishes key+value with a seqlock bump. seq is even/0 when stable;
// we go even->odd (write in progress) then odd->even+2 (published), so a reader
// that sees an odd seq or a changed seq across the value read retries.
func (t *compactTable) writeSlot(sl *compactSlot, key uint64, id abi.PrismIdentity) {
	seq := sl.seq.Load() // 0 (empty) or even (stable)
	sl.seq.Store(seq + 1) // odd: write in progress (0->1, 2->3, ...)
	w0, w1, w2 := packVal(id)
	sl.key.Store(key)
	sl.w0.Store(w0)
	sl.w1.Store(w1)
	sl.w2.Store(w2)
	sl.seq.Store(seq + 2) // even: published (0->2, 2->4, ...)
}

// Delete tombstones the slot (idempotent) under the writer lock.
func (s *CompactSink) Delete(key abi.WorkloadKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.table.Load()
	i := mix(uint64(key)) & t.mask
	for {
		sl := &t.slots[i]
		seq := sl.seq.Load()
		if seq == 0 {
			return nil // absent
		}
		if sl.key.Load() == uint64(key) {
			sl.seq.Store(seq + 1) // odd: write-in-progress
			sl.key.Store(tombKey) // mark tombstone (slot stays non-empty)
			sl.seq.Store(seq + 2) // even: published
			t.live--
			return nil
		}
		i = (i + 1) & t.mask
	}
}

// growLocked rebuilds a 2x table dropping tombstones and publishes it atomically.
func (s *CompactSink) growLocked(old *compactTable) *compactTable {
	size := len(old.slots) * 2
	nt := &compactTable{slots: make([]compactSlot, size), mask: uint64(size - 1)}
	for i := range old.slots {
		sl := &old.slots[i]
		if sl.seq.Load() == 0 {
			continue
		}
		k := sl.key.Load()
		if k == tombKey {
			continue
		}
		nt.insertLocked(k, unpackVal(sl.w0.Load(), sl.w1.Load(), sl.w2.Load()))
	}
	s.table.Store(nt)
	return nt
}

// Len returns the live entry count.
func (s *CompactSink) Len() int {
	s.mu.Lock()
	n := s.table.Load().live
	s.mu.Unlock()
	return n
}

// Range iterates every live entry, calling fn for each and stopping early if fn
// returns false. It snapshots the current table and walks the slots, skipping
// empty slots (seq==0) and tombstones (key==tombKey). Each slot is read via the
// seqlock and retried on a torn read, exactly like Lookup, so Range never hands
// fn a half-published value even while the single writer is mid-Upsert.
func (s *CompactSink) Range(fn func(key abi.WorkloadKey, id abi.PrismIdentity) bool) {
	t := s.table.Load()
	for i := range t.slots {
		sl := &t.slots[i]
		for {
			seq1 := sl.seq.Load()
			if seq1 == 0 {
				break // empty slot: nothing here
			}
			if seq1&1 != 0 {
				continue // write in progress: retry until stable
			}
			k := sl.key.Load()
			w0, w1, w2 := sl.w0.Load(), sl.w1.Load(), sl.w2.Load()
			if sl.seq.Load() != seq1 {
				continue // torn read: retry same slot
			}
			if k == tombKey {
				break // tombstone: skip
			}
			if !fn(abi.WorkloadKey(k), unpackVal(w0, w1, w2)) {
				return
			}
			break
		}
	}
}

// Kind reports "sim".
func (s *CompactSink) Kind() string { return "sim" }

// Close drops the table.
func (s *CompactSink) Close() error {
	s.mu.Lock()
	s.table.Store(&compactTable{slots: make([]compactSlot, fastInitSize), mask: fastInitSize - 1})
	s.mu.Unlock()
	return nil
}
