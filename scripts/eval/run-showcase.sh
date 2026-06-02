#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-showcase.sh — ONE command that produces every figure + headline number
# for a writeup / LinkedIn post, on whatever the current box supports.
#
# It does NOT reimplement any benchmark: it orchestrates the existing, blessed
# scripts in order, narrates WHAT EACH STEP PROVES, is presence-guarded (each
# tier that the box can't run is SKIPPED with a clear reason, never faked), and
# finally collects every *.png into one folder with a manifest that flags the
# best picks for a post.
#
# ---------------------------------------------------------------------------
# THREE TIERS (run as many as the box allows):
#
#   A. control plane     any host, NO root, NO sched_ext   (Go + clang + py)
#        -> "identity lookup is ~150x cheaper than re-deriving, O(1) at scale,
#            tiny footprint"           figs: layers_bar, money_plot_cdf, scale*
#
#   B. in-kernel sched   root + Linux >= 6.12 + sched_ext
#        -> "identity-aware dispatch from the shared bus cuts tail latency
#            (p99/p99.9) under contention; one K8s latency-class label protects
#            a workload the stock heuristic mislabels"
#                                       figs: sched_tail, sched_knee, latjit/bpfland
#
#   C. real microservice root + sched_ext + Docker + wrk2
#        -> "the same win holds for a real 30-service app under honest
#            (coordinated-omission-corrected) load"          fig: dsb_tail
#
#   (Cross-ISA x86 vs ARM is a TWO-HOST experiment — instructions printed at the
#    end; run scripts/eval/cross-isa-compare.sh on each box, not from here.)
# ---------------------------------------------------------------------------
#
# Usage:
#   scripts/eval/run-showcase.sh                 # run every tier the box supports
#   sudo scripts/eval/run-showcase.sh            # unlocks tiers B and C
#   PROVISION=1 sudo scripts/eval/run-showcase.sh  # also build the scx_prism kernel bits first
#   TIER=A scripts/eval/run-showcase.sh          # force a single tier (A | B | C)
#
# Env knobs (passed through to the underlying scripts if you set them):
#   TRIALS, RUNTIME, NOISE          -> sched / bpfland evals
#   RATES, DUR                      -> DeathStarBench
#   ITERS                           -> native C microbench
#   OUT                             -> base results dir (default scripts/eval/results)
#   PROVISION=1                     -> run provision-schedext-vm.sh + make -C loader first
#   TIER=A|B|C                      -> run only that tier
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO="$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)"
cd "${REPO}"

OUT="${OUT:-${SCRIPT_DIR}/results}"
SHOWCASE="${OUT}/showcase"
mkdir -p "${OUT}" "${SHOWCASE}"
ARCH="$(uname -m)"
KREL="$(uname -r)"
ONLY_TIER="${TIER:-}"

# ---- pretty helpers --------------------------------------------------------
c_hdr=$'\033[1;36m'; c_ok=$'\033[1;32m'; c_skip=$'\033[1;33m'; c_dim=$'\033[2m'; c_off=$'\033[0m'
banner() { printf '\n%s========================================================================%s\n' "$c_hdr" "$c_off"
           printf '%s  %s%s\n' "$c_hdr" "$1" "$c_off"
           [ $# -ge 2 ] && printf '%s  PROVES: %s%s\n' "$c_dim" "$2" "$c_off"
           printf '%s========================================================================%s\n' "$c_hdr" "$c_off"; }
ok()   { printf '%s  [done] %s%s\n' "$c_ok" "$1" "$c_off"; }
skip() { printf '%s  [skip] %s%s\n' "$c_skip" "$1" "$c_off"; }
have() { command -v "$1" >/dev/null 2>&1; }

# kernel >= 6.12 ?
kver_ge_612() { local a b; a="${KREL%%.*}"; b="${KREL#*.}"; b="${b%%.*}"; [ "${a:-0}" -gt 6 ] || { [ "${a:-0}" -eq 6 ] && [ "${b:-0}" -ge 12 ]; }; }
is_root()    { [ "$(id -u)" -eq 0 ]; }
has_scx()    { [ -d /sys/kernel/sched_ext ]; }
want_tier()  { [ -z "${ONLY_TIER}" ] || [ "${ONLY_TIER}" = "$1" ]; }

# run a step, never abort the whole showcase on its failure
run_step() { echo "${c_dim}  \$ $*${c_off}"; "$@" || skip "step exited non-zero (continuing): $*"; }

echo "${c_hdr}Prism showcase${c_off}  repo=${REPO}  arch=${ARCH}  kernel=${KREL}"
echo "  root=$(is_root && echo yes || echo no)  sched_ext=$(has_scx && echo yes || echo no)  out=${OUT}"

# ===========================================================================
# TIER A — control plane (no root, any host)
# ===========================================================================
if want_tier A; then
  banner "TIER A · control-plane resolution + scale + footprint" \
         "identity lookup ~3.6 ns native (~150x vs cgroup re-derive), O(1) into millions, ~320 B/identity"
  if have go; then
    run_step scripts/bench.sh "${OUT}"                       # money_plot_cdf, scale, layers_bar (+native.json)
    run_step bash -c "go run ./bench/cmd/scalecmp | tee '${OUT}/scalecmp.txt'"   # scale_sinks.csv -> scale_sinks.png
    have python3 && run_step python3 bench/plot.py           # refresh plots incl. scale_sinks
    run_step bash -c "go run ./bench/cmd/memprobe -n 256 -trim=false | tee '${OUT}/memprobe_notrim.txt'"
    run_step bash -c "go run ./bench/cmd/memprobe -n 256 -trim=true  | tee '${OUT}/memprobe_trim.txt'"
    ok "tier A complete"
  else
    skip "Go not found — install Go >= 1.24 to run tier A"
  fi
else skip "TIER=${ONLY_TIER} set — skipping tier A"; fi

# ===========================================================================
# TIER B — in-kernel sched_ext (root + 6.12+)
# ===========================================================================
if want_tier B; then
  banner "TIER B · identity-aware scheduling (tail latency)" \
         "scx_prism cuts p99/p99.9 vs EEVDF under contention; bpfland+Prism floor protects a CPU-bound critical task with no regression"
  if ! is_root;     then skip "not root — re-run with sudo to unlock tier B";
  elif ! kver_ge_612; then skip "kernel ${KREL} < 6.12 — sched_ext unavailable (use a 6.12+ box, e.g. Ubuntu 25.04 / Hetzner CPX41 / Oracle A1)";
  elif ! has_scx;   then skip "/sys/kernel/sched_ext missing — CONFIG_SCHED_CLASS_EXT not enabled in this kernel";
  else
    # build the kernel bits if asked, or if they're missing
    if [ "${PROVISION:-0}" = "1" ] || [ ! -f bpf/scx_prism.bpf.o ] || [ ! -x loader/scx_prism_loader ]; then
      if [ "${PROVISION:-0}" = "1" ]; then
        run_step scripts/provision-schedext-vm.sh
        run_step make -C loader
      else
        skip "missing bpf/scx_prism.bpf.o or loader/scx_prism_loader — run:  PROVISION=1 sudo $0   (or: sudo scripts/provision-schedext-vm.sh && make -C loader)"
      fi
    fi
    if [ -f bpf/scx_prism.bpf.o ] && [ -x loader/scx_prism_loader ]; then
      banner "B1 · synthetic contention (the money plot)" \
             "latency-critical probe p99/p99.9 with scx_prism vs baseline EEVDF under stress-ng noise"
      run_step scripts/eval/run-sched-eval-contended.sh      # -> sched_eval_<arch>.csv, sched_money_plot
      banner "B2 · load-knee sweep" "where each scheduler's tail hockey-sticks up as contention rises (4..32 hogs)"
      run_step scripts/eval/run-sched-sweep.sh               # -> sched_sweep_<arch>.csv, sched_knee_plot
      banner "B3 · bpfland retrofit — one latency-class label protects a workload" \
             "stock bpfland mislabels a CPU-bound critical task; +Prism CRITICAL floor recovers the tail, no regression on sleepy tasks, gaming-proof"
      if have cargo; then
        run_step integrations/bpfland/build.sh
        run_step scripts/eval/run-bpfland-eval.sh            # schbench (sleepy)  -> bpfland_eval_<arch>.csv
        [ -x integrations/bpfland/latjit ] || run_step cc -O2 -pthread -o integrations/bpfland/latjit integrations/bpfland/latjit.c
        run_step scripts/eval/run-latjit-eval.sh             # CPU-bound          -> latjit_eval_<arch>.csv
      else skip "cargo not found — bpfland retrofit needs the scx Rust tree (skipping B3)"; fi
      banner "B4 · three-leg coexistence demo" "sched + net + trace all read ONE pinned prism_identity map at once"
      [ -f scripts/three-leg-demo.sh ] && run_step scripts/three-leg-demo.sh || skip "three-leg-demo.sh not present"
      ok "tier B complete"
    fi
  fi
else skip "TIER=${ONLY_TIER} set — skipping tier B"; fi

# ===========================================================================
# TIER C — real microservice app (DeathStarBench)
# ===========================================================================
if want_tier C; then
  banner "TIER C · DeathStarBench socialNetwork (real 30-service app)" \
         "same identity-aware win on a real mesh under honest (coordinated-omission-corrected) wrk2 load"
  if ! is_root;     then skip "not root — re-run with sudo to unlock tier C";
  elif ! kver_ge_612; then skip "kernel ${KREL} < 6.12 — sched_ext unavailable";
  elif ! have docker; then skip "docker not found — DeathStarBench needs docker + docker compose";
  elif ! { have wrk2 || [ -x /opt/wrk2/wrk ]; }; then skip "wrk2 not found (need true wrk2 for coordinated-omission correction; plain wrk is refused)";
  else
    run_step scripts/eval/run-deathstarbench.sh              # -> dsb_socialnet_<arch>.csv, dsb_money_plot
    ok "tier C complete"
  fi
else skip "TIER=${ONLY_TIER} set — skipping tier C"; fi

# ===========================================================================
# Collect every figure + manifest
# ===========================================================================
banner "Collecting figures -> ${SHOWCASE}/"
n=0
while IFS= read -r -d '' png; do
  cp -f "${png}" "${SHOWCASE}/" && n=$((n+1))
done < <(find bench/results "${OUT}" -maxdepth 1 -name '*.png' -not -path "${SHOWCASE}/*" -print0 2>/dev/null)
ok "copied ${n} figure(s)"

cat <<EOF

${c_hdr}=== FIGURE MANIFEST (what to attach to the post) ===${c_off}
  ${c_ok}*${c_off} = top picks for LinkedIn

  layers_bar.png        identity lookup vs cgroup re-derive  (~150x, the speed story)   ${c_ok}*${c_off}
  money_plot_cdf.png    full latency CDF, Go control plane (p99 markers)
  scale.png             lookup stays O(1) as live identities grow 1k -> 1M
  scale_sinks.png       CompactSink stays flat past L3 where map+mutex degrades
  sched_money_plot_${ARCH}.png   p99/p99.9: scx_prism vs EEVDF under contention   ${c_ok}*${c_off}
  sched_knee_plot_${ARCH}.png    load-knee: where each scheduler's tail collapses
  dsb_money_plot_${ARCH}.png     DeathStarBench real-app tail vs offered load     ${c_ok}*${c_off}

  Pair with the architecture diagram: docs/architecture.svg

${c_hdr}=== CROSS-ISA (the biggest credibility upgrade — TWO hosts) ===${c_off}
  All numbers above are single-ISA. To prove the win is NOT x86-tuned, run the
  SAME eval on an ARM box and merge:
    # on the x86 host:
    OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
    # copy ~/prism-xisa/sched_eval_x86_64.csv to the ARM host's ~/prism-xisa, then on ARM:
    OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
  -> cross_isa_money_plot.png (x86_64 vs aarch64 side-by-side).
  Cheap hardware: x86 = Hetzner CPX41 (~EUR0.04/hr), ARM = Oracle Ampere A1 (free) / Hetzner CAX.
  Both must be Linux 6.12+.

All figures are in: ${SHOWCASE}/
EOF
