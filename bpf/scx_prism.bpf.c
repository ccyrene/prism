// SPDX-License-Identifier: GPL-2.0
//
// scx_prism.bpf.c — a minimal IDENTITY-AWARE sched_ext scheduler.
//
// This is the SCHED facet of the Prism identity bus. It is a real sched_ext
// (scx) BPF scheduler for Linux 6.12+ that schedules tasks by their Prism
// workload identity: it READS each task's identity in the SAME shared
// `prism_identity` map the net/observe programs use (via libprism.bpf.h) and
// derives a per-identity time slice / priority from it. One identity, three
// subsystems — the bus.
//
// READ-ONLY CONSUMER: the prism_identity map is BPF_F_RDONLY_PROG, so this
// scheduler cannot write it — the verifier enforces that. The SCHED_MANAGED
// facet (so net/observe can see the scheduler is managing an identity) is
// owned and stamped by the DAEMON (Controller.Flags); this program only READS
// identity (and, if it branched on a facet, would READ it via prism_has_flag).
//
// API NOTE (important): this uses the CURRENT (6.12+) sched_ext kfunc names:
//   * scx_bpf_dsq_insert()        (NOT the old scx_bpf_dispatch())
//   * scx_bpf_dsq_move_to_local() (NOT the old scx_bpf_consume())
//   * SCX_OPS_DEFINE() to declare the ops struct
//   * SCX_OPS_SWITCH_ALL so ALL tasks are switched onto this scheduler
//
// BUILD REQUIREMENTS (see README.md): this file needs the sched_ext build
// tooling that does NOT ship with a stock distro and is ABSENT on our 5.15
// dev box:
//   * a 6.12+ vmlinux.h that actually contains the sched_ext types
//     (struct sched_ext_ops, struct scx_init_task_args, enum scx_dsq_id_flags,
//      SCX_SLICE_DFL, ...). Our 5.15 BTF has NONE of these.
//   * scx's <scx/common.bpf.h> (from the sched_ext-schedulers repo / kernel
//     tools/sched_ext), which declares the scx_bpf_* kfuncs and SCX_OPS_DEFINE.
//   * libbpf + the scx loader/skeleton to actually attach it.
// On 5.15 it WILL NOT compile (no scx headers, no scx types in BTF) and WILL
// NOT load (no CONFIG_SCHED_CLASS_EXT). That is expected and documented.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h> // BPF_CORE_READ() — read p->cgroups->dfl_cgrp->kn->id
// scx's kfunc declarations + SCX_OPS_DEFINE. Provided by the scx tooling, not by
// libbpf-dev. This include is what fails on a 5.15 host. Built against scx v1.1.0
// (the newest release whose headers compile against a 6.17 BTF; v1.1.1+ reference
// 6.18-only kernel arg-structs). See bpf/README.md.
#include <scx/common.bpf.h>
#include "libprism.bpf.h"

// scx's headers (enums.autogen.bpf.h) redefine these public constants as
// const-volatile externs (__SCX_DSQ_LOCAL / __SCX_SLICE_DFL) that the scx
// USERSPACE framework normally populates from the kernel's BTF *before* load.
// Our deliberately-minimal libbpf loader (loader/scx_prism_loader.c) does not
// run that scx enum-init, so those externs would remain 0 at load — and a 0
// dispatch-queue id makes scx_bpf_dsq_insert() target the non-existent DSQ 0,
// which the kernel rejects at runtime ("non-existent DSQ 0x0", scheduler
// auto-disabled). We instead resolve them with bpf_core_enum_value(): a CO-RE
// relocation libbpf fills in from the TARGET kernel's BTF at load time, so the
// scheduler is self-contained and loadable by any plain libbpf loader, with no
// scx userspace framework. (The unused __SCX_* externs the header declared are
// harmless.)
#undef SCX_DSQ_LOCAL
#define SCX_DSQ_LOCAL bpf_core_enum_value(enum scx_dsq_id_flags, SCX_DSQ_LOCAL)
#undef SCX_SLICE_DFL
#define SCX_SLICE_DFL bpf_core_enum_value(enum scx_public_consts, SCX_SLICE_DFL)

char LICENSE[] SEC("license") = "GPL";

// Tell sched_ext to switch ALL tasks onto this scheduler (system-wide), rather
// than only tasks explicitly moved to SCHED_EXT. In the current scx/sched_ext
// API switching ALL tasks is the DEFAULT: the old opt-in SCX_OPS_SWITCH_ALL was
// removed and the opt-OUT is now SCX_OPS_SWITCH_PARTIAL, so flags=0 gives the
// intended system-wide behavior. (Updated from SCX_OPS_SWITCH_ALL, which no
// longer exists in scx >= v1.1.0; see bpf/README.md.)
#define PRISM_OPS_FLAGS 0

// One global dispatch queue per "scheduling class" we map identities onto. We
// keep it deliberately tiny: a high-priority lane and a normal lane. Identity
// determines the lane and the slice. (Custom DSQ ids are user-chosen u64s;
// SCX_DSQ_LOCAL / per-CPU local DSQs are reserved and handled by the kernel.)
#define PRISM_DSQ_HIGH 0x9001ULL // latency-sensitive identities
#define PRISM_DSQ_NORM 0x9002ULL // everything else

// Per-identity time slices. A "high" identity gets a longer slice (better
// throughput / fewer preemptions); normal identities get the default. These are
// the scheduling "treatment" that differs by identity — the whole point.
#ifdef PRISM_SHORT_SLICE
// Short slices suited to IPC-heavy microservice pipelines (many short RPC bursts
// + frequent wakeups), where the 20ms SCX_SLICE_DFL lets one service hog a CPU
// while the rest of the request chain waits. -DPRISM_SHORT_SLICE selects these.
#define PRISM_SLICE_HIGH 2000000ULL // 2ms
#define PRISM_SLICE_NORM 1000000ULL // 1ms
#else
#define PRISM_SLICE_HIGH (SCX_SLICE_DFL * 2)
#define PRISM_SLICE_NORM (SCX_SLICE_DFL)
#endif

// Per-task identity cache (BPF task-local storage). Resolving identity on every
// enqueue costs a bpf_task_get_cgroup_id() helper call + a hash-map lookup. A
// task's identity is stable for its lifetime, so we memoize it on the task after
// the first enqueue; every subsequent enqueue reads it in O(1) with NO hash and
// NO cgroup-id helper — the per-decision fast path. This is the single biggest
// kernel-side win for a hot scheduler. (BPF_MAP_TYPE_TASK_STORAGE, kernel >=
// 5.12; BPF_F_NO_PREALLOC is required for task storage.)
//
// Caveat: if the daemon relabels a running workload its cached identity goes
// stale until the task exits; production would invalidate via a generation
// counter bumped on relabel. Acceptable for the prototype and documented.
struct prism_task_cache_val {
	__u32 nid;      // cached numeric identity (PRISM_ID_UNKNOWN if off-bus)
	__u32 resolved; // 1 once resolution has been attempted (negative-cache too)
};
struct {
	__uint(type, BPF_MAP_TYPE_TASK_STORAGE);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, int);
	__type(value, struct prism_task_cache_val);
} prism_task_cache SEC(".maps");

// Demo policy: decide whether an identity is latency-sensitive. In a real
// deployment this would be driven by a per-identity priority/weight written by
// the daemon (e.g. encoded in spare bits or a side map). Here we use a simple,
// explicit rule so the behavior is obvious and verifiable: reserved infra
// identities and a designated "fast" identity get the high lane.
static __always_inline bool prism_is_high_prio(__u32 nid)
{
	// Reserved infra (host, kube-apiserver, health, ...) is latency
	// sensitive — keep the control plane responsive.
	if (prism_id_is_reserved(nid))
		return true;
	// Convention for the demo: the first dynamic identity is the "fast"
	// tier. Swap for a real per-identity weight lookup in production.
	return nid == PRISM_ID_MIN_DYNAMIC;
}

// ops.select_cpu: pick a CPU for a task waking up. We use the scx helper that
// returns an idle CPU if one is available (and, if so, immediately dispatches
// the task to its local DSQ with the default slice for a fast wakeup path).
s32 BPF_STRUCT_OPS(prism_select_cpu, struct task_struct *p, s32 prev_cpu,
		   u64 wake_flags)
{
	bool is_idle = false;
	s32 cpu;

	cpu = scx_bpf_select_cpu_dfl(p, prev_cpu, wake_flags, &is_idle);
	if (is_idle) {
		// Fast path: an idle CPU was found, run there immediately.
		scx_bpf_dsq_insert(p, SCX_DSQ_LOCAL, SCX_SLICE_DFL, 0);
	}
	return cpu;
}

// ops.enqueue: the heart of the identity-aware policy. Resolve (READ) the
// task's Prism identity from the shared bus and choose a DSQ + slice from it,
// then insert the task. The SCHED_MANAGED facet is set by the daemon, not here
// (the map is RDONLY_PROG to BPF programs) — this hook is a pure reader.
void BPF_STRUCT_OPS(prism_enqueue, struct task_struct *p, u64 enq_flags)
{
	__u32 nid;
	bool on_bus;

	// Fast path: identity already memoized on this task — no cgroup-id helper,
	// no map hash. This is what the steady-state scheduler hits on every
	// enqueue after a task's first.
	struct prism_task_cache_val *tc =
		bpf_task_storage_get(&prism_task_cache, p, 0, 0);
	if (tc && tc->resolved) {
		nid = tc->nid;
		on_bus = (nid != PRISM_ID_UNKNOWN);
	} else {
		// Slow path (first enqueue for this task): one bus READ, memoize it.
		// Per-task cgroup id == the bus key. We read the task's default
		// (cgroup-v2) cgroup id straight off p — cgroups->dfl_cgrp->kn->id —
		// rather than bpf_get_current_cgroup_id(), because enqueue may run off
		// `current`. This is exactly the value bpf_get_current_cgroup_id() and
		// the userspace daemon's cgroup-id keyer use (the cgroup dir inode). We
		// read dfl_cgrp directly (NOT scx_bpf_task_cgroup(p), which returns the
		// ROOT cgroup when the CPU controller is disabled) so resolution does
		// not depend on the cpu controller being enabled. NOTE (leaf-vs-pod):
		// this is the task's LEAF cgroup id; resolving the pod-ancestor level
		// for an arbitrary task p (the bounded-ancestor walk libprism.bpf.h does
		// for the *current* task) is future work — documented. (Updated from the
		// non-existent bpf_task_get_cgroup_id() kfunc; see bpf/README.md.)
		__u64 key = BPF_CORE_READ(p, cgroups, dfl_cgrp, kn, id);
		struct prism_identity *id = prism_lookup(key);
		nid = prism_id(id); // PRISM_ID_UNKNOWN(0) if not on the bus
		on_bus = (id != NULL);

		tc = bpf_task_storage_get(&prism_task_cache, p, 0,
					  BPF_LOCAL_STORAGE_GET_F_CREATE);
		if (tc) {
			tc->nid = nid;
			tc->resolved = 1;
		}
		// No write to the shared map: SCHED_MANAGED is stamped by the daemon.
	}

	u64 dsq, slice;
	if (on_bus && prism_is_high_prio(nid)) {
		dsq = PRISM_DSQ_HIGH;
		slice = PRISM_SLICE_HIGH;
	} else {
		dsq = PRISM_DSQ_NORM;
		slice = PRISM_SLICE_NORM;
	}

	// Queue the task on the identity-chosen DSQ with the identity-chosen
	// slice. dispatch() (below) drains these into per-CPU local DSQs.
	scx_bpf_dsq_insert(p, dsq, slice, enq_flags);
}

#ifdef PRISM_FAIR
// Anti-starvation (PRISM_FAIR build): even under sustained HIGH-lane load, give
// the NORM lane a guaranteed share of dispatches so normal tasks cannot be
// starved indefinitely by a busy high-priority workload. Every
// PRISM_NORM_EVERY-th dispatch services NORM first; otherwise HIGH first. This
// is the production policy the strict default deliberately omits: it bounds
// NORM's worst-case wait while still protecting the latency-critical HIGH tail.
#ifndef PRISM_NORM_EVERY
#define PRISM_NORM_EVERY 8 // override at build time (-DPRISM_NORM_EVERY=N) to tune the fairness/latency tradeoff
#endif
__u64 prism_disp_count = 0; // global dispatch tick (approximate; benign races)
#endif

// ops.dispatch: when a CPU needs work, pull from the high lane first (priority),
// then the normal lane. scx_bpf_dsq_move_to_local() moves the head task of the
// given DSQ to the calling CPU's local DSQ; it returns true if it moved one.
void BPF_STRUCT_OPS(prism_dispatch, s32 cpu, struct task_struct *prev)
{
#ifdef PRISM_FAIR
	// Anti-starvation slot: once every PRISM_NORM_EVERY dispatches, let NORM go
	// first so a saturated HIGH lane cannot starve normal tasks indefinitely.
	if ((__sync_fetch_and_add(&prism_disp_count, 1) % PRISM_NORM_EVERY) == 0) {
		if (scx_bpf_dsq_move_to_local(PRISM_DSQ_NORM))
			return;
		scx_bpf_dsq_move_to_local(PRISM_DSQ_HIGH);
		return;
	}
#endif
	// Priority: drain HIGH before NORM. Strict in the default build; the
	// PRISM_FAIR build interleaves a NORM slot (above) for anti-starvation. A
	// fuller production policy would use a per-identity weight; kept minimal here.
	if (scx_bpf_dsq_move_to_local(PRISM_DSQ_HIGH))
		return;
	scx_bpf_dsq_move_to_local(PRISM_DSQ_NORM);
}

// ops.init: create the global DSQs once at attach time. Returning non-zero
// aborts the load. Node id -1 == not NUMA-pinned.
s32 BPF_STRUCT_OPS_SLEEPABLE(prism_init)
{
	s32 err;

	err = scx_bpf_create_dsq(PRISM_DSQ_HIGH, -1);
	if (err)
		return err;
	return scx_bpf_create_dsq(PRISM_DSQ_NORM, -1);
}

// ops.exit: record why the scheduler is being unloaded (clean stop, error,
// etc.). Kept trivial; the loader reads ei->reason for diagnostics.
void BPF_STRUCT_OPS(prism_exit, struct scx_exit_info *ei)
{
	// Nothing to tear down: DSQs are destroyed by the kernel on unload.
}

// Bind the ops together. SCX_OPS_DEFINE wires each callback into the
// struct sched_ext_ops and gives the scheduler its name + flags. The loader
// finds this symbol and attaches it.
SCX_OPS_DEFINE(prism_ops,
	       .select_cpu = (void *)prism_select_cpu,
	       .enqueue    = (void *)prism_enqueue,
	       .dispatch   = (void *)prism_dispatch,
	       .init       = (void *)prism_init,
	       .exit       = (void *)prism_exit,
	       .flags      = PRISM_OPS_FLAGS,
	       .name       = "prism");
