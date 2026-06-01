// SPDX-License-Identifier: GPL-2.0
//
// net_policy_prism.bpf.c — a NETWORK consumer of the Prism identity bus.
//
// This is the NET facet of the bus, and the second domain (after the execsnoop
// TRACE consumer) proving the same identity is read by an independent subsystem.
// A cgroup-skb program runs per packet on a cgroup's sockets; unlike a raw
// tracepoint it has an `skb` whose cgroup association we can resolve, so it can
// look the workload identity up on the SAME shared `prism_identity` map the
// scheduler and the tracer read — and apply identity-keyed network policy
// in-kernel with no userspace round-trip and no separate pod-watching.
//
// COMPOSABILITY BOUNDARY (honest): cgroup-skb is SOCKET-level — the skb carries
// its cgroup, so identity resolves here via bpf_skb_ancestor_cgroup_id() (the
// pod-ancestor, walked across QoS depth like libprism's current-task helper).
// Raw XDP / tc-ingress runs before socket demux with no cgroup association, so
// there identity must be CARRIED in the packet (e.g. VXLAN VNI / ipcache, as
// Cilium does) rather than looked up locally. This program is the easy,
// drop-in-ish case; the carried-identity case is noted as design, not done.
//
// READ-ONLY CONSUMER: the prism_identity map is BPF_F_RDONLY_PROG, so this
// program cannot write it (the verifier enforces that). It only READS identity;
// its own per-identity counters live in a SEPARATE, writable map.
//
// Compiles on this host (clang -target bpf, vmlinux.h + minimal shim). On a real
// host it builds against system libbpf and the prism_identity map resolves
// (LIBBPF_PIN_BY_NAME) to the very map prismd pins.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include "prism_maps.bpf.h"  // the shared bus map (prism_identity, RDONLY_PROG)
#include "libprism.bpf.h"    // prism_lookup(), prism_id() — READERS only

char LICENSE[] SEC("license") = "GPL";

// This consumer's OWN state: per-identity packet/byte counters. Writable — it is
// NOT the shared identity map, so RDONLY_PROG does not apply.
struct net_stat {
	__u64 packets;
	__u64 bytes;
};
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1 << 16);
	__type(key, __u32);            // numeric identity
	__type(value, struct net_stat);
} prism_net_stats SEC(".maps");

// Pod cgroups sit a few levels below the v2 root; depth varies with QoS class
// (guaranteed vs burstable/besteffort). Probe a bounded ancestor range and take
// the first level whose id is on the bus — only pod-level keys exist there, so
// it resolves at the pod regardless of depth. Bounded loop = verifier-safe.
#define PRISM_NET_MIN_ANCESTOR 2
#define PRISM_NET_MAX_ANCESTOR 6

static __always_inline struct prism_identity *prism_lookup_skb(struct __sk_buff *skb)
{
	// Fast path: the skb's own cgroup id (works if the workload's cgroup is the
	// pod cgroup, e.g. single-container pods on some layouts).
	struct prism_identity *v = prism_lookup(bpf_skb_cgroup_id(skb));
	if (v)
		return v;
	// Otherwise walk a bounded set of ancestor levels to find the pod cgroup.
#pragma unroll
	for (int lvl = PRISM_NET_MIN_ANCESTOR; lvl <= PRISM_NET_MAX_ANCESTOR; lvl++) {
		__u64 id = bpf_skb_ancestor_cgroup_id(skb, lvl);
		if (!id)
			continue;
		v = prism_lookup(id);
		if (v)
			return v;
	}
	return 0;
}

// cgroup_skb/egress: per outbound packet, attribute it to a workload identity
// from the bus and account it. A real policy would also drop/allow/mark by
// identity; here we account and ALLOW (return 1). The per-packet cost is one
// shared-map lookup (a few ns) — see bench/native/net_overhead.c.
SEC("cgroup_skb/egress")
int prism_net_egress(struct __sk_buff *skb)
{
	struct prism_identity *wid = prism_lookup_skb(skb);
	__u32 nid = prism_id(wid); // PRISM_ID_UNKNOWN(0) for unmanaged traffic

	struct net_stat *st = bpf_map_lookup_elem(&prism_net_stats, &nid);
	if (st) {
		st->packets += 1;
		st->bytes += skb->len;
	} else {
		struct net_stat init = {.packets = 1, .bytes = skb->len};
		bpf_map_update_elem(&prism_net_stats, &nid, &init, BPF_ANY);
	}

	// Example identity-keyed policy hook: reserved infra identities are always
	// allowed; everything else could be subject to a per-identity verdict map.
	// We only READ identity (and could prism_has_flag a daemon-set facet); we
	// never write the shared bus. Allow.
	return 1;
}
