# Prism

**A workload-identity bus for Kubernetes-aware eBPF.**

![Prism architecture: the Kubernetes API feeds prismd, which derives one 24-bit identity per workload and publishes it into a single shared, read-only eBPF map (prism_identity) — the bus — read O(1) by a sched_ext scheduler, a tc/net-policy program, and a tracer.](docs/architecture.svg)

> `prismd` derives one 24-bit identity per workload and writes it into a single
> read-only eBPF map; **any number** of in-kernel consumers — `sched`, `net`,
> `trace`, and your own — read that one bus `O(1)`. The three legs shown are
> demonstrations, not a limit: a consumer is any eBPF program that reads
> `prism_identity` (see [`bpf/consumers/README.md`](bpf/consumers/README.md)).

A per-node daemon (`prismd`) turns each pod's labels, namespace, and service
account into a stable **24-bit workload identity** and publishes
`cgroup-id → identity` into **one** pinned, read-only eBPF map
(`prism_identity`). A `sched_ext` CPU scheduler, a `cgroup-skb` network program,
and a tracer all read that single map `O(1)` — the *same* identity across
subsystems, including the scheduler. The map is `BPF_F_RDONLY_PROG`: only the
daemon writes it; consumers can only read (verifier-enforced).

Measured in-kernel on Linux 6.17: an `scx_bpfland` retrofit that protects a
workload by its declared latency class, and all three subsystems reading one bus
at once.

## Quick start

Needs Linux ≥ 6.12 (`sched_ext`; tested on 6.17), `clang` + `libbpf-dev`,
`bpftool`, Go ≥ 1.24, and root to load BPF.

```sh
# build the daemon, BPF objects, scheduler, and the bpfland retrofit
scripts/build.sh
make -C loader
integrations/bpfland/build.sh

# run the identity-aware scheduler (Ctrl-C reverts to the default scheduler)
sudo ./loader/scx_prism_loader bpf/scx_prism.bpf.o

# watch all three legs (sched + net + trace) share one identity bus, live
sudo scripts/three-leg-demo.sh
```

- **New here? Start with [`docs/QUICKSTART.md`](docs/QUICKSTART.md)** — use it in 3 steps.
- Deploy `prismd` to a cluster: `kubectl apply -k deploy/`
- Reproduce the evaluation: `scripts/eval/README.md`

## How it works

| leg | program | uses the identity for |
|---|---|---|
| **sched** | `bpf/scx_prism.bpf.c`, `integrations/bpfland/` | per-task scheduling priority |
| **net** | `bpf/consumers/net_policy_prism.bpf.c` | per-packet attribution / policy |
| **trace** | `bpf/consumers/execsnoop_prism.bpf.c` | identity-tagged events |
| **sec** | `bpf/consumers/lsm_policy_prism.bpf.c` *(experimental)* | per-workload LSM allow/deny (`-EPERM`) |

The 24-byte map value (identity + a per-identity latency class/weight) is a
frozen ABI shared byte-for-byte by Go and C — see `spec/README.md`. A consumer's
own counters can be surfaced to Prometheus with a small exporter, e.g.
`cmd/prism-net-exporter` (`prism_net_bytes_total{identity="…"}`).

## Docs

- [`docs/QUICKSTART.md`](docs/QUICKSTART.md) — use Prism in 3 steps
- [`bpf/consumers/README.md`](bpf/consumers/README.md) — write your own consumer (the 3-line core)
- [`docs/verifying-the-legs.md`](docs/verifying-the-legs.md) — prove each leg works (and that the effect is caused by identity)
- [`docs/architecture.html`](docs/architecture.html) — diagrams, EN + TH
- [`docs/central-coordinator-design.md`](docs/central-coordinator-design.md) — design for cluster-wide identity coherence (proposed)
- [`docs/run-on-a-vm.md`](docs/run-on-a-vm.md) — run the in-kernel benchmarks on a throwaway VM (one command)
- [`scripts/eval/README.md`](scripts/eval/README.md) · [`scripts/eval/run-showcase.sh`](scripts/eval/run-showcase.sh) — reproduce the benchmarks

## Status

Research prototype. In-kernel results (the scheduler retrofit and 3-leg
coexistence) are measured on Linux 6.17; cluster-wide identity coherence and ARM
are open.

## License

Userspace is **Apache-2.0** (`LICENSE`); the in-kernel eBPF programs are
**GPL-2.0** (`bpf/LICENSE`) because they call GPL-only kernel helpers.
`integrations/bpfland/` is a GPL-2.0 derivative of `scx_bpfland` — see `NOTICE`.
