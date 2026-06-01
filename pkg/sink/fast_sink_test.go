// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"testing"

	"github.com/prism-bus/prism/pkg/abi"
)

func TestFastSinkBasic(t *testing.T) {
	s := NewFastSink()
	if v, ok, _ := s.Lookup(42); ok {
		t.Fatalf("empty lookup returned %v", v)
	}
	s.Upsert(42, abi.PrismIdentity{Identity: 256})
	v, ok, _ := s.Lookup(42)
	if !ok || v.Identity != 256 {
		t.Fatalf("got %v ok=%v", v, ok)
	}
	// overwrite keeps one live entry
	s.Upsert(42, abi.PrismIdentity{Identity: 999})
	v, _, _ = s.Lookup(42)
	if v.Identity != 999 || s.Len() != 1 {
		t.Fatalf("overwrite: %v len=%d", v, s.Len())
	}
	// delete -> absent, idempotent
	s.Delete(42)
	if _, ok, _ := s.Lookup(42); ok || s.Len() != 0 {
		t.Fatalf("after delete ok=%v len=%d", ok, s.Len())
	}
	s.Delete(42) // idempotent
}

func TestFastSinkResizeAndIntegrity(t *testing.T) {
	s := NewFastSink()
	const n = 50000 // forces several resizes past the 1024 initial table
	for i := 0; i < n; i++ {
		s.Upsert(abi.WorkloadKey(i*2654435761+1), abi.PrismIdentity{Identity: uint32(i + 256)})
	}
	if s.Len() != n {
		t.Fatalf("len=%d want %d", s.Len(), n)
	}
	for i := 0; i < n; i++ {
		v, ok, _ := s.Lookup(abi.WorkloadKey(i*2654435761 + 1))
		if !ok || v.Identity != uint32(i+256) {
			t.Fatalf("key %d: got %v ok=%v", i, v, ok)
		}
	}
	// delete half, ensure survivors intact and deleted absent (tombstone probing)
	for i := 0; i < n; i += 2 {
		s.Delete(abi.WorkloadKey(i*2654435761 + 1))
	}
	if s.Len() != n/2 {
		t.Fatalf("after half-delete len=%d want %d", s.Len(), n/2)
	}
	for i := 0; i < n; i++ {
		_, ok, _ := s.Lookup(abi.WorkloadKey(i*2654435761 + 1))
		wantPresent := i%2 == 1
		if ok != wantPresent {
			t.Fatalf("key %d present=%v want %v", i, ok, wantPresent)
		}
	}
	// re-insert deleted keys (reuses tombstones)
	for i := 0; i < n; i += 2 {
		s.Upsert(abi.WorkloadKey(i*2654435761+1), abi.PrismIdentity{Identity: uint32(i + 256)})
	}
	if s.Len() != n {
		t.Fatalf("after reinsert len=%d want %d", s.Len(), n)
	}
}

var _ abi.Sink = (*FastSink)(nil)
