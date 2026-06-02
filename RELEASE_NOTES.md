**Prism v0.1.0** — first tagged release of the workload-identity bus for Kubernetes-aware eBPF.

`prismd` turns each pod's labels into one stable **24-bit identity** in a single shared, read-only eBPF map; a `sched_ext` scheduler, a network program, a tracer, and your own consumers all read the *same* identity `O(1)`.

### Highlights
- Per-node daemon + the shared `prism_identity` bus (`BPF_F_RDONLY_PROG` — verifier-enforced sole-writer).
- Consumers: **sched** (latency-class scheduling, incl. an `scx_bpfland` retrofit), **net** (per-packet attribution), **trace**, and an experimental **LSM security** leg.
- Reproducible eval harness (`scripts/eval/run-showcase.sh`) + a Prometheus exporter for the net consumer.
- Docs: QUICKSTART, write-your-own-consumer guide, verifying-the-legs runbook, architecture + facets diagrams.

### Status
Research prototype. In-kernel results measured on Linux 6.17; cluster-wide identity coherence and ARM are open. Not production-hardened.

### Install
```
go install github.com/ccyrene/prism/cmd/prismd@v0.1.0
```

