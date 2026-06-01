#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-latjit-eval.sh — heuristic blind-spot #2: a CPU-bound latency-critical task.
#
# Same shape as run-bpfland-eval.sh but the probe is `latjit` (a CPU-bound,
# never-sleeping latency probe) instead of schbench (sleepy). bpfland's sleep
# heuristic mislabels a CPU-bound task as batch and deprioritizes it under
# contention; Prism marks it CRITICAL on the bus and the retrofit protects it
# regardless of sleep behaviour. The honesty guard is identical: vanilla and the
# Prism builds share one source/knobs; the only difference is reading the bus.
#
# Legs: baseline (EEVDF) / bpfland (vanilla) / bpfland_prism (replace-variant) /
#       bpfland_prism_floor (floor-variant). Seed the probe class=critical.
#
# Env: TRIALS=20 RUNTIME=5 THREADS=8 NOISE=16 FAST_ID=256
#      PROBE_CLASS=1 PROBE_WEIGHT=0
#      VANILLA=/opt/scx/target/release/scx_bpfland_vanilla
#      PRISM=/opt/scx/target/release/scx_bpfland_prism
#      FLOOR=/opt/scx/target/release/scx_bpfland_prism_floor
#      LEGS="baseline bpfland bpfland_prism bpfland_prism_floor"  (subset to taste)
set -euo pipefail
TRIALS="${TRIALS:-20}"; RUNTIME="${RUNTIME:-5}"; THREADS="${THREADS:-8}"
NOISE="${NOISE:-16}"; FAST_ID="${FAST_ID:-256}"; SETTLE="${SETTLE:-3}"
PROBE_CLASS="${PROBE_CLASS:-1}"; PROBE_WEIGHT="${PROBE_WEIGHT:-0}"
OUT="${OUT:-/work/prism/scripts/eval/results}"
LATJIT="${LATJIT:-/work/prism/integrations/bpfland/latjit}"
VANILLA="${VANILLA:-/opt/scx/target/release/scx_bpfland_vanilla}"
PRISM="${PRISM:-/opt/scx/target/release/scx_bpfland_prism}"
FLOOR="${FLOOR:-/opt/scx/target/release/scx_bpfland_prism_floor}"
LEGS="${LEGS:-baseline bpfland bpfland_prism bpfland_prism_floor}"
ARCH="$(uname -m)"; CG=/sys/fs/cgroup; PROBE_CG="${CG}/prism_probe"; NOISE_CG="${CG}/prism_noise"
BPFTOOL="bpftool"; command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1 || \
  BPFTOOL="$(ls /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | head -1)"
mkdir -p "${OUT}"; CSV="${OUT}/latjit_eval_${ARCH}.csv"
[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: root"; exit 1; }
[[ -x "${LATJIT}" ]] || { echo "ERROR: latjit not built at ${LATJIT} (cc -O2 -pthread -o latjit latjit.c)"; exit 1; }
mkdir -p "${PROBE_CG}" "${NOISE_CG}"; PROBE_ID="$(stat -c %i "${PROBE_CG}")"
SCHED_PID=""; NOISE_PID=""
u64h(){ printf "%016x" "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32h(){ printf "%08x"  "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
mkflags(){ local c=$(( $1 & 7 )) w="$2"; (( w > 127 )) && w=127; (( w < 0 )) && w=0; echo $(( 2 | (c << 8) | (w << 11) )); }
# NB: use -x (exact comm) / bracketed -f so the patterns never match this
# script's own cmdline (its path contains "latjit") or the parent shell.
cleanup(){ pkill -x stress-ng 2>/dev/null||true; pkill -f '[s]cx_bpfland' 2>/dev/null||true; pkill -x latjit 2>/dev/null||true
  [[ -n "${SCHED_PID}" ]] && { kill -INT "${SCHED_PID}" 2>/dev/null||true; wait "${SCHED_PID}" 2>/dev/null||true; }
  for c in "${PROBE_CG}" "${NOISE_CG}"; do [[ -d "$c" ]]||continue
    [[ -f "$c/cgroup.procs" ]] && while read -r p; do echo "$p">"${CG}/cgroup.procs" 2>/dev/null||true; done<"$c/cgroup.procs"
    rmdir "$c" 2>/dev/null||true; done; }
trap cleanup EXIT INT TERM
start_noise(){ ( echo $BASHPID>"${NOISE_CG}/cgroup.procs"; exec stress-ng --cpu "${NOISE}" --cpu-method matrixprod --timeout 100000s ) >/dev/null 2>&1 & NOISE_PID=$!; sleep 1; }
stop_noise(){ [[ -n "${NOISE_PID}" ]]||return 0; kill "${NOISE_PID}" 2>/dev/null||true; pkill -f stress-ng 2>/dev/null||true; wait "${NOISE_PID}" 2>/dev/null||true; NOISE_PID=""; }
attach(){ "$1" >"${OUT}/.latjit.sched.log" 2>&1 & SCHED_PID=$!; sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; }
detach(){ [[ -n "${SCHED_PID}" ]]||return 0; kill -INT "${SCHED_PID}" 2>/dev/null||true; wait "${SCHED_PID}" 2>/dev/null||true; SCHED_PID=""; sleep 1; }
seed(){ ${BPFTOOL} map update name prism_identity key hex $(u64h "${PROBE_ID}") \
  value hex $(u32h "${FAST_ID}") $(u32h "$(mkflags "${PROBE_CLASS}" "${PROBE_WEIGHT}")") 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 >/dev/null 2>&1
  # Read the key back so a silently-missed seed can't masquerade as an identity result.
  if ${BPFTOOL} map lookup name prism_identity key hex $(u64h "${PROBE_ID}") >/dev/null 2>&1; then
    echo "    seeded+verified prism_identity[${PROBE_ID}] id=${FAST_ID} class=${PROBE_CLASS} w=${PROBE_WEIGHT}"
  else echo "    ERROR seed for key ${PROBE_ID} NOT readable back — identity attribution would be INVALID"; fi; }
probe_once(){ local out; out="$( ( echo $BASHPID>"${PROBE_CG}/cgroup.procs"; exec env THREADS="${THREADS}" DURATION="${RUNTIME}" "${LATJIT}" ) 2>/dev/null )"; echo "${out:-0 0 0}"; }
measure(){ local s="$1" t p50 p99 p999; for ((t=1;t<=TRIALS;t++)); do read -r p50 p99 p999 < <(probe_once); echo "${s},${t},${p50},${p99},${p999}" >> "${CSV}"; done
  local m; m=$(awk -F, -v s="$s" '$1==s{print $4}' "${CSV}"|sort -n|awk '{a[NR]=$1}END{print a[int(NR/2)+1]}'); printf "    %-20s p99 stall median = %s us\n" "$s" "$m"; }

echo "scheduler,trial,p50_us,p99_us,p999_us" > "${CSV}"
echo "probe=latjit(THREADS=${THREADS} CPU-bound) noise=${NOISE} trials=${TRIALS}x${RUNTIME}s legs='${LEGS}'"
rm -f /sys/fs/bpf/prism_identity 2>/dev/null || true

for leg in ${LEGS}; do
  case "${leg}" in
    baseline)            echo "== baseline (EEVDF) =="; start_noise; measure "baseline"; stop_noise;;
    bpfland)             echo "== bpfland (vanilla, sleep heuristic) =="
                         if attach "${VANILLA}"; then start_noise; measure "bpfland"; stop_noise; detach; else echo "    WARN attach"; detach; fi;;
    bpfland_prism)       echo "== bpfland_prism (replace-variant, identity CRITICAL) =="
                         if attach "${PRISM}"; then seed; start_noise; measure "bpfland_prism"; stop_noise; detach; else echo "    WARN attach"; detach; fi;;
    bpfland_prism_floor) echo "== bpfland_prism_floor (floor-variant, identity CRITICAL) =="
                         if attach "${FLOOR}"; then seed; start_noise; measure "bpfland_prism_floor"; stop_noise; detach; else echo "    WARN attach"; detach; fi;;
    *) echo "    WARN unknown leg '${leg}'";;
  esac
done
echo "wrote ${CSV}"
