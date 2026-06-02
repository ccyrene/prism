// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/cilium/ebpf"

	"github.com/ccyrene/prism/pkg/abi"
)

// bpfPinDir is the directory component of abi.PinPath. The map is pinned BY NAME
// (LIBBPF_PIN_BY_NAME) under this directory, matching the C side's pinning
// declaration in bpf/prism_maps.bpf.h. cilium/ebpf pins at PinPath/<Name>.
var bpfPinDir = filepath.Dir(abi.PinPath)

// bpfFRdonlyProg is BPF_F_RDONLY_PROG (uapi/linux/bpf.h). It makes the map
// READ-ONLY to BPF programs (consumers): the kernel verifier rejects any
// consumer that attempts to write the identity map. Userspace map syscalls
// (this daemon's Upsert/BatchUpsert/Delete below) are NOT subject to this flag,
// so prismd remains the sole writer — exactly the integrity model we want.
//
// We use the literal value because cilium/ebpf only exposes this constant from
// its internal `sys` package (sys.BPF_F_RDONLY_PROG = 128), which is not
// importable here. The MapSpec.Flags field is the public surface and takes the
// raw flag. NOTE: on this 5.15 dev host the spec is only built/verified (BPF
// mode is unavailable, so we fall back to the sim sink); full kernel
// enforcement happens at map-load time on a 6.12+ host.
const bpfFRdonlyProg uint32 = 0x80 // BPF_F_RDONLY_PROG

// BPFSink writes identities into the real, kernel-resident prism_identity hash
// map via cilium/ebpf. Any BPF consumer (sched_ext, tc, observer) that opens the
// same pinned map sees these writes — that shared map IS the bus.
//
// Creating it requires CAP_BPF/root and a mounted bpffs, so NewBPFSink returns
// an error on hosts that lack either (e.g. this 5.15 WSL2 box). The factory in
// sink.go uses that error to fall back to the simulation sink.
type BPFSink struct {
	m *ebpf.Map
}

// NewBPFSink creates (or re-uses, if compatible) the pinned prism_identity map
// and returns a Sink backed by it. It returns an error — never panics — when the
// map cannot be created so the caller can degrade to the sim sink.
//
// The MapSpec is the exact ABI twin of the C definition: BPF_MAP_TYPE_HASH,
// key u64 (cgroup id / workload key), value struct prism_identity (24 bytes),
// max 1<<20 entries, pinned by name. The key/value sizes are derived from the
// Go types via unsafe.Sizeof so they can never drift from the ABI struct.
func NewBPFSink() (abi.Sink, error) {
	// Ensure the bpffs pin directory exists. On a host without bpffs mounted
	// this MkdirAll (or the later create) is what fails, which is the signal
	// the factory falls back on.
	if err := os.MkdirAll(bpfPinDir, 0o755); err != nil {
		return nil, fmt.Errorf("bpf sink: prepare pin dir %q: %w", bpfPinDir, err)
	}

	spec := &ebpf.MapSpec{
		Name:       abi.MapName,
		Type:       ebpf.Hash,
		KeySize:    uint32(unsafe.Sizeof(abi.WorkloadKey(0))),     // u64 cgroup id
		ValueSize:  uint32(unsafe.Sizeof(abi.PrismIdentity{})),    // 24-byte value
		MaxEntries: uint32(abi.MaxEntries),
		// Read-only to BPF programs (consumers); daemon writes via syscall. This
		// is the Go twin of the C side's __uint(map_flags, BPF_F_RDONLY_PROG) in
		// bpf/prism_maps.bpf.h — both declare the same integrity invariant.
		Flags:   bpfFRdonlyProg,
		Pinning: ebpf.PinByName,
	}

	m, err := ebpf.NewMapWithOptions(spec, ebpf.MapOptions{PinPath: bpfPinDir})
	if err != nil {
		// Most common cause here: EPERM (needs root/CAP_BPF, unprivileged BPF
		// disabled) or bpffs not mounted. Surface it verbatim for the fallback
		// log so the operator can see why BPF mode was unavailable.
		return nil, fmt.Errorf("bpf sink: create/pin map %q: %w", abi.MapName, err)
	}
	return &BPFSink{m: m}, nil
}

// Upsert writes id for key, creating or overwriting the entry (BPF_ANY).
func (s *BPFSink) Upsert(key abi.WorkloadKey, id abi.PrismIdentity) error {
	if err := s.m.Update(key, id, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("bpf sink: upsert key %d: %w", key, err)
	}
	return nil
}

// BatchUpsert writes many identities in a SINGLE bpf(BPF_MAP_UPDATE_BATCH)
// syscall instead of one syscall per entry. The daemon uses it for the initial
// list-sync (potentially thousands of pods at once), collapsing N syscalls to
// ceil(N/batch) and cutting cold-start propagation time dramatically. keys and
// ids must be the same length; returns the number of entries written.
func (s *BPFSink) BatchUpsert(keys []abi.WorkloadKey, ids []abi.PrismIdentity) (int, error) {
	if len(keys) != len(ids) {
		return 0, fmt.Errorf("bpf sink: batch len mismatch keys=%d ids=%d", len(keys), len(ids))
	}
	if len(keys) == 0 {
		return 0, nil
	}
	n, err := s.m.BatchUpdate(keys, ids, nil)
	if err != nil {
		return n, fmt.Errorf("bpf sink: batch upsert (%d entries): %w", len(keys), err)
	}
	return n, nil
}

// Delete removes key. A missing key is treated as success (idempotent delete),
// mirroring the sim sink and tolerating races where a delete arrives twice.
func (s *BPFSink) Delete(key abi.WorkloadKey) error {
	if err := s.m.Delete(key); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("bpf sink: delete key %d: %w", key, err)
	}
	return nil
}

// Lookup returns the identity for key and whether it was present.
func (s *BPFSink) Lookup(key abi.WorkloadKey) (abi.PrismIdentity, bool, error) {
	var out abi.PrismIdentity
	if err := s.m.Lookup(key, &out); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return abi.PrismIdentity{}, false, nil
		}
		return abi.PrismIdentity{}, false, fmt.Errorf("bpf sink: lookup key %d: %w", key, err)
	}
	return out, true, nil
}

// Len counts live entries by iterating the map. BPF hash maps expose no O(1)
// size, so this is O(n); it is intended for diagnostics, not the hot path.
func (s *BPFSink) Len() int {
	var (
		key   abi.WorkloadKey
		value abi.PrismIdentity
		n     int
	)
	it := s.m.Iterate()
	for it.Next(&key, &value) {
		n++
	}
	return n
}

// Range iterates every live entry via the kernel map's iterator, calling fn for
// each (key, value) and stopping early if fn returns false. BPF hash-map
// iteration is a snapshot-ish best effort under concurrent mutation; the daemon
// is the single writer and only calls Range at startup (reseed) and on the GC
// ticker, so concurrent writes are not a concern here.
func (s *BPFSink) Range(fn func(key abi.WorkloadKey, id abi.PrismIdentity) bool) {
	var (
		key   abi.WorkloadKey
		value abi.PrismIdentity
	)
	it := s.m.Iterate()
	for it.Next(&key, &value) {
		if !fn(key, value) {
			return
		}
	}
}

// Kind reports the sink implementation: "bpf".
func (s *BPFSink) Kind() string { return "bpf" }

// Close detaches the userspace handle. The map stays pinned in bpffs so BPF
// consumers keep their view across daemon restarts — unpinning would tear the
// bus down, which is not what a graceful daemon shutdown wants.
func (s *BPFSink) Close() error {
	return s.m.Close()
}
