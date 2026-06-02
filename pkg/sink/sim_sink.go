// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"sync"

	"github.com/ccyrene/prism/pkg/abi"
)

// SimSink is a userspace simulation of the pinned BPF identity map: a single
// mutex-guarded map[uint64]abi.PrismIdentity. It is the sink that runs on hosts
// without a loadable BPF map (this 5.15 WSL2 box, unprivileged CI) and the sink
// the benchmarks drive, so the propagation-latency numbers measure the daemon's
// control path rather than kernel syscall overhead.
//
// abi.PrismIdentity is a fixed-size value (24 bytes, no pointers), so storing it
// by value keeps the hot path allocation-free: Upsert/Lookup copy the struct in
// and out of the map without touching the heap.
type SimSink struct {
	mu sync.RWMutex
	m  map[abi.WorkloadKey]abi.PrismIdentity
}

// NewSimSink returns an empty simulation sink ready for use.
func NewSimSink() *SimSink {
	return &SimSink{m: make(map[abi.WorkloadKey]abi.PrismIdentity)}
}

// Upsert inserts or overwrites the identity for key. Value-typed map store, so
// no allocation occurs once the bucket exists.
func (s *SimSink) Upsert(key abi.WorkloadKey, id abi.PrismIdentity) error {
	s.mu.Lock()
	s.m[key] = id
	s.mu.Unlock()
	return nil
}

// Delete removes key. Deleting a missing key is a no-op (idempotent), matching
// the BPF sink which tolerates ErrKeyNotExist on delete.
func (s *SimSink) Delete(key abi.WorkloadKey) error {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
	return nil
}

// Lookup returns the identity for key and whether it was present. The returned
// struct is a copy; callers cannot mutate map state through it.
func (s *SimSink) Lookup(key abi.WorkloadKey) (abi.PrismIdentity, bool, error) {
	s.mu.RLock()
	id, ok := s.m[key]
	s.mu.RUnlock()
	return id, ok, nil
}

// Len returns the number of identities currently held.
func (s *SimSink) Len() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

// Range iterates every live entry under the read lock, calling fn for each and
// stopping early if fn returns false. The lock is held for the whole walk, so fn
// must not call back into the sink (it would deadlock on the write path).
func (s *SimSink) Range(fn func(key abi.WorkloadKey, id abi.PrismIdentity) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.m {
		if !fn(k, v) {
			return
		}
	}
}

// Kind reports the sink implementation: "sim".
func (s *SimSink) Kind() string { return "sim" }

// Close releases the backing map. The SimSink is unusable afterwards.
func (s *SimSink) Close() error {
	s.mu.Lock()
	s.m = nil
	s.mu.Unlock()
	return nil
}
