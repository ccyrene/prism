# Writing your own Prism consumer

A **consumer** is any eBPF program that reads the shared `prism_identity` bus to
find out *which Kubernetes workload* a task / packet / event belongs to, then
acts on it. Prism is designed for this: bring your own program, on your own
attach point, with your own state — the bus is an ABI, not a closed app.

The two programs here are real, working examples:

| file | type | what it does |
|---|---|---|
| `net_policy_prism.bpf.c` | `cgroup_skb/egress` | attributes each packet to an identity, counts in its own map |
| `execsnoop_prism.bpf.c` | `execve` tracepoints | tags each exec with its workload identity |

A minimal, copy-me skeleton lives in [`examples/consumer-template/myconsumer.bpf.c`](../../examples/consumer-template/myconsumer.bpf.c).

## The model (read this first)

The bus map is created `BPF_F_RDONLY_PROG`, so **a consumer can only READ it** —
the kernel verifier rejects any program that tries to write `prism_identity`.
`prismd` is the *sole writer* of identities. This is the safety property that
lets you plug arbitrary third-party programs onto one shared bus: a buggy or
hostile consumer cannot corrupt anyone's identity.

> **prismd writes identities · you read them and do whatever you want in your own program and your own maps.**

## Four steps

**1. Include the two headers** (from `bpf/`):

```c
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include "prism_maps.bpf.h"   // the shared bus map (read-only) + struct prism_identity
#include "libprism.bpf.h"     // the reader API (header-only, all static __always_inline)
char LICENSE[] SEC("license") = "GPL";   // required: BPF readers use GPL-only helpers
```

**2. Read the identity** — pick the reader that fits your context:

| reader (`libprism.bpf.h`) | use when |
|---|---|
| `prism_identity_of_current()` | you're on a task (tracepoint, kprobe, LSM) — walks to the pod cgroup |
| `prism_lookup(bpf_skb_cgroup_id(skb))` | you have an `skb` (cgroup_skb / tc) |
| `prism_lookup(key)` | you already have the workload key (leaf/pod cgroup id) |
| `prism_id(wid)` | → the `__u32` identity (`0` = `PRISM_ID_UNKNOWN`, i.e. off-bus) |
| `prism_has_flag(wid, FLAG)` | branch on a daemon-set facet bit |

**3. Keep your state in your OWN map** (never write the bus):

```c
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 1<<16);
         __type(key, __u32); __type(value, __u64); } my_state SEC(".maps");
```

**4. Build, then load reusing the pinned bus** (so you read the *same* map the
scheduler/daemon use, not a fresh empty copy):

```sh
# build (vmlinux.h + the prism headers are in bpf/)
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -I bpf \
      -c examples/consumer-template/myconsumer.bpf.c -o myconsumer.bpf.o

# load + auto-attach (tracepoints), REUSING the already-pinned prism_identity map
sudo bpftool prog loadall myconsumer.bpf.o /sys/fs/bpf/myconsumer \
     map name prism_identity pinned /sys/fs/bpf/prism_identity autoattach

# read your own map back
sudo bpftool map dump name my_open_counts
```

For a `cgroup_skb` program, attach it to a cgroup instead of `autoattach`:

```sh
sudo bpftool cgroup attach /sys/fs/cgroup/<your-cgroup> egress \
     pinned /sys/fs/bpf/myconsumer/<prog-name>
```

(The bus must already exist — i.e. `prismd` or a Prism scheduler is running and
has pinned `/sys/fs/bpf/prism_identity`. `scripts/three-leg-demo.sh` shows the
whole sched + net + trace flow end to end.)

## Notes

- The identity ABI (the 24-byte value layout, the facet flags, the per-identity
  scheduling class/weight) is frozen and documented in
  [`spec/README.md`](../../spec/README.md). Code against it; don't guess.
- Your consumer's eBPF object is **GPL-2.0** (it includes the GPL headers and
  declares `SEC("license")="GPL")` — inherent to eBPF that uses GPL-only kernel
  helpers, not a Prism-specific restriction. Your userspace loader/app is yours.
