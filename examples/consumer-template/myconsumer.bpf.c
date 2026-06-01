// SPDX-License-Identifier: GPL-2.0
//
// myconsumer.bpf.c — TEMPLATE: write YOUR OWN Prism identity-bus consumer.
//
// Copy this file, rename it, and change the SEC() attach point + the logic. The
// pattern is ALWAYS the same, in three moves:
//
//   1. #include the two Prism headers (the frozen ABI + the reader library).
//   2. READ the workload identity off the shared bus with one O(1) lookup.
//   3. Do whatever you want — keep YOUR state in YOUR OWN (writable) map.
//
// You CANNOT write the shared `prism_identity` map: it is created
// BPF_F_RDONLY_PROG, so the verifier rejects any program that tries. That is the
// point — it is what makes it safe to plug arbitrary third-party programs onto
// the same bus. `prismd` is the sole writer of identities; you are a reader.
//
// This example counts file opens per workload identity (a tracepoint on
// sys_enter_openat). Swap the SEC() and body for your subsystem: cgroup_skb for
// packets, kprobe/fentry for kernel calls, sched tracepoints, LSM, XDP, etc.
//
// Build + load: see bpf/consumers/README.md (this template is referenced there).

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include "prism_maps.bpf.h"   // the shared bus: struct prism_identity + the map (READ-ONLY)
#include "libprism.bpf.h"     // readers only: prism_identity_of_current(), prism_id(), ...

char LICENSE[] SEC("license") = "GPL";   // required — BPF readers use GPL-only kernel helpers

// ---- YOUR OWN state -------------------------------------------------------
// A writable map keyed by the 24-bit workload identity. The shared bus stays
// read-only; consumer state lives in maps the consumer owns (cf. the net
// consumer's prism_net_stats in bpf/consumers/net_policy_prism.bpf.c).
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1 << 16);
	__type(key, __u32);     // numeric workload identity (0 = PRISM_ID_UNKNOWN / off-bus)
	__type(value, __u64);   // your value — here, a per-identity open count
} my_open_counts SEC(".maps");

SEC("tracepoint/syscalls/sys_enter_openat")
int myconsumer_openat(void *ctx)
{
	// (2) ONE O(1) read of the shared bus: which workload is running right now?
	//     prism_identity_of_current() walks to the pod cgroup so it resolves
	//     regardless of QoS depth; returns NULL for tasks not on the bus.
	struct prism_identity *wid = prism_identity_of_current();
	__u32 id = prism_id(wid);          // 0 (PRISM_ID_UNKNOWN) if off-bus

	// (You can also branch on a daemon-set facet, e.g.:
	//    if (prism_has_flag(wid, PRISM_FLAG_OBSERVED)) { ... }
	//  or read the per-identity scheduling class out of wid->flags — readers only.)

	// (3) update YOUR map. Never touch prism_identity (RDONLY_PROG).
	__u64 *n = bpf_map_lookup_elem(&my_open_counts, &id);
	if (n) {
		__sync_fetch_and_add(n, 1);
	} else {
		__u64 one = 1;
		bpf_map_update_elem(&my_open_counts, &id, &one, BPF_ANY);
	}
	return 0;
}
