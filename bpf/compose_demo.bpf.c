// SPDX-License-Identifier: GPL-2.0
//
// compose_demo.bpf.c — the Prism 3-way COMPOSABILITY demo.
//
// The thesis of Prism: one workload identity, many subsystems. The userspace
// daemon classifies a workload once and writes a single `struct prism_identity`
// into the shared `prism_identity` map (keyed by cgroup id). Independent BPF
// programs from DIFFERENT subsystems then key off that ONE value:
//
//   net      -> sets PRISM_FLAG_NET_POLICY   (this file, cgroup-skb hook)
//   observe  -> sets PRISM_FLAG_OBSERVED      (this file, tracepoint)
//   sched    -> sets PRISM_FLAG_SCHED_MANAGED (scx_prism.bpf.c, see note below)
//
// None of them re-classify the workload or own their own identity table; they
// all READ the same value through libprism.bpf.h. That single shared value,
// fanned out across net + sched + observe, IS the identity bus. This one object
// file carries the net and observe halves; the sched half lives in the
// sched_ext scheduler because it must be a separate BPF program type.
//
// READ-ONLY CONSUMERS: the prism_identity map is BPF_F_RDONLY_PROG, so these
// programs cannot (and must not) write it — the verifier enforces that. The
// daemon owns the facet flags (Controller.Flags); these programs only READ
// identity and READ facets via prism_has_flag() to compose across subsystems.
//
// Build (6.12 host with libbpf): see README.md. This file (unlike the scheduler)
// uses only generic, long-stable program types + helpers, so it compiles and
// loads on essentially any modern kernel — no scx tooling required.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include "libprism.bpf.h"

char LICENSE[] SEC("license") = "GPL";

// ---------------------------------------------------------------------------
// (a) NET facet — cgroup-skb egress hook.
//
// Attached to a cgroup's egress path, this runs for every packet a workload in
// that cgroup sends. We READ the workload's identity from the SAME bus the
// scheduler uses and (as a stand-in for a real allow/deny policy engine) make a
// trivial verdict keyed on the identity. The fact that the daemon has tagged
// this identity NET_POLICY (a facet other subsystems can observe) is set by the
// daemon, not here — this hook is a pure reader of the shared value.
//
// Return: 1 = allow the packet, 0 = drop. A real net-policy program would look
// up an identity->verdict table; here we simply demonstrate that the verdict is
// a pure function of the shared identity.
// ---------------------------------------------------------------------------
SEC("cgroup_skb/egress")
int prism_net_egress(struct __sk_buff *skb)
{
	// Resolve the workload's identity from the shared bus. We use the
	// pod-ancestor-robust resolver so deep QoS cgroup hierarchies still hit
	// the pod-level key the daemon wrote.
	struct prism_identity *id = prism_identity_of_current();

	// Unknown / not-yet-classified workloads: don't apply per-workload
	// policy, just allow (fail-open for the demo). Real deployments may
	// fail-closed for unmanaged traffic.
	if (!id)
		return 1;

	__u32 nid = prism_id(id);

	// Reserved identities (host, world, kube-apiserver, ...) bypass
	// per-workload net policy — they are infrastructure, not workloads.
	if (prism_id_is_reserved(nid))
		return 1;

	// Demo verdict: a pure function of identity. (Swap for a real
	// identity->policy map lookup in production.) Here: everything allowed.
	// The NET_POLICY facet is stamped by the daemon, not by this read-only
	// consumer (the map is RDONLY_PROG to BPF programs).
	return 1;
}

// ---------------------------------------------------------------------------
// (b) OBSERVE facet — scheduler-switch tracepoint observer.
//
// This passive observer runs on every context switch. It READS (never decides
// and never writes) the identity of the task being switched in. The interesting
// part for composability: because it reads the SAME value the daemon stamps for
// every subsystem, it can tell whether net and/or sched are managing this
// identity — e.g. emit a richer event when a SCHED_MANAGED identity is also
// under NET_POLICY. That cross-subsystem visibility falls out for free from
// sharing one value, with the observer reading facets it never writes.
//
// We use the stable sched_switch tracepoint; its context layout is provided by
// vmlinux.h as struct trace_event_raw_sched_switch.
// ---------------------------------------------------------------------------
SEC("tp/sched/sched_switch")
int prism_observe_switch(struct trace_event_raw_sched_switch *ctx)
{
	// Pod-ancestor-robust read of the current task's identity.
	struct prism_identity *id = prism_identity_of_current();

	if (!id)
		return 0; // unmanaged task; nothing on the bus to observe

	// Composability payoff: with the single shared value we can correlate
	// facets the DAEMON set for OTHER subsystems without any extra plumbing —
	// a pure read, no write (the map is RDONLY_PROG to BPF programs).
	if (prism_has_flag(id, PRISM_FLAG_SCHED_MANAGED) &&
	    prism_has_flag(id, PRISM_FLAG_NET_POLICY)) {
		// A real observer would push an event to a ring buffer here.
		// Kept as a debug print so the demo has zero extra map deps.
		bpf_printk("prism: id=%u fully composed (net+sched+observe)\n",
			   prism_id(id));
	}

	return 0;
}

// ---------------------------------------------------------------------------
// (c) SCHED facet — for reference only; implemented in scx_prism.bpf.c.
//
// The sched_ext scheduler includes the very same libprism.bpf.h and, on
// ops.enqueue, does:
//
//     struct prism_identity *id = prism_lookup(bpf_task_get_cgroup_id(p));
//     ... choose DSQ / slice from prism_id(id) ...
//
// Same map, same value, same helpers — net + sched + observe all keyed off ONE
// identity, all READING it (the daemon owns the SCHED_MANAGED facet). That is
// the bus.
// ---------------------------------------------------------------------------
