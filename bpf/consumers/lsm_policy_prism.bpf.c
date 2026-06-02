// SPDX-License-Identifier: GPL-2.0
//
// lsm_policy_prism.bpf.c — a SECURITY consumer of the Prism identity bus
// (the "4th leg": sched + net + trace, now LSM enforcement).
//
// The first three legs READ identity and ACT softly — the scheduler prioritizes,
// the net consumer accounts, the tracer attributes. This leg READS the SAME
// shared `prism_identity` map and makes a HARD security decision: it can DENY a
// kernel operation (return -EPERM) based purely on the workload identity prismd
// already resolved. One O(1) map lookup turns "which Kubernetes workload is this"
// into an in-kernel access-control verdict — no userspace round-trip, no policy
// agent in the data path, no /proc or cgroup parsing.
//
// ATTACH POINT: lsm/bprm_check_security — the BPF LSM hook on every execve(),
// fired once the new program image is set up but BEFORE control transfers to it
// (the classic "should this workload be allowed to exec?" gate). It receives the
// linux_binprm and the verdict accumulated by LSMs ahead of us (`ret`); BPF LSM
// programs MUST honor an existing denial and may only ADD a denial — see the
// `ret` handling below. (An alternative socket-egress gate, lsm/socket_connect,
// is sketched at the bottom; same identity read, different object.)
//
// READ-ONLY CONSUMER: the prism_identity map is BPF_F_RDONLY_PROG, so the
// verifier rejects any write to it. This program ONLY READS identity (and a
// daemon-stamped facet); its OWN decision counters live in a SEPARATE, writable
// map (prism_lsm_decisions). It never writes the bus — prismd is the sole writer.
//
// REQUIREMENTS (honest): BPF LSM needs CONFIG_BPF_LSM=y AND "bpf" present in the
// active LSM list (kernel `lsm=` / CONFIG_LSM, see /sys/kernel/security/lsm). If
// "bpf" is absent the program compiles and verifies but cannot attach/enforce.
// On such a host this serves as the design + the load recipe; on a bpf-LSM host
// it enforces. (vmlinux.h must expose struct linux_binprm — it does, ~6.12+.)
//
// ── BUILD ─────────────────────────────────────────────────────────────────────
//   # -I bpf resolves vmlinux.h / prism_maps.bpf.h / libprism.bpf.h;
//   # -I bpf/include picks up the local libbpf shim (<bpf/bpf_helpers.h>) when
//   # libbpf-dev is absent — same flags as scripts/build.sh and the README.
//   clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -I bpf -I bpf/include \
//         -c bpf/consumers/lsm_policy_prism.bpf.c -o lsm_policy_prism.bpf.o
//
// ── LOAD (reusing the already-pinned shared bus, like bpf/consumers/README.md) ──
//   # The bus must already exist: prismd / a Prism scheduler is running and has
//   # pinned /sys/fs/bpf/prism_identity. We REUSE that pinned map (not a fresh
//   # empty copy) so we read the very identities prismd writes. lsm/* programs
//   # auto-attach on load, so `autoattach` wires the hook for us.
//   sudo bpftool prog loadall lsm_policy_prism.bpf.o /sys/fs/bpf/lsm_policy_prism \
//        map name prism_identity pinned /sys/fs/bpf/prism_identity autoattach
//
//   # inspect this consumer's OWN decision counters (allow/deny per identity):
//   sudo bpftool map dump name prism_lsm_de     # (name truncated to 15 chars)
// ────────────────────────────────────────────────────────────────────────────

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include "prism_maps.bpf.h"    // the shared bus map (prism_identity, RDONLY_PROG)
#include "libprism.bpf.h"      // prism_identity_of_current(), prism_id(), ... READERS only

char LICENSE[] SEC("license") = "GPL";   // required: BPF LSM uses GPL-only kernel helpers

#ifndef EPERM
#define EPERM 1                 // "Operation not permitted" — the denial verdict
#endif

// ── THE POLICY INPUTS ───────────────────────────────────────────────────────
//
// (A) A deny-set: numeric workload identities that are forbidden from exec'ing.
//     This is the consumer's OWN map (writable; NOT the shared bus). The operator
//     (or a userspace controller) populates it by identity; we only READ it here
//     to render the verdict. Key = 24-bit identity, value = 1 (present == denied).
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1 << 16);
	__type(key, __u32);     // numeric workload identity from the bus
	__type(value, __u8);    // 1 = deny exec for this identity
} prism_lsm_denyset SEC(".maps");

// (B) The REQUIRED facet: a workload must carry this daemon-stamped facet bit to
//     be allowed to exec. We reuse PRISM_FLAG_OBSERVED as the "vetted by policy"
//     facet — prismd (Controller.Flags) is the sole writer of facets, so a
//     consumer can only READ it (prism_has_flag). Treating "lacks the facet" as a
//     soft signal (audited, not hard-denied) keeps the demo safe; flip
//     PRISM_LSM_REQUIRE_FACET to 1 to make a missing facet a HARD deny.
#define PRISM_LSM_REQUIRED_FACET   PRISM_FLAG_OBSERVED
#define PRISM_LSM_REQUIRE_FACET    0   // 0 = facet is advisory; 1 = enforce it

// ── THIS CONSUMER'S OWN STATE: per-identity decision counters ─────────────────
// Writable (NOT the shared identity map), so RDONLY_PROG does not apply. Lets an
// operator see, per workload, how many execs were allowed vs denied by us.
struct prism_lsm_decision {
	__u64 allowed;
	__u64 denied;
};
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1 << 16);
	__type(key, __u32);                      // numeric identity (0 = off-bus)
	__type(value, struct prism_lsm_decision);
} prism_lsm_decisions SEC(".maps");

// Record an allow/deny against this identity in our own counter map. O(1).
static __always_inline void prism_lsm_count(__u32 id, bool denied)
{
	struct prism_lsm_decision *d = bpf_map_lookup_elem(&prism_lsm_decisions, &id);
	if (d) {
		if (denied)
			__sync_fetch_and_add(&d->denied, 1);
		else
			__sync_fetch_and_add(&d->allowed, 1);
	} else {
		struct prism_lsm_decision init = {};
		if (denied)
			init.denied = 1;
		else
			init.allowed = 1;
		bpf_map_update_elem(&prism_lsm_decisions, &id, &init, BPF_ANY);
	}
}

// ── THE HOOK: gate execve() by workload identity ─────────────────────────────
//
// The kernel invokes a BPF LSM program with a raw __u64[] context: the hook's
// typed arguments followed by the running verdict from LSMs ahead of us. For
// bprm_check_security(struct linux_binprm *bprm, int ret) that is:
//     ctx[0] = (struct linux_binprm *) bprm
//     ctx[1] = (int)                   ret   <- accumulated prior verdict
// (libbpf's BPF_PROG() macro from <bpf/bpf_tracing.h> just sugars this unpack;
// we read ctx[] directly so this stays buildable with the Prism dev-host shim,
// which ships no bpf_tracing.h — same constraint the sibling consumers honor.)
//
// CONTRACT: a BPF LSM hook must NOT override a prior denial — it may only turn an
// allow (0) into a denial. So we short-circuit when ctx[1] is already nonzero,
// and otherwise return either 0 (allow) or -EPERM (deny).
SEC("lsm/bprm_check_security")
int prism_lsm_bprm(unsigned long long *ctx)
{
	struct linux_binprm *bprm = (struct linux_binprm *)ctx[0];
	int ret = (int)ctx[1];
	(void)bprm; // policy here keys on identity, not on the binprm fields (yet)

	// (0) Honor any denial already decided by an LSM ahead of us. Never weaken it.
	if (ret != 0)
		return ret;

	// (1) ONE O(1) read of the shared bus: which workload is exec'ing right now?
	//     prism_identity_of_current() walks to the pod cgroup, so it resolves
	//     across QoS hierarchy depth; NULL/0 for tasks not on the bus.
	struct prism_identity *wid = prism_identity_of_current();
	__u32 id = prism_id(wid);          // PRISM_ID_UNKNOWN (0) if off-bus

	// (2) Reserved infra identities (host, kube-apiserver, init, ...) and off-bus
	//     tasks are NOT subject to per-workload policy — allow them outright. This
	//     keeps node/system execs from ever being gated by a workload deny rule.
	if (id == PRISM_ID_UNKNOWN || prism_id_is_reserved(id)) {
		prism_lsm_count(id, /*denied=*/false);
		return 0;
	}

	// (3) DENY RULE 1 — explicit deny-set: identity present in our deny map.
	__u8 *denied = bpf_map_lookup_elem(&prism_lsm_denyset, &id);
	if (denied && *denied) {
		prism_lsm_count(id, /*denied=*/true);
		return -EPERM;            // forbid this workload from exec'ing
	}

	// (4) DENY RULE 2 — BATCH-class workloads may not exec. The bus carries the
	//     operator's latency-class intent (set by prismd from a pod label); a
	//     BATCH workload is a non-interactive job with no business spawning new
	//     program images, so we lock its exec surface down. (Reading the class is
	//     a pure read of spare bits in wid->flags — see prism_sched_class().)
	if (prism_sched_class(wid) == PRISM_SCHED_CLASS_BATCH) {
		prism_lsm_count(id, /*denied=*/true);
		return -EPERM;
	}

	// (5) DENY RULE 3 (optional) — workload lacks the required, daemon-stamped
	//     facet. Advisory by default (compile-time switch above); when enforced,
	//     a workload prismd has not vetted (no PRISM_FLAG_OBSERVED) cannot exec.
	if (PRISM_LSM_REQUIRE_FACET && !prism_has_flag(wid, PRISM_LSM_REQUIRED_FACET)) {
		prism_lsm_count(id, /*denied=*/true);
		return -EPERM;
	}

	// (6) Default: ALLOW. Account the decision in our own map and return 0.
	prism_lsm_count(id, /*denied=*/false);
	return 0;
}

// ── ALTERNATIVE GATE (design note, not compiled-in) ───────────────────────────
// The exact same identity read drives an egress security policy on the socket
// layer. lsm/socket_connect(struct socket *sock, struct sockaddr *address,
// int addrlen, int ret) lands the prior verdict in ctx[3]; swap the SEC()/body:
//
//   SEC("lsm/socket_connect")
//   int prism_lsm_connect(unsigned long long *ctx)
//   {
//       int ret = (int)ctx[3];                          // ctx[0..2]=sock,addr,addrlen
//       if (ret != 0) return ret;                       // honor prior denial
//       struct prism_identity *wid = prism_identity_of_current();
//       __u32 id = prism_id(wid);
//       if (id == PRISM_ID_UNKNOWN || prism_id_is_reserved(id)) return 0;
//       __u8 *d = bpf_map_lookup_elem(&prism_lsm_denyset, &id);
//       if (d && *d) return -EPERM;                     // denied workload can't connect
//       if (prism_sched_class(wid) == PRISM_SCHED_CLASS_BATCH) return -EPERM;
//       return 0;                                       // allow
//   }
//
// Identity is read identically; only the kernel object being guarded changes —
// that uniformity across legs is exactly what the shared bus buys you.
