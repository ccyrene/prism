// SPDX-License-Identifier: GPL-2.0
//
// execsnoop_prism.bpf.c — the famous BCC/libbpf-tools `execsnoop` tracer, made
// WORKLOAD-IDENTITY-AWARE by plugging it into the Prism identity bus.
//
// Upstream execsnoop (iovisor/bcc, libbpf-tools) traces execve() and emits
// pid / ppid / uid / comm / argv. It has ZERO notion of which Kubernetes
// workload an exec belongs to — to attribute an event to a pod, a userspace
// consumer must re-derive it (parse /proc/<pid>/cgroup, or watch the K8s API
// and maintain a pid->pod cache, i.e. re-implement most of what prismd does).
//
// THE INTEGRATION (this is the whole point of the demo): by including
// libprism.bpf.h and READING the ONE shared `prism_identity` map that prismd
// already populates, execsnoop tags every exec with its workload identity
// IN-KERNEL, in ~2 added lines + 1 struct field — no extra pod-watching, no
// userspace cgroup parsing. The cost is a single O(1) map lookup per event.
//
// READ-ONLY CONSUMER: the prism_identity map is BPF_F_RDONLY_PROG, so this
// tracer cannot write it — the verifier enforces that. It only READS identity
// to attribute the exec; the OBSERVED facet is owned and stamped by the daemon
// (Controller.Flags), not by this consumer.
//
// Faithful to upstream's conventions (two execve tracepoints, per-pid HASH
// stash, perf-event-array output). The argv-copy loop and the CO-RE ppid read
// are upstream-internal and irrelevant to the identity story, so they are
// omitted here (marked below) to keep the demo focused and host-compilable.
//
// Compiles on this host (clang -target bpf, vmlinux.h + minimal shim). On a real
// 6.12+/libbpf host it builds unchanged against system libbpf, and the
// prism_identity map resolves (LIBBPF_PIN_BY_NAME) to the very map prismd pins.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include "prism_maps.bpf.h"  // the shared bus map (prism_identity)
#include "libprism.bpf.h"    // prism_identity_of_current(), prism_id() — READERS only

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16

// The emitted event. Identical to upstream execsnoop's struct event EXCEPT for
// the one added field: prism_identity. That single field is what turns a raw
// exec trace into a workload-attributed audit record.
struct exec_event {
	__u32 pid;
	__u32 uid;
	char  comm[TASK_COMM_LEN];
	__u32 prism_identity; // <-- ADDED: the workload identity, straight from the bus
};

// Per-pid in-flight stash (upstream execsnoop convention).
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u32);
	__type(value, struct exec_event);
} execs SEC(".maps");

// Output channel to userspace (upstream uses a perf event array, not ringbuf).
struct {
	__uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
	__uint(key_size, sizeof(__u32));
	__uint(value_size, sizeof(__u32));
} events SEC(".maps");

static const struct exec_event empty_event = {};

SEC("tracepoint/syscalls/sys_enter_execve")
int execsnoop_prism_enter(void *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 pid = (__u32)id;        // upstream keys the stash on the lower 32 bits
	__u32 tgid = (__u32)(id >> 32);

	if (bpf_map_update_elem(&execs, &pid, &empty_event, BPF_NOEXIST))
		return 0;
	struct exec_event *e = bpf_map_lookup_elem(&execs, &pid);
	if (!e)
		return 0;

	e->pid = tgid;
	e->uid = (__u32)bpf_get_current_uid_gid();

	// ---- PRISM INTEGRATION (the whole integration, both lines): attribute this
	// exec to a workload. One O(1) READ of the shared bus map, resolved to the
	// pod-level cgroup id (prism_identity_of_current walks QoS hierarchy depth).
	// No userspace round-trip, no /proc parsing. Unmanaged tasks -> identity 0.
	struct prism_identity *wid = prism_identity_of_current();
	e->prism_identity = prism_id(wid);

	// (upstream execsnoop also reads ppid via CO-RE and copies the full argv
	//  here — omitted in this demo; unchanged from upstream, irrelevant to the
	//  identity integration.)
	return 0;
}

SEC("tracepoint/syscalls/sys_exit_execve")
int execsnoop_prism_exit(void *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 pid = (__u32)id;

	struct exec_event *e = bpf_map_lookup_elem(&execs, &pid);
	if (!e)
		return 0;

	bpf_get_current_comm(&e->comm, sizeof(e->comm));

	// The OBSERVED facet (so a network/sched consumer of the SAME map can see
	// this identity is being audited) is set by the DAEMON, not here: the map
	// is RDONLY_PROG to BPF programs, so consumers compose by READING facets,
	// never writing them. One identity, many subsystems, one writer.

	bpf_perf_event_output(ctx, &events, 0xffffffffULL /*BPF_F_CURRENT_CPU*/,
			      e, sizeof(*e));
	bpf_map_delete_elem(&execs, &pid);
	return 0;
}
