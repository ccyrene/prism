/* SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause) */
/*
 * MINIMAL LOCAL SHIM — NOT the real libbpf header.
 *
 * On a real build host you install libbpf-dev (or vendor the upstream libbpf
 * src/ headers) and this file is shadowed: clang finds the system
 * <bpf/bpf_helper_defs.h> first. We ship this trimmed copy ONLY so the Prism
 * BPF C can be syntax/type-checked with clang on a host (like the 5.15 WSL2
 * dev box) that has clang + vmlinux.h but no libbpf-dev and no network.
 *
 * It declares just the handful of BPF helper functions the Prism programs
 * call. The signatures match upstream libbpf's generated bpf_helper_defs.h.
 * Helper IDs come from the kernel's BTF (see vmlinux.h, enum bpf_func_id).
 */
#ifndef __PRISM_SHIM_BPF_HELPER_DEFS_H__
#define __PRISM_SHIM_BPF_HELPER_DEFS_H__

/* All BPF helpers are called through fixed integer IDs cast to a function
 * pointer. The real header generates one such wrapper per helper. */

/* void *bpf_map_lookup_elem(void *map, const void *key) */
static void *(*bpf_map_lookup_elem)(void *map, const void *key) = (void *)1;

/* long bpf_map_update_elem(void *map, const void *key, const void *value,
 *                          __u64 flags) */
static long (*bpf_map_update_elem)(void *map, const void *key,
				   const void *value, __u64 flags) = (void *)2;

/* long bpf_map_delete_elem(void *map, const void *key) */
static long (*bpf_map_delete_elem)(void *map, const void *key) = (void *)3;

/* __u64 bpf_ktime_get_ns(void) */
static __u64 (*bpf_ktime_get_ns)(void) = (void *)5;

/* __u64 bpf_get_current_cgroup_id(void) */
static __u64 (*bpf_get_current_cgroup_id)(void) = (void *)80;

/* __u64 bpf_get_current_ancestor_cgroup_id(int ancestor_level)
 * Returns the cgroup id of the current task's ancestor at the given level in
 * the v2 hierarchy (level 0 == root). Used by libprism.bpf.h to resolve the
 * POD-level cgroup id robustly across QoS hierarchy depth. */
static __u64 (*bpf_get_current_ancestor_cgroup_id)(int ancestor_level) = (void *)123;

/* struct task_struct *bpf_get_current_task_btf(void) */
static struct task_struct *(*bpf_get_current_task_btf)(void) = (void *)158;

/* long bpf_printk-style trace; we only need the raw printk helper id. */
static long (*bpf_trace_printk)(const char *fmt, __u32 fmt_size,
				...) = (void *)6;

/* Helpers used by the execsnoop consumer demo (bpf/consumers/). */
/* __u64 bpf_get_current_pid_tgid(void) */
static __u64 (*bpf_get_current_pid_tgid)(void) = (void *)14;
/* __u64 bpf_get_current_uid_gid(void) */
static __u64 (*bpf_get_current_uid_gid)(void) = (void *)15;
/* long bpf_get_current_comm(void *buf, __u32 size) */
static long (*bpf_get_current_comm)(void *buf, __u32 size) = (void *)16;
/* long bpf_perf_event_output(void *ctx, void *map, __u64 flags, void *data, __u64 size) */
static long (*bpf_perf_event_output)(void *ctx, void *map, __u64 flags,
				     void *data, __u64 size) = (void *)25;

/* Helpers used by the cgroup-skb network consumer (bpf/consumers/net_policy_prism.bpf.c).
 * cgroup-skb runs with an skb that carries its cgroup association, so identity
 * resolves here via the skb's (pod-ancestor) cgroup id — the "socket-level
 * network composes" case. (Raw XDP/tc-ingress has no socket/cgroup yet and would
 * instead need identity carried in the packet, as Cilium does.) */
/* __u64 bpf_skb_cgroup_id(struct __sk_buff *skb) */
static __u64 (*bpf_skb_cgroup_id)(struct __sk_buff *skb) = (void *)79;
/* __u64 bpf_skb_ancestor_cgroup_id(struct __sk_buff *skb, int ancestor_level) */
static __u64 (*bpf_skb_ancestor_cgroup_id)(struct __sk_buff *skb,
					   int ancestor_level) = (void *)95;

#endif /* __PRISM_SHIM_BPF_HELPER_DEFS_H__ */
