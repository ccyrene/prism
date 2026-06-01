#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-sched-eval-contended.sh — the load-bearing experiment:
#   "Does identity-aware sched_ext dispatch from the Prism bus measurably help a
#    latency-sensitive workload's tail latency under CPU contention?"
#
# This is the contended/seeded companion to run-sched-eval.sh. The plain harness
# runs schbench standalone, which on an idle box shows no differentiation: with
# spare CPUs every wakeup finds an idle core, so the scheduler's lane choice is
# irrelevant and scx_prism == baseline by construction. To actually exercise the
# policy we (1) create CPU CONTENTION and (2) tell the bus which workload is
# latency-sensitive, then measure that workload's wakeup-latency tail.
#
# Experiment (per scheduler leg):
#   * PROBE  : schbench (Facebook's scheduler wakeup-latency benchmark) runs in
#              its own cgroup `prism_probe`. Its wakeup-latency p50/p99/p99.9 are
#              the metric (time from wakeup to actually running — pure scheduler).
#   * NOISE  : stress-ng CPU hogs saturate every CPU from a SEPARATE cgroup
#              `prism_noise`, so woken probe threads must compete to run.
#   * BUS    : for the scx_prism *seeded* leg we write
#              prism_identity[cgroup_id(prism_probe)] = PRISM_ID_MIN_DYNAMIC (256)
#              via a userspace map-update (the same syscall path prismd uses;
#              unaffected by BPF_F_RDONLY_PROG). scx_prism then routes the probe
#              to its strict-priority HIGH lane (2x slice); the noise (UNKNOWN
#              identity) stays in the NORM lane.
#
# Legs measured (CSV `scheduler` column):
#   baseline          vanilla EEVDF, no scx                              (control)
#   scx_prism_nobus   scx_prism attached, EMPTY bus (probe NOT seeded)   (control:
#                     isolates "identity routing" from "just using scx_prism")
#   scx_prism         scx_prism attached, bus seeded -> probe in HIGH lane (treatment)
#   scx_layered       reference scx scheduler, default config            (if built)
#
# Reading it: if scx_prism's p99/p99.9 sit materially BELOW baseline AND below
# scx_prism_nobus, then identity-aware dispatch from the shared bus — not merely
# running an scx scheduler — is what protected the tail. A null result (no gap)
# is reported honestly.
#
# Output (same schema as run-sched-eval.sh, so sched_eval_plot.py works):
#   results/sched_eval_<arch>.csv          per-trial p50/p99/p99.9
#   results/sched_eval_summary_<arch>.csv  median + bootstrap 95% CI
#   results/sched_money_plot_<arch>.png    the money plot
#
# Run as root (loads a system-wide scheduler, creates cgroups, writes a BPF map).
# Env knobs:
#   TRIALS=30 RUNTIME=5 WARMUP=1 MSG=2 WORKERS=8 NOISE=<nproc> FAST_ID=256
#   OUT=<dir> PRISM_OBJ=... LOADER=... SCX_DIR=/opt/scx SKIP_PLOT=1 SETTLE=3

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)}"

TRIALS="${TRIALS:-30}"
RUNTIME="${RUNTIME:-5}"
WARMUP="${WARMUP:-1}"
MSG="${MSG:-2}"
WORKERS="${WORKERS:-8}"
NOISE="${NOISE:-$(nproc)}"
FAST_ID="${FAST_ID:-256}"          # PRISM_ID_MIN_DYNAMIC — scx_prism's HIGH tier
SETTLE="${SETTLE:-3}"
OUT="${OUT:-${SCRIPT_DIR}/results}"
SCX_DIR="${SCX_DIR:-/opt/scx}"
PRISM_OBJ="${PRISM_OBJ:-${REPO_DIR}/bpf/scx_prism.bpf.o}"
LOADER="${LOADER:-${REPO_DIR}/loader/scx_prism_loader}"
ARCH="$(uname -m)"; KREL="$(uname -r)"
CG=/sys/fs/cgroup
PROBE_CG="${CG}/prism_probe"
NOISE_CG="${CG}/prism_noise"
WL="schbench-contended"

# bpftool: prefer a working one on PATH, else the version-matched binary.
BPFTOOL="bpftool"
command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1 || \
  BPFTOOL="$(ls /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | head -1)"

mkdir -p "${OUT}"
CSV="${OUT}/sched_eval_${ARCH}.csv"

step(){ echo; echo "==> $*"; }
info(){ echo "    $*"; }
warn(){ echo "    WARN: $*" >&2; }
have(){ command -v "$1" >/dev/null 2>&1; }
[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: must run as root"; exit 1; }
[[ -d /sys/kernel/sched_ext ]] || { echo "ERROR: /sys/kernel/sched_ext missing (need kernel >= 6.12)"; exit 1; }
have schbench || { echo "ERROR: schbench not found"; exit 1; }
have stress-ng || { echo "ERROR: stress-ng not found"; exit 1; }

echo "repo=${REPO_DIR} arch=${ARCH} kernel=${KREL}"
echo "trials=${TRIALS} runtime=${RUNTIME}s warmup=${WARMUP}s probe=schbench(-m ${MSG} -t ${WORKERS}) noise=stress-ng(--cpu ${NOISE})"
echo "out=${OUT}"

# ---------------------------------------------------------------------------
# cgroups for probe and noise (distinct cgroup ids => distinct bus keys).
# ---------------------------------------------------------------------------
mkdir -p "${PROBE_CG}" "${NOISE_CG}"
PROBE_ID="$(stat -c %i "${PROBE_CG}")"
NOISE_ID="$(stat -c %i "${NOISE_CG}")"
info "probe cgroup id = ${PROBE_ID}   noise cgroup id = ${NOISE_ID}"

NOISE_PID=""; PRISM_PID=""; LAYERED_PID=""
cleanup() {
  stop_noise || true
  detach_scx || true
  # move any stragglers out, then remove cgroups
  for c in "${PROBE_CG}" "${NOISE_CG}"; do
    [[ -d "$c" ]] || continue
    if [[ -f "$c/cgroup.procs" ]]; then
      while read -r p; do echo "$p" > "${CG}/cgroup.procs" 2>/dev/null || true; done < "$c/cgroup.procs"
    fi
    rmdir "$c" 2>/dev/null || true
  done
}
trap cleanup EXIT INT TERM

start_noise() {
  ( echo $BASHPID > "${NOISE_CG}/cgroup.procs"
    exec stress-ng --cpu "${NOISE}" --cpu-method matrixprod --timeout 1000s ) >/dev/null 2>&1 &
  NOISE_PID=$!
  sleep 1   # let the hogs spin up and saturate
}
stop_noise() {
  [[ -n "${NOISE_PID}" ]] || return 0
  kill "${NOISE_PID}" 2>/dev/null || true
  pkill -P "${NOISE_PID}" 2>/dev/null || true
  pkill -f "stress-ng" 2>/dev/null || true
  wait "${NOISE_PID}" 2>/dev/null || true
  NOISE_PID=""
}

# run ONE schbench trial inside the probe cgroup; echo "p50 p99 p999" (usec).
run_probe_once() {
  local jf="${OUT}/.sb.$$.json"
  ( echo $BASHPID > "${PROBE_CG}/cgroup.procs"
    exec schbench -m "${MSG}" -t "${WORKERS}" -r "${RUNTIME}" -w "${WARMUP}" -i 3600 -j "${jf}" ) >/dev/null 2>&1 || true
  local p50 p99 p999
  p50="$(jq -r '.int."wakeup_latency_pct50.0" // 0' "${jf}" 2>/dev/null)"
  p99="$(jq -r '.int."wakeup_latency_pct99.0" // 0' "${jf}" 2>/dev/null)"
  p999="$(jq -r '.int."wakeup_latency_pct99.9" // 0' "${jf}" 2>/dev/null)"
  rm -f "${jf}"
  echo "${p50:-0} ${p99:-0} ${p999:-0}"
}

attach_prism() {
  [[ -x "${LOADER}" && -f "${PRISM_OBJ}" ]] || { warn "loader/obj missing"; return 1; }
  "${LOADER}" "${PRISM_OBJ}" > "${OUT}/.prism_loader.log" 2>&1 &
  PRISM_PID=$!
  sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]] || {
    warn "scx_prism did not enable (see ${OUT}/.prism_loader.log)"; return 1; }
  return 0
}
seed_bus() {  # seed prism_identity[PROBE_ID] = {identity=FAST_ID, flags=SCHED_MANAGED}
  local key val
  key="$(u64_le_hex "${PROBE_ID}")"
  val="$(u32_le_hex "${FAST_ID}") $(u32_le_hex 2) 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00"
  ${BPFTOOL} map update name prism_identity key hex ${key} value hex ${val} 2>&1 | sed 's/^/    seed: /' || \
    { warn "bus seed failed"; return 1; }
  info "seeded prism_identity[${PROBE_ID}] = identity ${FAST_ID} (HIGH lane)"
  ${BPFTOOL} map dump name prism_identity 2>/dev/null | sed 's/^/    map: /' | head -4
}
find_layered() {
  have scx_layered && { command -v scx_layered; return 0; }
  local c
  c="$(find "${SCX_DIR}" -type f -name scx_layered -perm -u+x 2>/dev/null | head -1 || true)"
  [[ -n "$c" ]] && { echo "$c"; return 0; }; return 1
}
attach_layered() {
  local bin; bin="$(find_layered)" || return 1
  "${bin}" --help >/dev/null 2>&1 || true
  "${bin}" > "${OUT}/.layered.log" 2>&1 &
  LAYERED_PID=$!
  sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]] || { warn "scx_layered did not enable"; return 1; }
  return 0
}
detach_scx() {
  for v in PRISM_PID LAYERED_PID; do
    local pid="${!v}"
    [[ -n "${pid}" ]] || continue
    kill -INT "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
    printf -v "$v" '%s' ""
  done
  sleep 1
}

u64_le_hex(){ printf "%016x" "$1" | sed 's/../& /g' | awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32_le_hex(){ printf "%08x"  "$1" | sed 's/../& /g' | awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }

measure_leg() {
  local sched="$1"
  local t p50 p99 p999
  for (( t=1; t<=TRIALS; t++ )); do
    read -r p50 p99 p999 < <(run_probe_once)
    echo "${sched},${ARCH},${KREL},${WL},${t},${p50},${p99},${p999}" >> "${CSV}"
    printf "    %-16s trial %2d/%-2d  p50=%-6s p99=%-7s p99.9=%-7s us\n" "${sched}" "${t}" "${TRIALS}" "${p50}" "${p99}" "${p999}"
  done
}

echo "scheduler,arch,kernel,workload,trial,p50_us,p99_us,p999_us" > "${CSV}"

# ---- (a) baseline: vanilla EEVDF, no scx ----
step "[a] baseline (EEVDF, no scx) under contention"
start_noise; measure_leg "baseline"; stop_noise

# ---- (b) scx_prism, EMPTY bus (control) ----
step "[b] scx_prism_nobus (attached, empty bus) under contention"
if attach_prism; then
  info "bus is empty (probe NOT seeded) -> probe should resolve UNKNOWN -> NORM lane"
  start_noise; measure_leg "scx_prism_nobus"; stop_noise
  detach_scx
else warn "scx_prism_nobus leg skipped"; detach_scx; fi

# ---- (c) scx_prism, SEEDED bus (treatment) ----
step "[c] scx_prism (bus seeded: probe -> HIGH lane) under contention"
if attach_prism; then
  seed_bus || true
  start_noise; measure_leg "scx_prism"; stop_noise
  detach_scx
else warn "scx_prism leg skipped"; detach_scx; fi

# ---- (d) scx_layered (bonus reference) ----
step "[d] scx_layered (reference scx, default config) under contention"
if find_layered >/dev/null 2>&1; then
  if attach_layered; then start_noise; measure_leg "scx_layered"; stop_noise; detach_scx
  else warn "scx_layered attach failed; skipped"; detach_scx; fi
else warn "scx_layered not built; skipped (bonus leg)"; fi

step "summary + money plot"
echo "wrote ${CSV} ($(($(wc -l < "${CSV}") - 1)) trial rows)"
if [[ "${SKIP_PLOT:-0}" != "1" ]] && have python3; then
  python3 "${SCRIPT_DIR}/sched_eval_plot.py" "${CSV}" "${OUT}" || warn "plot step failed"
fi
echo
echo "DONE. raw=${CSV}  summary=${OUT}/sched_eval_summary_${ARCH}.csv  plot=${OUT}/sched_money_plot_${ARCH}.png"
