/* SPDX-License-Identifier: GPL-2.0
 *
 * libprism.bpf.h — Prism Identity Bus, BPF-side helper library.
 *
 * THIS is the small API that every Prism BPF consumer includes to talk to the
 * shared identity map: the sched_ext scheduler (scx_prism.bpf.c), the network
 * policy hook and the observer (compose_demo.bpf.c). All of them key off the
 * SAME `prism_identity` map declared in prism_maps.bpf.h — that shared lookup
 * is the "bus". A consumer never re-declares the map or re-implements the
 * lookup/flag dance; it includes this header and calls these inline helpers.
 *
 * Layering (all headers are BPF-side, compiled with clang -target bpf):
 *     vmlinux.h            -- kernel types from BTF
 *     <bpf/bpf_helpers.h>  -- SEC(), __uint(), helper prototypes (libbpf)
 *     prism_maps.bpf.h     -- struct prism_identity + the prism_identity map
 *     libprism.bpf.h       -- (this file) the consumer-facing helpers
 *
 * The map value is written by the userspace daemon (Go side, abi.PrismIdentity)
 * and read here; the struct layout is the frozen ABI and must not drift.
 *
 * INTEGRITY MODEL — CONSUMERS ARE READ-ONLY: the `prism_identity` map carries
 * BPF_F_RDONLY_PROG (see prism_maps.bpf.h), so the kernel verifier rejects any
 * BPF program that tries to map_update/map_delete it. The privileged daemon is
 * the SOLE writer (its map syscalls are not subject to RDONLY_PROG). Therefore
 * this library exposes ONLY readers — prism_lookup / prism_current_key /
 * prism_identity_of_current / prism_has_flag / prism_id / prism_id_is_reserved.
 * There is deliberately NO prism_set_flag: facet flags (PRISM_FLAG_*) are
 * stamped by the daemon (Controller.Flags), never by a consumer. A consumer
 * that branches on a facet uses prism_has_flag() to READ it.
 *
 * Everything is `static __always_inline`: a header-only library means each
 * consumer program gets its own inlined copy with no cross-object linkage and
 * no function calls the verifier has to reason about across boundaries.
 */
#ifndef __LIBPRISM_BPF_H__
#define __LIBPRISM_BPF_H__

#include "prism_maps.bpf.h" /* struct prism_identity + `prism_identity` map */

/*
 * prism_lookup() — fetch the identity value for an explicit workload key.
 *
 * @key: the workload key. On a real kernel this is a cgroup id (the same value
 *       the userspace daemon used as the map key). Returns a pointer directly
 *       into the map value (safe to read; write only via map_update) or NULL if
 *       the workload is unknown to the bus (e.g. host/unmanaged traffic).
 *
 * Callers MUST null-check the result — an unknown key is the common case for
 * the host itself and for anything the daemon has not classified yet.
 */
static __always_inline struct prism_identity *prism_lookup(__u64 key)
{
	return bpf_map_lookup_elem(&prism_identity, &key);
}

/*
 * prism_current_key() — the LEAF cgroup id of the current task.
 *
 * Wraps bpf_get_current_cgroup_id(). Valid in any context that has a current
 * task (enqueue, syscalls, most tracepoints, cgroup-skb). It is NOT valid in
 * hard-irq / NMI context — callers in such contexts must derive the key some
 * other way. Returns 0 if there is no current cgroup association.
 *
 * CAVEAT (leaf vs pod): bpf_get_current_cgroup_id() returns the CONTAINER
 * (leaf) cgroup id. The daemon, however, keys the identity map on the
 * POD-level cgroup id (one identity per pod, shared by its containers). On a
 * deep hierarchy (kubepods/<qos>/pod<uid>/<container>) the leaf id is NOT the
 * pod id, so a raw prism_lookup(prism_current_key()) can MISS. This helper is
 * kept as a primitive and as the level-0 probe; consumers should resolve
 * identity through prism_identity_of_current(), which walks up to the pod
 * level. See that function for why.
 */
static __always_inline __u64 prism_current_key(void)
{
	return bpf_get_current_cgroup_id();
}

/*
 * PRISM_ANCESTOR_PROBE_LEVELS — how many cgroup-hierarchy levels above the leaf
 * we probe when resolving the pod-level key. The cgroup v2 path for a pod looks
 * like, from the v2 root (level 0) down:
 *
 *   /                                   level 0  (root)
 *   /kubepods.slice                     level 1
 *   /kubepods.slice/<qos>.slice         level 2  (burstable / besteffort)
 *   /kubepods.slice/<qos>/pod<uid>      level 3  <-- POD cgroup (daemon's key)
 *   /.../pod<uid>/<container-id>        level 4  (container, the leaf)
 *
 * but the DEPTH VARIES BY QoS: a *guaranteed* pod has NO per-QoS .slice level,
 * so its pod cgroup sits one level shallower than a burstable/besteffort pod.
 * cgroup driver (systemd vs cgroupfs) can shift it too. We therefore cannot
 * hardcode "the pod is N levels up". A small fixed window of levels covers
 * every real layout (guaranteed ~3, burstable/besteffort ~4, plus headroom).
 */
#define PRISM_ANCESTOR_PROBE_LEVELS 8

/*
 * prism_identity_of_current() — ROBUST resolution of the current task's
 * workload identity, independent of cgroup hierarchy depth.
 *
 * WHY THIS IS NOT JUST prism_lookup(prism_current_key()): the daemon keys the
 * map on the POD cgroup id, but the running task lives in the deeper CONTAINER
 * (leaf) cgroup, and the number of levels between them depends on the pod's QoS
 * class (guaranteed vs burstable/besteffort) and the cgroup driver — see
 * prism_current_key()'s caveat and PRISM_ANCESTOR_PROBE_LEVELS above. So we:
 *
 *   1. Try the leaf id first (covers any setup where the daemon happens to key
 *      on the leaf, and is the cheapest probe).
 *   2. Otherwise walk a BOUNDED set of ancestor levels via
 *      bpf_get_current_ancestor_cgroup_id(level) and return the FIRST level
 *      whose cgroup id is present in the map.
 *
 * Because ONLY pod-level keys are ever written to the bus, the first ancestor
 * that hits is exactly the pod cgroup — the walk self-resolves to the pod level
 * regardless of how deep the leaf is. The walk is MANUALLY UNROLLED via the
 * PRISM_ANCESTOR_PROBE() macro over a fixed list of levels, so the compiled
 * program is straight-line code with a constant number of map lookups and NO
 * backedge — verifier-safe by construction (no reliance on the optimizer
 * honoring an unroll pragma). Returns NULL for unmanaged tasks (nothing on the
 * bus at any level).
 */
static __always_inline struct prism_identity *prism_identity_of_current(void)
{
	struct prism_identity *id;
	__u64 akey;

	/* Level-0 probe: the leaf cgroup id (cheapest, covers leaf-keyed setups). */
	id = prism_lookup(prism_current_key());
	if (id)
		return id;

	/* Probe one ancestor level: fetch its cgroup id; if present, look it up and
	 * return on the first hit (== pod level, since only pod keys exist). A zero
	 * akey means the task has no ancestor at this level — skip it. */
#define PRISM_ANCESTOR_PROBE(level)                                    \
	do {                                                           \
		akey = bpf_get_current_ancestor_cgroup_id((level));    \
		if (akey) {                                            \
			id = prism_lookup(akey);                       \
			if (id)                                        \
				return id;                             \
		}                                                      \
	} while (0)

	/* Manually unrolled, nearest-the-leaf first, over the bounded window
	 * [1 .. PRISM_ANCESTOR_PROBE_LEVELS]. Keep this list in sync with that
	 * bound. (vmlinux.h does not define NULL, so the miss path returns the
	 * canonical (void *)0 BPF idiom.) */
	PRISM_ANCESTOR_PROBE(8);
	PRISM_ANCESTOR_PROBE(7);
	PRISM_ANCESTOR_PROBE(6);
	PRISM_ANCESTOR_PROBE(5);
	PRISM_ANCESTOR_PROBE(4);
	PRISM_ANCESTOR_PROBE(3);
	PRISM_ANCESTOR_PROBE(2);
	PRISM_ANCESTOR_PROBE(1);
#undef PRISM_ANCESTOR_PROBE

	return (struct prism_identity *)0; /* unmanaged: not on the bus at any level */
}

/*
 * prism_id() — extract the canonical 24-bit numeric identity from a value.
 *
 * The identity space is Cilium-style 24-bit (see PRISM_IDENTITY_MASK); the
 * stored u32 keeps the value in its low bits. Masking here guarantees every
 * subsystem agrees on the same numeric identity regardless of any future use
 * of the upper byte. Returns PRISM_ID_UNKNOWN (0) for a NULL value so callers
 * can treat "no entry" and "explicitly unknown" uniformly.
 */
static __always_inline __u32 prism_id(const struct prism_identity *id)
{
	if (!id)
		return PRISM_ID_UNKNOWN;
	return id->identity & PRISM_IDENTITY_MASK;
}

/*
 * prism_has_flag() — test a per-subsystem facet bit on an identity value.
 *
 * @id:   value from prism_lookup() / prism_identity_of_current() (may be NULL).
 * @flag: one of PRISM_FLAG_NET_POLICY / SCHED_MANAGED / OBSERVED.
 *
 * Returns false for a NULL value. This is how one subsystem observes that
 * another subsystem has already acted on the same identity (e.g. the observer
 * can tell whether the scheduler is managing a task) — composability via a
 * single shared value.
 */
static __always_inline bool prism_has_flag(const struct prism_identity *id,
					   __u32 flag)
{
	if (!id)
		return false;
	return (id->flags & flag) != 0;
}

/*
 * prism_id_is_reserved() — true if @id is in the well-known reserved range
 * [PRISM_ID_UNKNOWN .. PRISM_ID_INGRESS]. Reserved identities (host, world,
 * kube-apiserver, ...) are fixed across the cluster and usually exempt from
 * per-workload policy/scheduling treatment, so consumers special-case them.
 */
static __always_inline bool prism_id_is_reserved(__u32 id)
{
	return id < PRISM_ID_MIN_DYNAMIC;
}

/*
 * prism_sched_class() — extract the per-identity latency class (thick bus).
 *
 * The bus carries not just WHICH workload this is (the numeric identity) but
 * HOW it should be scheduled — a latency class the daemon derived from a pod
 * label. A scheduler maps the class to its OWN knobs (e.g. bpfland's
 * deadline/slice-lag): the bus dictates the operator's intent, not a mechanism.
 *
 * Returns PRISM_SCHED_CLASS_UNSET for a NULL value or an identity the daemon
 * gave no policy, so a consumer treats "off-bus", "unmanaged" and "no explicit
 * policy" uniformly as "use my own heuristic" — the field is strictly opt-in.
 */
static __always_inline __u32 prism_sched_class(const struct prism_identity *id)
{
	if (!id)
		return PRISM_SCHED_CLASS_UNSET;
	return (id->flags & PRISM_SCHED_CLASS_MASK) >> PRISM_SCHED_CLASS_SHIFT;
}

/*
 * prism_sched_weight() — extract the per-identity scheduling weight (0 = unset).
 *
 * A 7-bit relative weight (1..127; PRISM_SCHED_WEIGHT_NEUTRAL == "1x"). It is an
 * OPTIONAL refinement on top of the class — a consumer that ignores it loses no
 * correctness. Returns 0 (unset) for a NULL value.
 */
static __always_inline __u32 prism_sched_weight(const struct prism_identity *id)
{
	if (!id)
		return 0;
	return (id->flags & PRISM_SCHED_WEIGHT_MASK) >> PRISM_SCHED_WEIGHT_SHIFT;
}

/*
 * NOTE: there is intentionally NO prism_set_flag() here. The `prism_identity`
 * map is BPF_F_RDONLY_PROG, so the verifier rejects any consumer write. The
 * daemon (Controller.Flags) is the sole writer of facet flags; consumers READ
 * them with prism_has_flag(). See the file header (INTEGRITY MODEL).
 */

#endif /* __LIBPRISM_BPF_H__ */
