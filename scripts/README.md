# Prism — from zero to real: the runbook

Prism is a **workload-identity bus**: a single pinned BPF hash map
(`prism_identity`, at `/sys/fs/bpf/prism/identity`) that several kernel
subsystems — a network program (`cgroup_skb`), an observer (`tracepoint`), and a
`sched_ext` scheduler — all key off. `prismd` watches Kubernetes Pods and writes
each workload's stable numeric identity into that map; the BPF consumers read it.
**One identity, many subsystems — that shared map is the bus.**

This directory provisions the hardest tier (a real `sched_ext` kernel). This
README is the whole ladder: three tiers, each proving more, each needing more
hardware/kernel. Climb only as far as your claim requires.

| Tier | What you run | What it PROVES | What it needs |
|------|--------------|----------------|---------------|
| **A. Control plane only** | `go run ./examples/livedemo` (or `prismd -sim`) | The real client-go informer resolves Pod -> identity and propagates it into the sink (dedup, relabel re-alloc, delete release), sub-microsecond. **No BPF.** | Any host. No root, no cluster, no special kernel. (Verified on 5.15 WSL2.) |
| **B. Real cluster** | `run-kind.sh` + the manifests in `deploy/` | `prismd` runs as a DaemonSet against a **real Kubernetes API**, node-scoped, resolving live Pods. Optionally exercises the real pinned BPF map *if* the node kernel/caps allow. | Docker + kind (or any cluster). A Linux kernel; BPF map mode additionally needs bpffs + CAP_BPF on the node. |
| **C. Real `sched_ext` kernel** | `provision-schedext-vm.sh` / `cloud-init.yaml`, then load `scx_prism` | The **full bus end-to-end on a real kernel map**: `prismd` fills the pinned map and the identity-aware scheduler `scx_prism` reads the *same* map and schedules by identity, stamping `PRISM_FLAG_SCHED_MANAGED`. | A Linux **6.12+** kernel with `CONFIG_SCHED_CLASS_EXT` (Ubuntu 25.04, Fedora 41+, or a custom/scx kernel) + root + the scx toolchain. **Not available on the 5.15 WSL2 dev box.** |

Build everything first:

```sh
cd <repo> && GOFLAGS=-mod=mod go build ./...
```

---

## Tier A — control plane only (this repo, `go run`)

The point: you do **not** need BPF, a kernel feature, root, or a cluster to see
the identity logic work. The demo runs the genuine client-go informer over a
fake clientset and drives real `Pod` objects through the Kubernetes API
machinery.

```sh
cd <repo>
go run ./examples/livedemo        # narrated: create pods -> identities appear in the sink
# or run the daemon against an empty fake cluster (idle smoke test):
go run ./cmd/prismd -sim
# or benchmark the resolution path (percentiles, CIs):
go run ./bench/cmd/prismbench
```

What it proves: Pod create/update/delete -> identity allocation, replica
dedup (same labels share one identity), relabel re-allocation, and delete
release — all propagating into the **simulation sink** (an in-memory map that is
the ABI twin of the kernel map). The headline numbers in `REPORT.md` (e.g.
~19 ns Go map lookup, sub-µs propagation) come from this tier.

What it does **not** prove: nothing touches a real kernel BPF map or a real
scheduler here. The sink is userspace. For that, climb to B and C.

Prerequisites: just Go >= 1.22. Verified working on the 5.15 WSL2 dev box.

---

## Tier B — real cluster (`run-kind.sh` / `deploy/`)

The point: `prismd` runs as it would in production — as a DaemonSet, scoped to
its node via the downward API (`NODE_NAME` = `spec.nodeName`), watching the
**real** Kubernetes API and resolving live Pods.

`scripts/run-kind.sh` drives the whole flow on a [kind](https://kind.sigs.k8s.io)
cluster; the manifests live in `deploy/` (kustomize). The contract below is what
`prismd` itself requires, independent of how the cluster tier is packaged.

```sh
./scripts/run-kind.sh                 # create/reuse kind, docker build + kind load prismd, apply deploy/
kubectl -n prism-system logs ds/prismd -f
# under the hood it runs: kubectl apply -k deploy/   (namespace + RBAC + DaemonSet)
```

What `prismd` needs on the node (from the project spec):

1. **RBAC** to `list`/`watch` Pods.
2. **`NODE_NAME`** via the downward API (`fieldRef: spec.nodeName`) so the
   DaemonSet pod scopes to its own node's Pods (`-node $NODE_NAME`).
3. For the **cgroup keyer** (`-keyer=cgroup`, the default, real-kernel parity):
   read access to `/sys/fs/cgroup` to `stat` each pod's cgroup directory inode.
4. For the **BPF sink** (`-bpf=true`): `/sys/fs/bpf` mounted (bpffs) **and**
   `CAP_BPF`/`CAP_SYS_ADMIN` to create + pin the map. Without these, `prismd`
   logs the reason and falls back to the simulation sink automatically — the
   daemon stays fully functional, you just don't get a real kernel map.

What it proves: real-API Pod resolution, node scoping, RBAC, and (where the
node allows) a real pinned BPF map being populated. What it does **not** prove
on a stock kernel: the `sched_ext` scheduler — kind nodes are almost never
6.12+ with `CONFIG_SCHED_CLASS_EXT`. For the scheduler, you need Tier C.

Prerequisites: Docker + kind (or any cluster) and a Linux kernel. BPF map mode
needs bpffs + `CAP_BPF` on the node; the scheduler needs Tier C's kernel.

---

## Tier C — real `sched_ext` kernel (`provision-schedext-vm.sh`)

The point: the **whole bus, end-to-end, on a real kernel map** — including the
identity-aware scheduler. `prismd` creates and fills the pinned
`prism_identity` map; `scx_prism` (a real `sched_ext` BPF scheduler) opens the
**same** pinned map and schedules tasks by their Prism identity, stamping
`PRISM_FLAG_SCHED_MANAGED` so the net/observe facets can see the scheduler is
managing that identity. That cross-subsystem sharing is the architectural claim.

### Why a special kernel

`sched_ext` (the `CONFIG_SCHED_CLASS_EXT` scheduler class that lets BPF programs
implement a scheduler) landed in **Linux 6.12**. `bpf/scx_prism.bpf.c` is written
against the current 6.12+ scx API (`scx_bpf_dsq_insert`,
`scx_bpf_dsq_move_to_local`, `SCX_OPS_DEFINE`, `SCX_OPS_SWITCH_ALL`) and
`#include <scx/common.bpf.h>`, which ships with the scx toolchain — not with
`libbpf-dev`. It also needs a `vmlinux.h` that contains the `sched_ext` types
(`struct sched_ext_ops`, `SCX_SLICE_DFL`, ...), which only a 6.12+ kernel's BTF
has. See `bpf/README.md` for the gory details.

Kernels that work: Ubuntu 25.04 (6.14), Fedora 41+, or a custom/scx kernel built
with `CONFIG_SCHED_CLASS_EXT=y`. The **5.15 WSL2 dev box cannot do this tier** —
it has no `sched_ext`, so `scx_prism` neither compiles (no scx types in BTF, no
scx headers) nor loads (no scheduler class). That is expected and documented.

### Option 1 — provision an existing 6.12+ host

Run the provisioner on a fresh Ubuntu 25.04 or Fedora 41+ machine (bare metal or
VM). It is `set -euo pipefail`, distro- and arch-detecting, and idempotent:

```sh
sudo ./scripts/provision-schedext-vm.sh
```

It installs build deps (clang/llvm, libbpf-dev, bpftool, make, Go >= 1.22, git,
meson/ninja, cargo), verifies `CONFIG_SCHED_CLASS_EXT` and `/sys/kernel/sched_ext`,
mounts bpffs at `/sys/fs/bpf` if needed, clones + builds the scx toolchain
(`sched-ext/scx`) for its `<scx/common.bpf.h>` headers and the `scx_loader`,
regenerates `bpf/vmlinux.h` from the booted kernel's BTF, compiles
`bpf/compose_demo.bpf.o` and `bpf/scx_prism.bpf.o`, builds `prismd`, and prints
the exact load steps.

### Option 2 — spin a cloud VM that self-provisions on first boot

Use `scripts/cloud-init.yaml` as user-data. Pick a 6.12+ image. **Cross-ISA**
(the cross-ISA eval): the same config works on both architectures — boot it on
x86_64 (e.g. AWS c7i, GCP c4) **or** aarch64 / AWS Graviton (e.g. c7g, GCP c4a)
with the matching Ubuntu 25.04 image; the setup script auto-detects `uname -m`
and sets the clang `-D__TARGET_ARCH_*` define. Watch progress in
`/var/log/prism-setup.log`.

```sh
# AWS example (x86_64); use a Graviton instance type + arm64 AMI for aarch64.
aws ec2 run-instances \
  --image-id <ubuntu-25.04-amd64-ami> \
  --instance-type c7i.large \
  --user-data file://scripts/cloud-init.yaml \
  --key-name <key> --security-group-ids <sg>
```

### Load it (the exact commands)

After provisioning, the bus is exercised in three moves (the provisioner reprints
these with absolute paths). A `sched_ext` scheduler is loaded by a userspace
program that attaches its `struct_ops`:

1. **Producer** — `prismd` creates + pins the map and fills it from Pod events.
   Run as root (needs CAP_BPF to create/pin a BPF map), cgroup keyer for
   real-kernel parity with the scheduler's per-task cgroup key:

   ```sh
   sudo NODE_NAME=$(hostname) /opt/prism/prismd \
        -bpf=true -keyer=cgroup -cgroup-driver=systemd \
        -kubeconfig=/root/.kube/config         # or in-cluster
   sudo bpftool map show name prism_identity   # confirm the bus exists
   ```

2. **Scheduler** — load `scx_prism`, which opens the *same* pinned map. Two ways:

   - **scx_loader** (the standard scx way; built by the provisioner under
     `/opt/scx/build`). It is a D-Bus-driven service — see `/opt/scx/README.md`
     for the unit + config, then `sudo systemctl start scx_loader`.
   - **Direct libbpf/bpftool** (most direct for the demo): register and pin the
     struct_ops object —

     ```sh
     sudo bpftool struct_ops register /opt/prism/bpf/scx_prism.bpf.o \
          /sys/fs/bpf/prism/scx_prism_ops
     cat /sys/kernel/sched_ext/state          # -> "enabled"
     sudo rm /sys/fs/bpf/prism/scx_prism_ops  # detach / stop the scheduler
     ```

   sched_ext has a kernel watchdog: if the loader dies the kernel auto-reverts
   to the default scheduler, so loading `scx_prism` can never wedge the box.

3. **Verify the sharing** — with both running, dump the map and watch the
   scheduler's flag appear on identities it manages:

   ```sh
   sudo bpftool map dump name prism_identity   # look for PRISM_FLAG_SCHED_MANAGED
   ```

What it proves: the complete claim — one identity, three subsystems, one shared
kernel map, on a real `sched_ext` kernel, on both ISAs.

---

## What was verified where (honesty box)

| Claim | Where verified |
|-------|----------------|
| Tier A (`livedemo`, `prismd -sim`, bench) runs | Verified on the 5.15 WSL2 dev box (Go 1.24). |
| `prismd` builds static (`CGO_ENABLED=0`) | Verified here. |
| `compose_demo.bpf.o` compiles (net + observe) | Verified here with clang 18 (`-I bpf` + the local libbpf shim). |
| `scx_prism.bpf.o` compiles + loads, scheduler attaches | **Only on a 6.12+ sched_ext host.** Cannot run here (5.15, no `sched_ext`, no root). Tier C exists to make this real. |
| `provision-schedext-vm.sh` / `cloud-init.yaml` | Syntax-checked (`bash -n`) and YAML-validated here; **cannot be executed** on the 5.15 WSL2 box (no root, no 6.12 kernel, no cloud). Run on a real 6.12+ host. |
