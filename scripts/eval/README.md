# Prism evaluation runbook — zero to results on a real 6.12+ host

This is **Component B**: the ready-to-run evaluation harness for the experiments
the 5.15 WSL2 dev box *cannot* run (load + attach a `sched_ext` scheduler, drive
a real microservice workload, compare schedulers, and do it across ISAs). Every
script here is presence-guarded so it degrades gracefully, but the *interesting*
results require a Linux **6.12+** host with `CONFIG_SCHED_CLASS_EXT`.

Provision the box with `scripts/provision-schedext-vm.sh`, then run the scripts
in **this** directory in order. See the cost notes for what each tier costs and
the cheapest hardware to get it (≈ $0–10 total; Oracle free Ampere A1 for ARM).

---

## TL;DR — the whole ladder

```sh
# 0. Get a 6.12+ box (Hetzner CPX41 x86, your own Ubuntu 25.04, or Oracle A1 ARM).
sudo scripts/provision-schedext-vm.sh        # builds bpf/scx_prism.bpf.o, vmlinux.h, prismd

# 1. Build the userspace loader for scx_prism.
make -C loader                               # -> loader/scx_prism_loader  (needs libbpf-dev)

# 2. Synthetic scheduler eval (baseline vs scx_layered vs scx_prism).
sudo scripts/eval/run-sched-eval.sh          # -> results/sched_*_<arch>.{csv,png}

# 3. Realistic microservice eval (DeathStarBench socialNetwork + wrk2).
sudo scripts/eval/run-deathstarbench.sh      # -> results/dsb_*_<arch>.{csv,png}

# 4. Cross-ISA: run #2 on x86 AND aarch64, merge.
OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh   # on each arch -> cross_isa_*.{csv,png}
```

All outputs land in `scripts/eval/results/` (override with `OUT=`).

---

## What each step proves

| Step | Script | Proves | Needs |
|---|---|---|---|
| 0 | `provision-schedext-vm.sh` | The toolchain builds `scx_prism.bpf.o` against a **real 6.12 BTF** + scx headers, and `prismd` builds. | 6.12+ kernel, root. |
| 1 | `loader/Makefile` → `scx_prism_loader` | A standard libbpf loader can **load + attach** `scx_prism` as a `struct_ops` scheduler. | `libbpf-dev` ≥ 1.0. |
| 2 | `run-sched-eval.sh` | Under a latency-sensitive workload, `scx_prism`'s **p99/p99.9 beat vanilla CFS/EEVDF** (and are competitive with `scx_layered`) — i.e. identity-aware dispatch from the shared bus measurably helps tail latency. | 6.12+ + sched_ext. |
| 3 | `run-deathstarbench.sh` | The same holds for a **real 30-service app** under honest (coordinated-omission-corrected) load: a higher load "knee" before tail collapse. | Docker + **wrk2** + sched_ext. |
| 4 | `cross-isa-compare.sh` | The **identical portable policy** delivers the win on **both x86_64 and aarch64** (only `clang -D__TARGET_ARCH_*` differs). | An x86 host **and** an ARM host. |

---

## Step 0 — provision a real sched_ext kernel

```sh
sudo scripts/provision-schedext-vm.sh
```

Idempotent. It detects the distro/arch, installs `clang`/`libbpf-dev`/`bpftool`/
Go, verifies `CONFIG_SCHED_CLASS_EXT`, mounts bpffs at `/sys/fs/bpf`, clones +
builds the [`sched-ext/scx`](https://github.com/sched-ext/scx) toolchain (for
`<scx/common.bpf.h>` **and** reference schedulers like `scx_layered`), regenerates
`bpf/vmlinux.h` from the running kernel's BTF, compiles `bpf/scx_prism.bpf.o`, and
builds `prismd`. It does **not** auto-load anything privileged; it prints the
exact next commands.

> If `/sys/kernel/sched_ext` is missing afterward, the kernel lacks sched_ext —
> you are not on 6.12+. The eval scripts below will run the **baseline leg only**
> and tell you which legs they skipped and why.

**The bus** is one pinned map: `/sys/fs/bpf/prism/identity` (map name
`prism_identity`). See `spec/README.md`.

---

## Step 1 — build the scx_prism loader

```sh
make -C loader            # generic object API (default)
# or, the upstream-scx skeleton way:
make -C loader skel       # regenerates loader/scx_prism.skel.h via bpftool
```

This produces `loader/scx_prism_loader`, which does
`open → load → attach struct_ops → poll until SIGINT → detach`. See
`loader/README.md` for both build modes, the exact `clang` lines, and the failure
table. (On the dev box this **won't** build — `bpf/libbpf.h` is absent; that is
the documented libbpf-dev dependency, not a code bug.)

To attach it directly, with the bus populated by `prismd`:

```sh
# producer (fills + pins the map from Pod events):
sudo NODE_NAME=$(hostname) ./prismd -bpf=true -keyer=cgroup \
     -cgroup-root=/sys/fs/cgroup -cgroup-driver=systemd -kubeconfig=/root/.kube/config
# scheduler (reads the SAME map, schedules by identity):
sudo ./loader/scx_prism_loader bpf/scx_prism.bpf.o
# verify:
cat /sys/kernel/sched_ext/state          # -> enabled
cat /sys/kernel/sched_ext/root/ops        # -> prism
sudo bpftool map dump name prism_identity
```

`run-sched-eval.sh` / `run-deathstarbench.sh` do the attach/detach for you.

---

## Step 2 — synthetic scheduler eval (the money plot)

```sh
sudo scripts/eval/run-sched-eval.sh
# more rigor:
sudo TRIALS=20 DURATION=20 WORKLOAD=schbench scripts/eval/run-sched-eval.sh
```

For each scheduler — **baseline** (no scx; vanilla CFS/EEVDF), **scx_layered**
(if the scx toolchain built it), **scx_prism** — it runs a latency-sensitive
workload `TRIALS` times and records per-trial p50/p99/p99.9.

- **Workload** (auto-picked, `WORKLOAD=` to force): `schbench` (Facebook's
  scheduler wakeup-latency benchmark — reports tail percentiles directly, the
  best signal) → `hackbench` → a portable built-in pipe ping-pong it compiles on
  demand (so it *always* produces a baseline number even with no extra tools).
- **Stats**: per-scheduler median of each percentile across trials **plus a
  percentile-bootstrap 95% CI on the median** — the same heavy-tail-safe
  methodology as `bench/cmd/prismbench/stats.go` (fixed seed → reproducible).

Outputs in `results/`:
- `sched_eval_<arch>.csv` — raw, long format: `scheduler,arch,kernel,workload,trial,p50_us,p99_us,p999_us`
- `sched_eval_summary_<arch>.csv` — median + 95% CI per (scheduler, percentile)
- `sched_money_plot_<arch>.png` — grouped bars (log y) of p50/p99/p99.9 per
  scheduler, error bars = bootstrap CI.

**Read it**: if `scx_prism`'s p99/p99.9 bars sit materially below `baseline`
(and at/below `scx_layered`), identity-aware dispatch helped the tail.

> Populate the bus first (`prismd` or a seed) so `scx_prism` has identities to
> act on. With an empty bus every task resolves to UNKNOWN → the normal lane, and
> `scx_prism` ≈ baseline by construction — the script warns you when the bus is
> empty.

---

## Step 3 — DeathStarBench socialNetwork under wrk2

```sh
sudo scripts/eval/run-deathstarbench.sh
sudo RATES="500 1000 2000 4000 8000" DURATION=30 scripts/eval/run-deathstarbench.sh
```

Clones [DeathStarBench](https://github.com/delimitrou/DeathStarBench), brings up
the `socialNetwork` compose (≈30 microservices), and drives it with **wrk2** at a
**fixed request rate**, sweeping the offered load and recording p50/p99/p99.9 per
(rate, scheduler).

### Why wrk2 and not wrk — coordinated omission (CO)

Plain `wrk` is closed-loop: a connection sends its next request only after the
previous response returns. When the server stalls, `wrk` stalls *with* it and
simply stops issuing requests during the stall — so the requests that *would*
have been slow are never sent, and the reported p99/p99.9 are wildly optimistic
(often off by orders of magnitude, exactly where it matters). This is Gil Tene's
**Coordinated Omission**. **wrk2** is constant-throughput: you set `-R <rate>` and
it schedules each request at its *intended* send time, measuring latency from
that intended start even through a stall — an **honest** tail. For a scheduling
paper, reporting `wrk` tails would be malpractice; the script *requires* wrk2,
refuses to silently substitute `wrk`, and prints the build command if it is
missing.

Outputs in `results/`:
- `dsb_socialnet_<arch>.csv` — `scheduler,arch,kernel,rate_rps,p50_ms,p99_ms,p999_ms`
- `dsb_money_plot_<arch>.png` — latency (log) vs offered load (log), one curve per
  (scheduler, percentile).

**Read it**: the load at which a curve "hockey-sticks" upward is the knee; a
**higher knee** (and lower tail before it) under `scx_prism` means it sustains
more load before tail-latency collapse.

---

## Step 4 — cross-ISA (x86_64 vs aarch64)

```sh
# on the x86 6.12 box:
OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
# copy ~/prism-xisa/sched_eval_x86_64.csv to the ARM box's ~/prism-xisa, then on ARM:
OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
# (or, once both CSVs sit in OUT, just merge:)
MERGE_ONLY=1 OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
```

Runs `run-sched-eval.sh` on the current host (CSV auto-tagged by `uname -m`), and
when CSVs from **both** arches are present in `OUT`, merges them and emits:
- `cross_isa_summary.csv` — median + 95% CI per (arch, scheduler, percentile)
- `cross_isa_money_plot.png` — p99 by scheduler, x86 vs ARM bars side by side.

**Get the ARM host for free**: Oracle Cloud Always-Free **Ampere A1** (up to 4
OCPU / 24 GB, $0 forever), or Hetzner **CAX** (≈ €0.05/hr). See the cost notes.

**Read it**: matching qualitative tail-latency improvement on both ISAs shows the
identity-aware policy is ISA-agnostic (only the `clang -D__TARGET_ARCH_*` build
flag differs between them).

---

## Expected outputs (all under `scripts/eval/results/`)

| File | From | Contents |
|---|---|---|
| `sched_eval_<arch>.csv` | step 2 | raw per-trial p50/p99/p99.9 per scheduler |
| `sched_eval_summary_<arch>.csv` | step 2 | median + bootstrap 95% CI on the median |
| `sched_money_plot_<arch>.png` | step 2 | grouped tail-latency bars per scheduler |
| `dsb_socialnet_<arch>.csv` | step 3 | tail latency vs offered rate per scheduler |
| `dsb_money_plot_<arch>.png` | step 3 | rate-vs-tail curves (the knee plot) |
| `sched_eval_cross_isa.csv` | step 4 | merged x86 + ARM trials |
| `cross_isa_summary.csv` | step 4 | per (arch, scheduler) median + CI |
| `cross_isa_money_plot.png` | step 4 | x86 vs ARM p99 side by side |

---

## Safety + gotchas

- **A stuck scheduler cannot wedge the box**: sched_ext's kernel watchdog reverts
  to the default scheduler if the loader dies; the scripts also `trap` cleanup to
  detach on exit/Ctrl-C.
- **Root**: loading a system-wide scheduler and creating a pinned BPF map are
  privileged. Run the eval scripts with `sudo`.
- **Quiesce the box** before measuring tails — close other workloads; pin the
  governor to `performance` for stable numbers.
- **Empty bus ⇒ no effect**: see step 2's note. Always populate `prism_identity`
  (via `prismd` or a seed) before the `scx_prism` legs.
- **Reproducibility**: the bootstrap CI uses a fixed seed (matches `stats.go`), so
  re-summarising the same CSV is deterministic.

See also: `scripts/README.md` (the three-tier ladder), `loader/README.md`
(loader build + failure table), the cost notes (hardware + cost).
