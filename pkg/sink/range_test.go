// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"sync"
	"testing"

	"github.com/ccyrene/prism/pkg/abi"
)

// All four sinks implement abi.Sink, including the new Range method. The BPF
// sink can't be exercised on this host (no bpffs/CAP_BPF) but must still satisfy
// the interface at compile time.
var (
	_ abi.Sink = (*SimSink)(nil)
	_ abi.Sink = (*FastSink)(nil)
	_ abi.Sink = (*CompactSink)(nil)
	_ abi.Sink = (*BPFSink)(nil)
)

// collect drains a sink via Range into a map for order-independent assertions.
func collect(s abi.Sink) map[abi.WorkloadKey]abi.PrismIdentity {
	out := make(map[abi.WorkloadKey]abi.PrismIdentity)
	s.Range(func(k abi.WorkloadKey, v abi.PrismIdentity) bool {
		out[k] = v
		return true
	})
	return out
}

// rangeContract is the shared behavioural test every userspace sink must pass.
func rangeContract(t *testing.T, s abi.Sink) {
	t.Helper()

	// Empty sink: Range visits nothing.
	if got := len(collect(s)); got != 0 {
		t.Fatalf("empty Range visited %d entries, want 0", got)
	}

	want := map[abi.WorkloadKey]abi.PrismIdentity{
		1:    {Identity: 256, Flags: abi.FlagNetPolicy, LabelHash: 0x11, UpdatedNs: 100},
		2:    {Identity: 257, LabelHash: 0x22, UpdatedNs: 200},
		9999: {Identity: 4242, Flags: abi.FlagSchedManaged, LabelHash: 0x33, UpdatedNs: 300},
	}
	for k, v := range want {
		if err := s.Upsert(k, v); err != nil {
			t.Fatalf("upsert %d: %v", k, err)
		}
	}

	got := collect(s)
	if len(got) != len(want) {
		t.Fatalf("Range visited %d entries, want %d", len(got), len(want))
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Fatalf("Range missed key %d", k)
		}
		if gv != wv {
			t.Fatalf("key %d: Range yielded %+v, want %+v", k, gv, wv)
		}
	}

	// Range must skip a deleted (tombstoned) entry and still yield the survivors.
	if err := s.Delete(2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got = collect(s)
	if len(got) != 2 {
		t.Fatalf("after delete Range visited %d, want 2", len(got))
	}
	if _, ok := got[2]; ok {
		t.Fatalf("Range yielded deleted key 2")
	}

	// Early stop: returning false after the first entry visits exactly one.
	visited := 0
	s.Range(func(abi.WorkloadKey, abi.PrismIdentity) bool {
		visited++
		return false
	})
	if visited != 1 {
		t.Fatalf("early-stop Range visited %d entries, want 1", visited)
	}
}

func TestSimSinkRange(t *testing.T)     { rangeContract(t, NewSimSink()) }
func TestFastSinkRange(t *testing.T)    { rangeContract(t, NewFastSink()) }
func TestCompactSinkRange(t *testing.T) { rangeContract(t, NewCompactSink()) }

// TestRangeAfterResize checks Range sees every survivor after the table has
// grown past its initial size and half the keys have been tombstoned — the
// resize/tombstone interplay the Range walk must get right.
func TestRangeAfterResize(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    abi.Sink
	}{
		{"fast", NewFastSink()},
		{"compact", NewCompactSink()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 5000 // forces several resizes past the 1024 initial table
			k := func(i int) abi.WorkloadKey { return abi.WorkloadKey(uint64(i)*0x9E3779B97F4A7C15 + 1) }
			for i := 0; i < n; i++ {
				if err := tc.s.Upsert(k(i), abi.PrismIdentity{Identity: uint32(i + 256)}); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < n; i += 2 { // tombstone half
				if err := tc.s.Delete(k(i)); err != nil {
					t.Fatal(err)
				}
			}
			got := collect(tc.s)
			if len(got) != n/2 {
				t.Fatalf("Range after resize+half-delete visited %d, want %d", len(got), n/2)
			}
			for i := 1; i < n; i += 2 {
				if v, ok := got[k(i)]; !ok || v.Identity != uint32(i+256) {
					t.Fatalf("survivor %d missing/wrong: %+v ok=%v", i, v, ok)
				}
			}
		})
	}
}

// TestCompactSinkRangeConcurrent exercises Range's seqlock retry against a live
// writer: a survivor's invariant (LabelHash == Identity*7) must never be torn.
// Run under -race.
func TestCompactSinkRangeConcurrent(t *testing.T) {
	s := NewCompactSink()
	const keys = 500
	for i := 0; i < keys; i++ {
		s.Upsert(abi.WorkloadKey(i), abi.PrismIdentity{Identity: uint32(i), LabelHash: uint64(i) * 7})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.Range(func(_ abi.WorkloadKey, v abi.PrismIdentity) bool {
				if v.LabelHash != uint64(v.Identity)*7 {
					t.Errorf("torn Range read: id=%d hash=%d", v.Identity, v.LabelHash)
					return false
				}
				return true
			})
		}
	}()
	for w := 0; w < 5000; w++ {
		i := w % keys
		id := uint32(i + w)
		s.Upsert(abi.WorkloadKey(i), abi.PrismIdentity{Identity: id, LabelHash: uint64(id) * 7})
	}
	close(stop)
	wg.Wait()
}
