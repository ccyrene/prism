# Run the benchmarks on a VM

The control-plane numbers (identity lookup, scale, footprint) run on any host with
no root. The **interesting** in-kernel results — identity-aware scheduling cutting
tail latency, and the real-microservice (DeathStarBench) run — need root and a
`sched_ext` kernel, so they belong on a throwaway VM (loading a `sched_ext`
scheduler replaces the host CPU scheduler system-wide for the duration; don't do it
on a machine you care about).

`scripts/eval/run-showcase.sh` orchestrates everything in tiers, is presence-guarded
(skips, never fakes, what the box can't run), and collects every figure into one
folder with a manifest of the best picks for a writeup.

---

## 1. Get a VM

- **Kernel ≥ 6.12** with `CONFIG_SCHED_CLASS_EXT` (`sched_ext`). Ubuntu 25.04 ships it.
- Cheap options: **Hetzner CPX41** (x86, ~€0.04/hr) or **Oracle Ampere A1** (ARM,
  always-free — also unlocks the cross-ISA comparison below).
- **8+ vCPU** makes the scheduler tail-latency plot most dramatic (the effect scales
  with contention).

Confirm sched_ext is present:

```sh
ls /sys/kernel/sched_ext        # should exist
uname -r                        # should be 6.12+
```

## 2. One-time setup

```sh
# governor = performance — important for stable tail latency (the scripts don't set it)
echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor >/dev/null

sudo apt-get update && sudo apt-get install -y stress-ng jq git
git clone https://github.com/ccyrene/prism && cd prism
```

`provision-schedext-vm.sh` (run for you by the showcase below, or standalone)
installs `clang`/`libbpf-dev`/`bpftool`/Go, clones+builds the `sched-ext/scx`
toolchain, regenerates `bpf/vmlinux.h` from the running kernel, and builds
`bpf/scx_prism.bpf.o` + `prismd`.

## 3. Run everything the box supports (one command)

```sh
PROVISION=1 sudo scripts/eval/run-showcase.sh
```

This provisions the scx toolchain, builds, then runs:

| tier | needs | proves | figures |
|---|---|---|---|
| **A** control-plane | nothing (no root) | identity lookup ~150× cheaper than re-deriving, O(1) at scale | `layers_bar`, `scale`, `money_plot_cdf` |
| **B** sched | root + 6.12+ sched_ext | p99/p99.9 beat EEVDF under contention; bpfland+Prism protects a CPU-bound critical task | `sched_money_plot`, `sched_knee_plot` |
| **C** DSB | + Docker + wrk2 | same win on a real 30-service app under honest (CO-corrected) load | `dsb_money_plot` |

Knobs (pass-through): `TRIALS`, `RUNTIME`, `NOISE`, `RATES`, `DUR`. Force a single
tier with `TIER=A|B|C`.

## 4. Collect the figures

```sh
ls scripts/eval/results/showcase/        # all PNGs + a manifest of the best picks
```

The manifest flags the top picks for a post (pair them with `docs/architecture.svg`).

---

## Optional: DeathStarBench (the 228× number — heavier)

Tier C needs Docker + `wrk2` (a coordinated-omission-correct load generator; plain
`wrk` is refused). DeathStarBench is auto-cloned on first run. Needs several GB RAM
(27 microservices).

```sh
sudo apt-get install -y docker.io
git clone https://github.com/giltene/wrk2 /tmp/wrk2 && make -C /tmp/wrk2
sudo mkdir -p /opt/wrk2 && sudo cp /tmp/wrk2/wrk /opt/wrk2/wrk
# then re-run the showcase (or just: sudo scripts/eval/run-deathstarbench.sh)
```

## Optional: cross-ISA (the biggest credibility upgrade)

All single-ISA numbers are x86-only today. To prove the win isn't x86-tuned, run the
SAME eval on an ARM box and merge:

```sh
# on the x86 host:
OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
# copy ~/prism-xisa/sched_eval_x86_64.csv to the ARM host's ~/prism-xisa, then on ARM:
OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
# -> cross_isa_money_plot.png (x86_64 vs aarch64, side by side)
```

---

## What "good" looks like (so you know it worked)

The raw rows are in `scripts/eval/results/`. Sanity-check before trusting a plot:

```sh
cat scripts/eval/results/sched_eval_*.csv        # scx_prism p99 well below baseline AND scx_prism_nobus
cat scripts/eval/results/dsb_socialnet_*.csv     # (Tier C) scx_prism tail flat where baseline collapses
```

- The `scx_prism_nobus` row (scheduler attached, **bus empty**) is the control: if
  `scx_prism` only beats it when the bus is seeded, the win is the identity routing,
  not merely "running scx". A skipped leg (empty CSV) is **not** a pass.
- A flat result on an **idle** box is expected (no contention → no differentiation);
  the harness manufactures load on purpose.

Full per-leg verification (including the false-pass traps) is in
[`verifying-the-legs.md`](verifying-the-legs.md).

Reference numbers measured on Linux 6.17 (hardware-dependent — yours will differ):
sched p99 ≈ **13.5× lower** than EEVDF under contention; DeathStarBench p99 ≈
**228× lower** at the load where the baseline collapses.
