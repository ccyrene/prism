# Prism Quickstart — use it in 3 steps

Prism gives every Kubernetes workload a stable **name tag** (a 24-bit number) that
the *kernel* can read instantly. In-kernel subsystems — the CPU scheduler, a
network program, a security hook — then treat each workload differently, driven by
plain Kubernetes labels, **with no application changes**.

Mental model: `prismd` is the **tagger**; the things that read the tag and act on
it (`sched` / `net` / `sec` / `trace`) are **consumers**. You install the tagger,
label your workloads, and turn on the consumers you want.

> Needs Linux ≥ 6.12 (`sched_ext`; tested on 6.17), `clang` + `libbpf-dev`,
> `bpftool`, Go ≥ 1.24, and root to load BPF.

---

## 1. Run the tagger (`prismd`)

**On a cluster** — one Pod per node (DaemonSet):

```sh
kubectl apply -k deploy/
```

**On a single dev box** — build, then start an identity-aware scheduler (it pins
the shared tag map `/sys/fs/bpf/prism_identity` for every consumer to read):

```sh
scripts/build.sh
make -C loader
sudo ./loader/scx_prism_loader bpf/scx_prism.bpf.o    # Ctrl-C reverts to the default scheduler
```

## 2. Label a workload with your intent

Add a label to the Deployment — `prismd` stamps it into that workload's tag:

```yaml
spec:
  template:
    metadata:
      labels:
        prism.io/latency-class: critical   # critical | normal | batch
        # prism.io/weight: "96"            # optional 1..127, refines the class
```

That's the whole "ask": you declare *what matters*; the kernel does the rest.

## 3. Turn on a consumer that acts on the tag

The scheduler is the flagship consumer — once it's loaded (step 1, dev box), a
`critical` workload is protected from CPU starvation under contention, automatically.

Want to *see* all three demo legs (`sched + net + trace`) read the **one** tag map
at once, live:

```sh
sudo scripts/three-leg-demo.sh
```

---

## Add your own consumer (≈ 3 lines)

A consumer is any eBPF program that reads the tag. The Prism-specific part is tiny:

```c
#include "prism_maps.bpf.h"   // the shared tag map (read-only)
#include "libprism.bpf.h"     // the reader helpers
__u32 id = prism_id(prism_identity_of_current());   // which workload? (0 = off-bus)
```

Then branch / count / allow-deny by `id`, keeping your own state in your own map.
Full walkthrough + a copy-me template: [`bpf/consumers/README.md`](../bpf/consumers/README.md).

## Check it's actually working

Each consumer keeps its own counters and has an observable effect. The runbook
shows, per leg, the exact commands to confirm it ran, read the right tag, and made
the right decision — **and** the with/without-identity control that proves the
effect is caused by the tag, not luck: [`docs/verifying-the-legs.md`](verifying-the-legs.md).

## Surface a consumer's data to Prometheus

`prismd` already exposes its own health on `/metrics` (`prism_identities_allocated_total`,
`prism_propagation_seconds`, …). To export a *consumer's* per-workload counters,
run a small exporter — e.g. the net leg's:

```sh
go build -o prism-net-exporter ./cmd/prism-net-exporter
sudo ./prism-net-exporter            # :9465/metrics -> prism_net_bytes_total{identity="..."}
```

---

**Where to go next:** [architecture](architecture.html) ·
[write a consumer](../bpf/consumers/README.md) ·
[verify the legs](verifying-the-legs.md) ·
[cluster-wide identity (design)](central-coordinator-design.md) ·
[run the benchmarks on a VM](run-on-a-vm.md) ·
[reproduce the benchmarks](../scripts/eval/README.md)
