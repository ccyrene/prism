# Building the Prism BPF (kernel-side) artifacts

The kernel side of the Prism identity bus is four files in this directory:

| file | role | builds on a stock distro? |
|------|------|---------------------------|
| `prism_maps.bpf.h` | frozen C ABI: `struct prism_identity` + the shared `prism_identity` map | header only |
| `libprism.bpf.h` | the consumer-facing helper library (lookup / flags / id) every program includes | header only |
| `compose_demo.bpf.c` | net (`cgroup_skb/egress`) + observe (`tp/sched/sched_switch`) facets | **yes** — generic program types + libbpf only |
| `scx_prism.bpf.c` | the identity-aware `sched_ext` scheduler (the sched facet) | **no** — needs a 6.12+ kernel's BTF + scx tooling |

Everything keys off the one `prism_identity` map → that sharing is the bus.

## Prerequisites

* clang/LLVM ≥ 12 with the BPF target (we tested clang 18).
* `bpftool` (to dump BTF). A version matching a recent kernel; the one bundled
  with `linux-tools-<ver>` works even if the running kernel is older.
* **libbpf development headers** (`<bpf/bpf_helpers.h>`, …): install
  `libbpf-dev`, or vendor upstream libbpf's `src/` headers.
* For `scx_prism.bpf.c` only: the **sched_ext build tooling** —
  `<scx/common.bpf.h>` and friends from the kernel's `tools/sched_ext` or the
  `scx` (sched-ext/scx) repo, plus a 6.12+ `vmlinux.h` that actually contains
  the sched_ext types.

## Step 1 — generate `vmlinux.h`

`vmlinux.h` is the kernel type universe extracted from BTF. Generate it from
the **target** kernel (6.12+ for the scheduler):

```sh
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
```

On this dev box `/usr/sbin/bpftool` is a kernel-version wrapper that refuses to
run (it looks for `linux-tools-$(uname -r)`); call the real binary directly:

```sh
/usr/lib/linux-tools-6.8.0-117/bpftool btf dump file /sys/kernel/btf/vmlinux \
    format c > vmlinux.h
```

This produced a 149k-line `vmlinux.h` here. NOTE: it was dumped from the **5.15
WSL2** kernel, so it contains the generic BPF types (`task_struct`, `cgroup`,
`__sk_buff`, `trace_event_raw_sched_switch`, the `bpf_func_id` enum, …) — enough
for `compose_demo.bpf.c` — but it does **not** contain any `sched_ext` types
(`struct sched_ext_ops`, `SCX_SLICE_DFL`, `SCX_DSQ_LOCAL`, …). For the scheduler
you must regenerate `vmlinux.h` on a 6.12+ host.

## Step 2 — compile the composability demo (works here)

`compose_demo.bpf.c` uses only long-stable program types and helpers, so it
compiles and loads on any modern kernel:

```sh
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
      -I. -c compose_demo.bpf.c -o compose_demo.bpf.o
```

(`-I.` so `#include "vmlinux.h"` / `"libprism.bpf.h"` resolve. On a host with
`libbpf-dev`, `<bpf/bpf_helpers.h>` is found automatically.)

Verify the object has the expected sections:

```sh
llvm-readelf -S compose_demo.bpf.o | grep -E 'cgroup_skb|sched_switch|\.maps'
```

You should see `cgroup_skb/egress`, `tp/sched/sched_switch`, and `.maps`.

## Step 3 — compile the sched_ext scheduler (needs a 6.12 host + scx tooling)

On a 6.12+ host with the scx tooling on the include path:

```sh
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
      -I. -I<path-to-scx>/include -c scx_prism.bpf.c -o scx_prism.bpf.o
```

Then load it with the scx userspace loader (the scx repo's `scx_loader`, or a
custom libbpf skeleton that attaches the `prism_ops` struct_ops). The kernel
must be built with `CONFIG_SCHED_CLASS_EXT=y`.

## What fails on this 5.15 dev box, and exactly why

### `vmlinux.h` via the default `bpftool` wrapper
```
WARNING: bpftool not found for kernel 5.15.167.4-microsoft
```
`/usr/sbin/bpftool` is a shell wrapper that execs `linux-tools-$(uname -r)`,
which isn't installed. Fixed by calling the real `linux-tools-6.8.0-117/bpftool`
binary directly (Step 1).

### Missing `libbpf-dev` (affects every `.c` until installed)
```
fatal error: 'bpf/bpf_helpers.h' file not found
```
`libbpf1` (the runtime `.so`) is installed but the **development headers** are
not, and this box has no network/root to `apt-get install libbpf-dev`. To make
the buildable artifacts verifiable here anyway, a **minimal local shim** of just
the macros/helper-prototypes the Prism programs use lives in `include/bpf/`
(`bpf_helpers.h`, `bpf_helper_defs.h`). It is clearly marked "NOT the real
libbpf header" and is shadowed by the system header on any host that has
`libbpf-dev`. Add `-Iinclude` to use it:

```sh
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
      -I. -Iinclude -c compose_demo.bpf.c -o compose_demo.bpf.o   # succeeds here
```

### `scx_prism.bpf.c` (the scheduler) — unbuildable here even with the shim
```
fatal error: 'scx/common.bpf.h' file not found
```
That header (kfunc declarations, `BPF_STRUCT_OPS`, `SCX_OPS_DEFINE`,
`SCX_OPS_SWITCH_ALL`) ships with the sched_ext tooling, not with libbpf, and is
not present. Even if the include is stubbed out, compilation still fails because
**none of the sched_ext types exist in the 5.15 BTF**. Verified absent in our
`vmlinux.h` (0 occurrences each):

```
sched_ext_ops, scx_exit_info, scx_init_task_args,
SCX_SLICE_DFL, SCX_DSQ_LOCAL, SCX_OPS_DEFINE,
scx_bpf_dsq_insert, scx_bpf_dsq_move_to_local,
scx_bpf_select_cpu_dfl, bpf_task_get_cgroup_id
```

This is expected: WSL2's 5.15 kernel has no `CONFIG_SCHED_CLASS_EXT`, so it can
neither compile (no scx types/headers) nor load (no sched_ext support) the
scheduler. `scx_prism.bpf.c` is written against the **current 6.12 scx API**
(`scx_bpf_dsq_insert`, `scx_bpf_dsq_move_to_local`, `SCX_OPS_DEFINE`,
`SCX_OPS_SWITCH_ALL`) and is intended to be built and loaded on a 6.12+ host
with the scx tooling.

## Summary of local verification (5.15 WSL2, clang 18)

| artifact | command | result here |
|----------|---------|-------------|
| `vmlinux.h` | `bpftool btf dump … format c` | OK (real bpftool binary) |
| `libprism.bpf.h` | compiled via a probe consumer | OK |
| `compose_demo.bpf.o` | `clang … -I. -Iinclude -c compose_demo.bpf.c` | OK (net + observe + .maps sections) |
| `scx_prism.bpf.o` | `clang … -c scx_prism.bpf.c` | FAILS — needs 6.12 BTF + scx tooling (documented above) |

## Building `scx_prism.bpf.o` for real on a 6.12+ kernel (DONE on Linux 6.17, 2026-05-29)

On a real sched_ext host (Ubuntu 24.04, **kernel 6.17.0-29**, clang 18, libbpf 1.3,
bpftool 7.7) the scheduler builds, loads, and schedules. Recipe:

```sh
# 1. 6.17 vmlinux.h from the running kernel's BTF (contains struct sched_ext_ops):
bpftool btf dump file /sys/kernel/btf/vmlinux format c > bpf/vmlinux.h
# 2. scx BPF headers — pin scx to v1.1.0 (the newest release whose headers compile
#    against a 6.17 BTF; v1.1.1+ reference 6.18-only kernel arg-structs):
git clone https://github.com/sched-ext/scx /opt/scx && git -C /opt/scx checkout v1.1.0
# 3. compile (scx headers + 6.17 BTF):
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
      -I bpf -I /opt/scx/scheds/include -c bpf/scx_prism.bpf.c -o bpf/scx_prism.bpf.o
# 4. loader + load:
make -C loader && sudo ./loader/scx_prism_loader bpf/scx_prism.bpf.o
cat /sys/kernel/sched_ext/state            # -> enabled
bpftool struct_ops show | grep prism        # -> prism_ops sched_ext_ops
```

### Three scx-API updates vs the original 6.12-era source

The source was written against an early-6.12 scx API; current scx (v1.1.0) needed
three minimal, documented changes in `scx_prism.bpf.c` (all kept faithful to intent):

1. **`SCX_OPS_SWITCH_ALL` was removed** — switching ALL tasks is now the DEFAULT;
   the opt-OUT is `SCX_OPS_SWITCH_PARTIAL`. So `PRISM_OPS_FLAGS` is now `0`.
2. **`bpf_task_get_cgroup_id()` is not a kfunc.** Read the task's leaf (cgroup-v2)
   id directly with `BPF_CORE_READ(p, cgroups, dfl_cgrp, kn, id)` — the same value
   `bpf_get_current_cgroup_id()` and the daemon's cgroup keyer use, independent of
   whether the cpu controller is enabled (unlike `scx_bpf_task_cgroup(p)`, which
   returns the root cgroup when the cpu controller is off).
3. **scx public consts are runtime externs.** `SCX_DSQ_LOCAL` / `SCX_SLICE_DFL`
   are `const volatile` externs (`__SCX_*`) that the scx *userspace framework*
   normally fills from kernel BTF before load; a minimal libbpf loader leaves them
   0 (→ "non-existent DSQ 0x0" runtime abort). We `#undef` and resolve them with
   `bpf_core_enum_value()` (a CO-RE relocation libbpf fills from the target BTF at
   load), so the object is self-contained for any plain libbpf loader.

Result on 6.17: `nr_rejected = 0`, `dmesg: sched_ext: BPF scheduler "prism" enabled`,
clean SIGINT detach to EEVDF. Tail-latency eval: see
`scripts/eval/run-sched-eval-contended.sh`.
