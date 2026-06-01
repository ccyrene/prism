# `scx_prism_loader` — userspace loader for the Prism sched_ext scheduler

This is the userspace program that **loads and attaches** `bpf/scx_prism.bpf.c`
(the SCHED facet of the Prism identity bus) as a real
[sched_ext](https://docs.kernel.org/scheduler/sched-ext.html) `struct_ops`
scheduler on Linux **6.12+**.

```
open  ->  load  ->  attach struct_ops  ->  poll until SIGINT  ->  detach
```

A sched_ext scheduler is a `BPF_MAP_TYPE_STRUCT_OPS` map whose value is a
`struct sched_ext_ops`. **Attaching** that map (`bpf_map__attach_struct_ops`)
installs the scheduler system-wide; **destroying the returned link** uninstalls
it and reverts the kernel to its default scheduler (CFS/EEVDF). If the loader
dies without detaching, the kernel's sched_ext watchdog auto-reverts — a stuck
scx scheduler can never wedge the box.

---

## Where this runs

| Host | Compiles? | Loads/attaches? |
|---|---|---|
| Dev box (Linux 5.15 WSL2, no libbpf-dev, no root) | **No** | No |
| 6.12+ host (Ubuntu 25.04 / Fedora 41+) with libbpf-dev, root | **Yes** | **Yes** |

This file is **compile-only-on-6.12 by design.** On the dev box `clang
-fsyntax-only scx_prism_loader.c` fails at the very first include:

```
scx_prism_loader.c:81:10: fatal error: 'bpf/libbpf.h' file not found
   81 | #include <bpf/libbpf.h> // libbpf userspace API (open/load/attach); from libbpf-dev
      |          ^~~~~~~~~~~~~~
1 error generated.
```

That is the **expected libbpf-dev dependency, not a code defect** — it stops at
the header, before any of the loader's own code is parsed. `make
syntax-expected` reproduces and documents exactly this. The userspace loader
needs the full libbpf *library* (`-lbpf` + headers from `libbpf-dev`); the BPF
*program* it loads only needs the headers vendored under `bpf/include`.

---

## Build (on a 6.12+ host)

### 0. Provision + compile the BPF object

```sh
# Installs clang/libbpf-dev/bpftool, regenerates a 6.12 bpf/vmlinux.h from the
# running kernel's BTF, fetches the scx headers, and compiles bpf/scx_prism.bpf.o.
sudo scripts/provision-schedext-vm.sh
```

After this you have `bpf/scx_prism.bpf.o` and a `vmlinux.h` that contains
`struct sched_ext_ops` (the provisioner refuses to overwrite it otherwise).

### 1. Build the loader — generic object API (default, simplest)

Needs only the compiled `.bpf.o` at runtime; no skeleton, no bpftool at build:

```sh
make -C loader
# equivalently, the exact line:
clang -O2 -g -Wall loader/scx_prism_loader.c -lbpf -lelf -lz -o loader/scx_prism_loader
```

### 2. Build the loader — skeleton API (what upstream scx schedulers use)

Regenerates a typed skeleton with `bpftool`, then compiles with `-DUSE_SKEL`:

```sh
make -C loader skel
# equivalently, the exact lines:
bpftool gen skeleton bpf/scx_prism.bpf.o name scx_prism > loader/scx_prism.skel.h
clang -O2 -g -Wall -DUSE_SKEL -I loader loader/scx_prism_loader.c -lbpf -lelf -lz -o loader/scx_prism_loader
```

> The struct_ops map name the loader attaches (`prism_ops`) **must** match the
> `SCX_OPS_DEFINE(prism_ops, ...)` name in `bpf/scx_prism.bpf.c`. Both build
> modes look the ops map up by that name.

`make CC=gcc` builds with gcc; `pkg-config --cflags/--libs libbpf` is used
automatically if a non-default libbpf prefix is installed.

---

## Run (root; installs a system-wide scheduler)

```sh
# generic build: pass the BPF object path (default bpf/scx_prism.bpf.o)
sudo ./loader/scx_prism_loader bpf/scx_prism.bpf.o
# skeleton build: object is embedded, no path needed
sudo ./loader/scx_prism_loader
```

While attached, verify from another shell:

```sh
cat /sys/kernel/sched_ext/state          # -> enabled
cat /sys/kernel/sched_ext/root/ops        # -> prism
sudo bpftool map dump name prism_identity # the shared bus the scheduler reads
```

Press **Ctrl-C** (or send SIGTERM) to detach and restore the default scheduler.

For the scheduler to do anything identity-aware, the bus must be populated first
— run `prismd` (or the seed tool) so `prism_identity` has entries. See
`scripts/eval/README.md` and `scripts/provision-schedext-vm.sh` step 9.

---

## Failure modes you will actually see

| Symptom | Cause |
|---|---|
| `'bpf/libbpf.h' file not found` at build | `libbpf-dev` not installed (you are on a non-provisioned / dev box). |
| `bpf_object__load() failed` with a verifier log | BPF program rejected — e.g. an attempt to write the RDONLY_PROG map, or scx ABI mismatch. |
| `attach_struct_ops failed: Operation not supported` (EOPNOTSUPP) | Kernel has no `CONFIG_SCHED_CLASS_EXT` (not 6.12+, or sched_ext not enabled). |
| `prism_ops not found` | `.bpf.o` was built without the `SCX_OPS_DEFINE(prism_ops, ...)` symbol (wrong/old object). |

---

## Relationship to the rest of the harness

- `bpf/scx_prism.bpf.c` — the scheduler this loads (reads `prism_identity`).
- `scripts/provision-schedext-vm.sh` — gets a host to "can build + load".
- `scripts/eval/run-sched-eval.sh` — uses this loader to attach `scx_prism`,
  then measures a latency-sensitive workload vs CFS/EEVDF and `scx_layered`.
- `scripts/eval/README.md` — zero-to-results, in order.
