#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-dsb-eval.sh — DeathStarBench socialNetwork tail latency under CPU
# contention: baseline EEVDF vs scx_prism with the bus seeded so the app is
# protected. The REAL-microservice complement to the synthetic schbench result.
#
# Topology (run this INSIDE the privileged prism container, --cgroupns=host):
#   * DeathStarBench socialNetwork (27 services) runs on the HOST docker; its
#     container cgroups are visible here via cgroupns=host. We seed EVERY docker
#     container cgroup -> PRISM_ID_MIN_DYNAMIC (256) so the whole app (and the
#     wrk2 client, which shares this container's cgroup) lands in scx_prism's
#     HIGH lane.
#   * stress-ng saturates the CPUs from a SEPARATE cgroup (prism_noise) -> NORM.
#   * wrk2 (coordinated-omission-correct, -R fixed rate) drives the app via the
#     host gateway and records the app's request-latency tail.
#
# Legs: baseline (no scx) and scx_prism (seeded). Sweeps wrk2 rates.
# Metric: app request latency p50/p99/p99.9 (ms), CO-corrected, per (rate,sched).
#
# Env: RATES="2000 4000 6000" DUR=20 CONNS=64 THREADS=8 NOISE=16 FAST_ID=256
#      URL=http://172.17.0.1:8080 LUA=.../mixed-workload.lua
set -euo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)}"
RATES="${RATES:-2000 4000 6000 8000}"; DUR="${DUR:-20}"; CONNS="${CONNS:-64}"; THREADS="${THREADS:-8}"
NOISE="${NOISE:-16}"; FAST_ID="${FAST_ID:-256}"; SETTLE="${SETTLE:-3}"
URL="${URL:-http://172.17.0.1:8080}"
LUA="${LUA:-${SCRIPT_DIR}/dsb_lua/mixed-workload.lua}"
WRK2="${WRK2:-/opt/wrk2/wrk}"
OBJ="${OBJ:-${REPO_DIR}/bpf/scx_prism.bpf.o}"; LOADER="${LOADER:-${REPO_DIR}/loader/scx_prism_loader}"
OUT="${OUT:-${SCRIPT_DIR}/results}"; ARCH="$(uname -m)"; KREL="$(uname -r)"; CG=/sys/fs/cgroup
NOISE_CG="${CG}/prism_noise"
BPFTOOL="bpftool"; command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1 || \
  BPFTOOL="$(ls /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | head -1)"
mkdir -p "${OUT}"; CSV="${OUT}/dsb_socialnet_${ARCH}.csv"
[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: root required"; exit 1; }
[[ -x "${WRK2}" ]] || { echo "ERROR: wrk2 missing at ${WRK2}"; exit 1; }
[[ -f "${LUA}" ]] || { echo "ERROR: lua missing at ${LUA}"; exit 1; }
curl -fsS -o /dev/null "${URL}/" 2>/dev/null || { echo "ERROR: DSB frontend not reachable at ${URL}"; exit 1; }
echo "rates=[${RATES}] dur=${DUR}s conns=${CONNS} threads=${THREADS} noise=${NOISE} url=${URL}"

mkdir -p "${NOISE_CG}"; NOISE_PID=""; PRISM_PID=""
u64h(){ printf "%016x" "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32h(){ printf "%08x"  "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
to_ms(){ awk -v s="$1" 'BEGIN{u=s;sub(/[0-9.]+/,"",u);v=s;sub(/[a-z]+$/,"",v);v=v+0;
  if(u=="us")print v/1000; else if(u=="ms")print v; else if(u=="s")print v*1000; else if(u=="m")print v*60000; else print v}'; }
cleanup(){ pkill -f stress-ng 2>/dev/null||true
  [[ -n "${PRISM_PID}" ]] && { kill -INT "${PRISM_PID}" 2>/dev/null||true; wait "${PRISM_PID}" 2>/dev/null||true; }
  [[ -d "${NOISE_CG}" ]] && { [[ -f "${NOISE_CG}/cgroup.procs" ]] && while read -r p; do echo "$p">"${CG}/cgroup.procs" 2>/dev/null||true; done<"${NOISE_CG}/cgroup.procs"; rmdir "${NOISE_CG}" 2>/dev/null||true; }; }
trap cleanup EXIT INT TERM
start_noise(){ ( echo $BASHPID>"${NOISE_CG}/cgroup.procs"; exec stress-ng --cpu "${NOISE}" --cpu-method matrixprod --timeout 100000s ) >/dev/null 2>&1 & NOISE_PID=$!; sleep 1; }
stop_noise(){ [[ -n "${NOISE_PID}" ]]||return 0; kill "${NOISE_PID}" 2>/dev/null||true; pkill -f stress-ng 2>/dev/null||true; wait "${NOISE_PID}" 2>/dev/null||true; NOISE_PID=""; }
attach(){ "${LOADER}" "${OBJ}" >"${OUT}/.dsb_loader.log" 2>&1 & PRISM_PID=$!; sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; }
detach(){ [[ -n "${PRISM_PID}" ]]||return 0; kill -INT "${PRISM_PID}" 2>/dev/null||true; wait "${PRISM_PID}" 2>/dev/null||true; PRISM_PID=""; sleep 1; }
seed_all_docker(){ local d id c=0
  for d in ${CG}/system.slice/docker-*.scope; do
    [[ -d "$d" ]]||continue; id="$(stat -c %i "$d")"
    ${BPFTOOL} map update name prism_identity key hex $(u64h "$id") \
      value hex $(u32h "${FAST_ID}") $(u32h 2) 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 >/dev/null 2>&1 && c=$((c+1))
  done; echo "    seeded ${c} docker container cgroups -> identity ${FAST_ID} (HIGH lane)"; }
runwrk(){ local sched="$1" rate="$2" o p50 p99 p999
  o="$("${WRK2}" -t"${THREADS}" -c"${CONNS}" -d"${DUR}s" -R"${rate}" -L -s "${LUA}" "${URL}" 2>/dev/null)"
  p50="$(awk '/ 50.000%/{print $2; exit}' <<<"$o")"; p99="$(awk '/ 99.000%/{print $2; exit}' <<<"$o")"; p999="$(awk '/ 99.900%/{print $2; exit}' <<<"$o")"
  local m50 m99 m999; m50="$(to_ms "${p50:-0ms}")"; m99="$(to_ms "${p99:-0ms}")"; m999="$(to_ms "${p999:-0ms}")"
  echo "${sched},${ARCH},${KREL},${rate},${m50},${m99},${m999}" >> "${CSV}"
  printf "    %-10s rate=%-5s p50=%-7s p99=%-7s p99.9=%-7s ms\n" "$sched" "$rate" "$m50" "$m99" "$m999"; }

echo "scheduler,arch,kernel,rate_rps,p50_ms,p99_ms,p999_ms" > "${CSV}"
echo "== baseline (EEVDF) under ${NOISE}-hog noise =="
start_noise; for r in ${RATES}; do runwrk "baseline" "$r"; done; stop_noise
# scx legs: NAME:OBJ pairs. Default policy (20ms slice) shows the long-slice
# pathology on IPC-heavy services; the short-slice (2ms) policy fits microservices.
SCX_LEGS="${SCX_LEGS:-scx_prism:${REPO_DIR}/bpf/scx_prism.bpf.o scx_prism_short:${REPO_DIR}/bpf/scx_prism_short.bpf.o}"
for leg in ${SCX_LEGS}; do
  name="${leg%%:*}"; OBJ="${leg#*:}"
  [[ -f "${OBJ}" ]] || { echo "    skip ${name} (no obj ${OBJ})"; continue; }
  echo "== ${name} (bus seeded: app -> HIGH lane) under ${NOISE}-hog noise =="
  if attach; then seed_all_docker; start_noise; for r in ${RATES}; do runwrk "${name}" "$r"; done; stop_noise; detach
  else echo "    WARN: ${name} attach failed"; detach; fi
done
echo "wrote ${CSV}"
[[ "${SKIP_PLOT:-0}" == "1" ]] || python3 "${SCRIPT_DIR}/dsb_plot.py" "${CSV}" "${OUT}" 2>/dev/null || echo "(plot skipped/failed)"
cat "${CSV}"
