# Contributing to Prism

Thanks for your interest! Prism is a research prototype — a workload-identity bus
for Kubernetes-aware eBPF. Contributions, issues, and questions are welcome.

## Develop

The Go control plane builds and tests on any host (no root, no special kernel):

```sh
go build ./...
go test ./...          # pkg/abi, identity, sink, sync, … all run in userspace
go vet ./...
```

The eBPF consumers compile with `clang` against the in-repo `bpf/vmlinux.h` + libbpf
shim:

```sh
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -I bpf -I bpf/include \
      -c bpf/consumers/<name>.bpf.c -o /tmp/<name>.o
```

The **interesting in-kernel results** (the `sched_ext` scheduler, the 3-leg demo,
the benchmarks) need a **Linux 6.12+** host with `CONFIG_SCHED_CLASS_EXT`. See
[`docs/QUICKSTART.md`](docs/QUICKSTART.md) to run it and
[`docs/run-on-a-vm.md`](docs/run-on-a-vm.md) for a one-command VM setup.

## Ground rules

- **CI must pass** (`go build` / `go vet` / `go test -race` + the eBPF compile) —
  see `.github/workflows/ci.yml`.
- **The map value is a frozen ABI** (24 bytes, byte-identical Go ⇄ C). Don't change
  its layout; it's documented in [`spec/README.md`](spec/README.md) and cross-checked
  by `pkg/abi/abi_test.go`. New facets claim a *reserved* bit (append-only).
- **Licensing**: userspace is Apache-2.0; the in-kernel eBPF programs are GPL-2.0
  (they call GPL-only kernel helpers). Keep `SEC("license")="GPL"` on BPF code.

## Good first contributions

Writing a **consumer** is the easiest way in — it's ~3 lines against the bus; the
walkthrough + template are in [`bpf/consumers/README.md`](bpf/consumers/README.md).
Open areas (see the README's *Status*): cluster-wide identity coherence (a
[central coordinator](docs/central-coordinator-design.md)), ARM/aarch64 validation,
and a Prometheus exporter per consumer.

## PRs

Fork, branch, keep the change focused, make sure CI is green, and describe what you
changed and why. For anything touching the ABI or the daemon's write path, please
open an issue to discuss first.
