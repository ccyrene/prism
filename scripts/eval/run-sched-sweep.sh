#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-sched-sweep.sh — the "knee" experiment: how does the latency-critical
# probe's tail degrade as CPU contention rises, baseline EEVDF vs scx_prism?
#
# For each contention level (number of stress-ng CPU hogs in a separate cgroup)
# we measure the probe's (schbench) wakeup-latency tail under (a) baseline EEVDF
# and (b) scx_prism with the bus seeded so the probe lands in the HIGH lane.
# The "knee" is the load at which a curve hockey-sticks upward; a higher knee
# (and lower tail before it) for scx_prism means identity-aware dispatch sustains
# more load before tail-latency collapse.
#
# Output:
#   results/sched_sweep_<arch>.csv          scheduler,noise,trial,p50_us,p99_us,p999_us
#   results/sched_knee_plot_<arch>.png      p99 (+p99.9) vs offered load, per scheduler
#
# Run as root. Env: NOISE_LEVELS="4 8 16 24 32" TRIALS=10 RUNTIME=5 WARMUP=1
#                   MSG=2 WORKERS=8 FAST_ID=256 OUT=... PRISM_OBJ=... LOADER=...
set -euo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)}"
NOISE_LEVELS="${NOISE_LEVELS:-4 8 16 24 32}"
TRIALS="${TRIALS:-10}"; RUNTIME="${RUNTIME:-5}"; WARMUP="${WARMUP:-1}"
MSG="${MSG:-2}"; WORKERS="${WORKERS:-8}"; FAST_ID="${FAST_ID:-256}"; SETTLE="${SETTLE:-3}"
OUT="${OUT:-${SCRIPT_DIR}/results}"
PRISM_OBJ="${PRISM_OBJ:-${REPO_DIR}/bpf/scx_prism.bpf.o}"
LOADER="${LOADER:-${REPO_DIR}/loader/scx_prism_loader}"
ARCH="$(uname -m)"; KREL="$(uname -r)"; CG=/sys/fs/cgroup
PROBE_CG="${CG}/prism_probe"; NOISE_CG="${CG}/prism_noise"
BPFTOOL="bpftool"; command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1 || \
  BPFTOOL="$(ls /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | head -1)"
mkdir -p "${OUT}"; CSV="${OUT}/sched_sweep_${ARCH}.csv"
[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: must run as root"; exit 1; }
echo "levels=[${NOISE_LEVELS}] trials=${TRIALS} runtime=${RUNTIME}s probe=schbench(-m ${MSG} -t ${WORKERS})"

mkdir -p "${PROBE_CG}" "${NOISE_CG}"
PROBE_ID="$(stat -c %i "${PROBE_CG}")"
NOISE_PID=""; PRISM_PID=""
u64_le_hex(){ printf "%016x" "$1" | sed 's/../& /g' | awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32_le_hex(){ printf "%08x"  "$1" | sed 's/../& /g' | awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
cleanup(){ [[ -n "${NOISE_PID}" ]] && { kill "${NOISE_PID}" 2>/dev/null||true; pkill -f stress-ng 2>/dev/null||true; }
  [[ -n "${PRISM_PID}" ]] && { kill -INT "${PRISM_PID}" 2>/dev/null||true; wait "${PRISM_PID}" 2>/dev/null||true; }
  for c in "${PROBE_CG}" "${NOISE_CG}"; do [[ -d "$c" ]] || continue
    [[ -f "$c/cgroup.procs" ]] && while read -r p; do echo "$p">"${CG}/cgroup.procs" 2>/dev/null||true; done <"$c/cgroup.procs"
    rmdir "$c" 2>/dev/null||true; done; }
trap cleanup EXIT INT TERM
start_noise(){ ( echo $BASHPID>"${NOISE_CG}/cgroup.procs"; exec stress-ng --cpu "$1" --cpu-method matrixprod --timeout 1000s ) >/dev/null 2>&1 & NOISE_PID=$!; sleep 1; }
stop_noise(){ [[ -n "${NOISE_PID}" ]]||return 0; kill "${NOISE_PID}" 2>/dev/null||true; pkill -f stress-ng 2>/dev/null||true; wait "${NOISE_PID}" 2>/dev/null||true; NOISE_PID=""; }
run_probe_once(){ local jf="${OUT}/.sw.$$.json"
  ( echo $BASHPID>"${PROBE_CG}/cgroup.procs"; exec schbench -m "${MSG}" -t "${WORKERS}" -r "${RUNTIME}" -w "${WARMUP}" -i 3600 -j "${jf}" ) >/dev/null 2>&1||true
  local p50 p99 p999; p50="$(jq -r '.int."wakeup_latency_pct50.0"//0' "${jf}" 2>/dev/null)"
  p99="$(jq -r '.int."wakeup_latency_pct99.0"//0' "${jf}" 2>/dev/null)"; p999="$(jq -r '.int."wakeup_latency_pct99.9"//0' "${jf}" 2>/dev/null)"
  rm -f "${jf}"; echo "${p50:-0} ${p99:-0} ${p999:-0}"; }
attach_prism(){ "${LOADER}" "${PRISM_OBJ}" >"${OUT}/.sweep_loader.log" 2>&1 & PRISM_PID=$!; sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; }
detach_prism(){ [[ -n "${PRISM_PID}" ]]||return 0; kill -INT "${PRISM_PID}" 2>/dev/null||true; wait "${PRISM_PID}" 2>/dev/null||true; PRISM_PID=""; sleep 1; }
seed_bus(){ ${BPFTOOL} map update name prism_identity key hex $(u64_le_hex "${PROBE_ID}") \
  value hex $(u32_le_hex "${FAST_ID}") $(u32_le_hex 2) 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 >/dev/null 2>&1; }
measure(){ local sched="$1" noise="$2" t p50 p99 p999
  for ((t=1;t<=TRIALS;t++)); do read -r p50 p99 p999 < <(run_probe_once)
    echo "${sched},${noise},${t},${p50},${p99},${p999}" >> "${CSV}"; done
  printf "    %-10s noise=%-3s done (%d trials)\n" "$sched" "$noise" "$TRIALS"; }

echo "scheduler,noise,trial,p50_us,p99_us,p999_us" > "${CSV}"
for L in ${NOISE_LEVELS}; do
  echo "== contention: ${L} hogs =="
  start_noise "${L}"; measure "baseline" "${L}"; stop_noise
  if attach_prism; then seed_bus; start_noise "${L}"; measure "scx_prism" "${L}"; stop_noise; detach_prism
  else echo "    WARN: scx_prism attach failed at noise=${L}"; detach_prism; fi
done
echo "wrote ${CSV} ($(($(wc -l < "${CSV}")-1)) rows)"
[[ "${SKIP_PLOT:-0}" == "1" ]] || python3 "${SCRIPT_DIR}/sched_sweep_plot.py" "${CSV}" "${OUT}" || echo "plot failed"
