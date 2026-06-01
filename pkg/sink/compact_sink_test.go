// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"sync"
	"testing"

	"github.com/prism-bus/prism/pkg/abi"
)

func TestCompactSinkBasic(t *testing.T) {
	s := NewCompactSink()
	if _, ok, _ := s.Lookup(7); ok {
		t.Fatal("empty lookup hit")
	}
	s.Upsert(7, abi.PrismIdentity{Identity: 256, Flags: abi.FlagSchedManaged, LabelHash: 0xABCD, UpdatedNs: 99})
	v, ok, _ := s.Lookup(7)
	if !ok || v.Identity != 256 || v.Flags != abi.FlagSchedManaged || v.LabelHash != 0xABCD || v.UpdatedNs != 99 {
		t.Fatalf("roundtrip wrong: %+v ok=%v", v, ok)
	}
	s.Upsert(7, abi.PrismIdentity{Identity: 300})
	if v, _, _ := s.Lookup(7); v.Identity != 300 || s.Len() != 1 {
		t.Fatalf("overwrite: %+v len=%d", v, s.Len())
	}
	s.Delete(7)
	if _, ok, _ := s.Lookup(7); ok || s.Len() != 0 {
		t.Fatalf("after delete ok=%v len=%d", ok, s.Len())
	}
	s.Delete(7) // idempotent
}

func TestCompactSinkResizeIntegrity(t *testing.T) {
	s := NewCompactSink()
	const n = 60000
	k := func(i int) abi.WorkloadKey { return abi.WorkloadKey(uint64(i)*0x9E3779B97F4A7C15 + 1) }
	for i := 0; i < n; i++ {
		s.Upsert(k(i), abi.PrismIdentity{Identity: uint32(i + 256), LabelHash: uint64(i)})
	}
	if s.Len() != n {
		t.Fatalf("len=%d want %d", s.Len(), n)
	}
	for i := 0; i < n; i++ {
		v, ok, _ := s.Lookup(k(i))
		if !ok || v.Identity != uint32(i+256) || v.LabelHash != uint64(i) {
			t.Fatalf("key %d: %+v ok=%v", i, v, ok)
		}
	}
	for i := 0; i < n; i += 3 {
		s.Delete(k(i))
	}
	for i := 0; i < n; i++ {
		_, ok, _ := s.Lookup(k(i))
		if want := i%3 != 0; ok != want {
			t.Fatalf("key %d present=%v want %v", i, ok, want)
		}
	}
}

// TestCompactSinkConcurrent exercises the seqlock: many readers hammering keys
// while a single writer churns values. Must be race-clean (run with -race) and
// readers must only ever see fully-published values.
func TestCompactSinkConcurrent(t *testing.T) {
	s := NewCompactSink()
	const keys = 1000
	for i := 0; i < keys; i++ {
		s.Upsert(abi.WorkloadKey(i), abi.PrismIdentity{Identity: uint32(i), LabelHash: uint64(i) * 7})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < keys; i++ {
					v, ok, _ := s.Lookup(abi.WorkloadKey(i))
					// Invariant maintained by the writer: LabelHash == Identity*7.
					if ok && v.LabelHash != uint64(v.Identity)*7 {
						t.Errorf("torn read key %d: id=%d hash=%d", i, v.Identity, v.LabelHash)
						return
					}
				}
			}
		}()
	}
	for w := 0; w < 5000; w++ {
		i := w % keys
		id := uint32(i + w)
		s.Upsert(abi.WorkloadKey(i), abi.PrismIdentity{Identity: id, LabelHash: uint64(id) * 7})
	}
	close(stop)
	wg.Wait()
}

var _ abi.Sink = (*CompactSink)(nil)
