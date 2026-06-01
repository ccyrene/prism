/* SPDX-License-Identifier: GPL-2.0
 *
 * prism_maps.bpf.h — Prism Identity Bus ABI (C side).
 *
 * This is the C twin of pkg/abi/abi.go. struct prism_identity MUST stay
 * byte-identical (little-endian, 24 bytes) with abi.PrismIdentity so a value
 * written by the userspace daemon is read correctly by any BPF consumer.
 *
 * Any BPF program (sched_ext scheduler, tc/XDP net policy, observer) includes
 * this header to share the ONE identity map. That sharing is the bus.
 */
#ifndef __PRISM_MAPS_BPF_H__
#define __PRISM_MAPS_BPF_H__

/* Subsystem facet flags — one identity, per-subsystem bits (3-way composability). */
#define PRISM_FLAG_NET_POLICY    (1u << 0)
#define PRISM_FLAG_SCHED_MANAGED  (1u << 1)
#define PRISM_FLAG_OBSERVED      (1u << 2)

/* --- Scheduling-policy sub-fields packed into `flags` (the "thick" bus) ------
 * The bus carries identity AND a per-identity scheduling-policy class, written
 * by the daemon from a pod label and consumed NATIVELY by any scx scheduler
 * (mapped to that scheduler's own knobs). The operator sets scheduling policy
 * centrally; every consumer agrees on it. This does NOT grow the 24-byte value:
 * the policy is encoded in spare bits of `flags`, DISJOINT from the facet bits.
 *
 *   bits [0:2]   facet flags (PRISM_FLAG_*)        — frozen
 *   bits [3:7]   reserved (MUST be 0)
 *   bits [8:10]  latency class (PRISM_SCHED_CLASS_*)
 *   bits [11:17] weight (0 = unset, else 1..127)
 *   bits [18:31] reserved (MUST be 0)
 *
 * Keep byte-identical with the SchedClass* and EncodeSchedPolicy constants in
 * pkg/abi/abi.go (Go side). */
#define PRISM_SCHED_CLASS_SHIFT     8
#define PRISM_SCHED_CLASS_MASK      (0x7u  << PRISM_SCHED_CLASS_SHIFT)  /* bits 8..10  */
#define PRISM_SCHED_WEIGHT_SHIFT    11
#define PRISM_SCHED_WEIGHT_MASK     (0x7Fu << PRISM_SCHED_WEIGHT_SHIFT) /* bits 11..17 */
#define PRISM_SCHED_WEIGHT_MAX      0x7Fu
#define PRISM_SCHED_WEIGHT_NEUTRAL  64u   /* weight a consumer treats as "1x" */

/* Latency classes (3-bit field). UNSET => no policy: the scheduler uses its own
 * heuristic, so an identity with no class behaves exactly as before this field
 * existed (the thick bus is opt-in and backward compatible). */
#define PRISM_SCHED_CLASS_UNSET     0u
#define PRISM_SCHED_CLASS_CRITICAL  1u
#define PRISM_SCHED_CLASS_NORMAL    2u
#define PRISM_SCHED_CLASS_BATCH     3u
/* 4..7 reserved for future classes. */

/* Compile-time ABI lock: these MUST equal pkg/abi/abi.go's SchedClassMask /
 * SchedWeightMask (also asserted Go-side in pkg/abi/abi_test.go). A drift here
 * fails the build instead of silently corrupting the wire format. */
_Static_assert(PRISM_SCHED_CLASS_MASK  == 0x700u,   "class mask must match abi.SchedClassMask");
_Static_assert(PRISM_SCHED_WEIGHT_MASK == 0x3F800u, "weight mask must match abi.SchedWeightMask");
_Static_assert((PRISM_SCHED_CLASS_MASK & 0x7u) == 0u, "policy fields must not overlap facet bits");

/* 24-bit numeric identity space (Cilium-style). */
#define PRISM_IDENTITY_MASK   0x00FFFFFFu
#define PRISM_ID_MIN_DYNAMIC  256u
#define PRISM_ID_MAX          0x00FFFFFFu

/* Reserved identities (mirror the well-known Cilium reserved range so the
 * identity bus is interoperable across subsystems). */
#define PRISM_ID_UNKNOWN        0u
#define PRISM_ID_HOST           1u
#define PRISM_ID_WORLD          2u
#define PRISM_ID_UNMANAGED      3u
#define PRISM_ID_HEALTH         4u
#define PRISM_ID_INIT           5u
#define PRISM_ID_REMOTE_NODE    6u
#define PRISM_ID_KUBE_APISERVER 7u
#define PRISM_ID_INGRESS        8u

#define PRISM_MAP_MAX_ENTRIES (1u << 20)

/* Per-workload identity value. 24 bytes, must match abi.PrismIdentity. */
struct prism_identity {
	__u32 identity;   /* 24-bit numeric identity (low bits) */
	__u32 flags;      /* PRISM_FLAG_* facet bits             */
	__u64 label_hash; /* FNV-1a/64 of canonical label set    */
	__u64 updated_ns; /* wall-clock ns of last write         */
};
_Static_assert(sizeof(struct prism_identity) == 24,
	       "prism_identity must stay 24 bytes (frozen ABI, must match abi.PrismIdentity)");

/* The bus: one hash map keyed by workload key (cgroup id on a real kernel).
 *
 * INTEGRITY: BPF_F_RDONLY_PROG makes this map READ-ONLY to every BPF program
 * (consumer). The privileged userspace daemon (prismd) is the SOLE writer: its
 * map_update/map_delete *syscalls* are unaffected by RDONLY_PROG — that flag
 * only restricts in-kernel BPF program access to reads. So the kernel verifier
 * itself rejects any consumer that attempts to corrupt an identity, and facet
 * flags are stamped exclusively by the daemon (Controller.Flags). This turns
 * "consumers must not write identities" from a convention into a load-time,
 * verifier-enforced invariant.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, PRISM_MAP_MAX_ENTRIES);
	__type(key, __u64);                  /* workload key = cgroup id */
	__type(value, struct prism_identity);
	__uint(map_flags, BPF_F_RDONLY_PROG); /* read-only to BPF; daemon writes via syscall */
	__uint(pinning, LIBBPF_PIN_BY_NAME); /* /sys/fs/bpf/.../prism_identity */
} prism_identity SEC(".maps");

#endif /* __PRISM_MAPS_BPF_H__ */
