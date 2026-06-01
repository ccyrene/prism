#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-fairness-eval.sh — does anti-starvation fix scx_prism's strict-priority
# weakness WITHOUT giving up the latency win?
#
# The default scx_prism drains the HIGH lane before NORM (strict priority): a
# saturated HIGH workload can starve NORM. The PRISM_FAIR build interleaves a
# guaranteed NORM dispatch every 8th tick. This experiment quantifies both:
#
#   Part A (starvation):   a CPU hog in the HIGH lane (seeded) competes with a CPU
#                          hog in the NORM lane (unseeded) for all 16 CPUs. We
#                          report each lane's throughput (stress-ng bogo-ops/s).
#                          Strict should starve NORM; FAIR should restore it;
#                          baseline EEVDF shares ~evenly.
#   Part B (latency kept): the latency-critical probe (schbench, HIGH) under NORM
#                          noise — p99 wakeup latency must stay low under FAIR too.
#
# Legs: baseline (EEVDF), scx_prism (strict), scx_prism_fair (anti-starvation).
# Run as root. Env: T=8 TRIALS=15 RUNTIME=5 WARMUP=1 HOGS=8 NOISE=16 FAST_ID=256
set -euo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)}"
T="${T:-8}"; TRIALS="${TRIALS:-15}"; RUNTIME="${RUNTIME:-5}"; WARMUP="${WARMUP:-1}"
HOGS="${HOGS:-8}"; NOISE="${NOISE:-16}"; FAST_ID="${FAST_ID:-256}"; SETTLE="${SETTLE:-3}"
MSG="${MSG:-2}"; WORKERS="${WORKERS:-8}"
OUT="${OUT:-${SCRIPT_DIR}/results}"
OBJ_STRICT="${OBJ_STRICT:-${REPO_DIR}/bpf/scx_prism.bpf.o}"
OBJ_FAIR="${OBJ_FAIR:-${REPO_DIR}/bpf/scx_prism_fair.bpf.o}"
LOADER="${LOADER:-${REPO_DIR}/loader/scx_prism_loader}"
ARCH="$(uname -m)"; CG=/sys/fs/cgroup; HIGH_CG="${CG}/prism_probe"; NORM_CG="${CG}/prism_noise"
BPFTOOL="bpftool"; command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1 || \
  BPFTOOL="$(ls /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | head -1)"
mkdir -p "${OUT}"; [[ "$(id -u)" -eq 0 ]] || { echo "ERROR: root required"; exit 1; }
TPUT_CSV="${OUT}/fairness_throughput_${ARCH}.csv"; LAT_CSV="${OUT}/fairness_latency_${ARCH}.csv"
mkdir -p "${HIGH_CG}" "${NORM_CG}"; HIGH_ID="$(stat -c %i "${HIGH_CG}")"
PRISM_PID=""
u64h(){ printf "%016x" "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32h(){ printf "%08x"  "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
cleanup(){ pkill -f stress-ng 2>/dev/null||true
  [[ -n "${PRISM_PID}" ]] && { kill -INT "${PRISM_PID}" 2>/dev/null||true; wait "${PRISM_PID}" 2>/dev/null||true; }
  for c in "${HIGH_CG}" "${NORM_CG}"; do [[ -d "$c" ]]||continue
    [[ -f "$c/cgroup.procs" ]] && while read -r p; do echo "$p">"${CG}/cgroup.procs" 2>/dev/null||true; done<"$c/cgroup.procs"
    rmdir "$c" 2>/dev/null||true; done; }
trap cleanup EXIT INT TERM
attach(){ "${LOADER}" "$1" >"${OUT}/.fair_loader.log" 2>&1 & PRISM_PID=$!; sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; }
detach(){ [[ -n "${PRISM_PID}" ]]||return 0; kill -INT "${PRISM_PID}" 2>/dev/null||true; wait "${PRISM_PID}" 2>/dev/null||true; PRISM_PID=""; sleep 1; }
seed(){ ${BPFTOOL} map update name prism_identity key hex $(u64h "${HIGH_ID}") \
  value hex $(u32h "${FAST_ID}") $(u32h 2) 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 >/dev/null 2>&1; }
bops(){ grep "bogo-ops-per-second-real-time" "$1" 2>/dev/null | head -1 | awk '{printf "%.0f",$2}'; }

# Part A: HIGH hog vs NORM hog throughput.
throughput_leg(){ local sched="$1"
  local hy="${OUT}/.high.yaml" ny="${OUT}/.norm.yaml"; rm -f "$hy" "$ny"
  ( echo $BASHPID>"${HIGH_CG}/cgroup.procs"; exec stress-ng --cpu "${HOGS}" --cpu-method matrixprod --timeout "${T}s" --metrics --yaml "$hy" ) >/dev/null 2>&1 &
  local hp=$!
  ( echo $BASHPID>"${NORM_CG}/cgroup.procs"; exec stress-ng --cpu "${HOGS}" --cpu-method matrixprod --timeout "${T}s" --metrics --yaml "$ny" ) >/dev/null 2>&1 &
  local np=$!
  wait "$hp" "$np" 2>/dev/null||true
  local h n; h="$(bops "$hy")"; n="$(bops "$ny")"
  echo "${sched},HIGH,${h:-0}" >> "${TPUT_CSV}"
  echo "${sched},NORM,${n:-0}" >> "${TPUT_CSV}"
  local tot=$(( ${h:-0} + ${n:-0} )); local share=0
  [[ "$tot" -gt 0 ]] && share=$(awk -v n="${n:-0}" -v t="$tot" 'BEGIN{printf "%.1f",100*n/t}')
  printf "    %-16s HIGH=%-10s NORM=%-10s  NORM share=%s%%\n" "$sched" "${h:-0}" "${n:-0}" "$share"
}
# Part B: probe latency under NORM noise.
probe_once(){ local jf="${OUT}/.fl.$$.json"
  ( echo $BASHPID>"${HIGH_CG}/cgroup.procs"; exec schbench -m "${MSG}" -t "${WORKERS}" -r "${RUNTIME}" -w "${WARMUP}" -i 3600 -j "$jf" ) >/dev/null 2>&1||true
  local a b c; a="$(jq -r '.int."wakeup_latency_pct50.0"//0' "$jf" 2>/dev/null)"; b="$(jq -r '.int."wakeup_latency_pct99.0"//0' "$jf" 2>/dev/null)"; c="$(jq -r '.int."wakeup_latency_pct99.9"//0' "$jf" 2>/dev/null)"; rm -f "$jf"; echo "${a:-0} ${b:-0} ${c:-0}"; }
latency_leg(){ local sched="$1" t p50 p99 p999 np
  ( echo $BASHPID>"${NORM_CG}/cgroup.procs"; exec stress-ng --cpu "${NOISE}" --cpu-method matrixprod --timeout 1000s ) >/dev/null 2>&1 & np=$!; sleep 1
  for ((t=1;t<=TRIALS;t++)); do read -r p50 p99 p999 < <(probe_once); echo "${sched},${t},${p50},${p99},${p999}" >> "${LAT_CSV}"; done
  kill "$np" 2>/dev/null||true; pkill -f stress-ng 2>/dev/null||true; wait "$np" 2>/dev/null||true
  local med; med=$(awk -F, -v s="$sched" '$1==s{print $4}' "${LAT_CSV}"|sort -n|awk '{a[NR]=$1}END{print a[int(NR/2)+1]}')
  printf "    %-16s probe p99 median ~ %s us\n" "$sched" "$med"
}

echo "== Part A: starvation (HIGH hog vs NORM hog, ${HOGS}+${HOGS} on 16 CPUs, ${T}s) =="
echo "scheduler,lane,bogo_ops_per_s" > "${TPUT_CSV}"
throughput_leg "baseline"
if attach "${OBJ_STRICT}"; then seed; throughput_leg "scx_prism"; detach; fi
if attach "${OBJ_FAIR}";   then seed; throughput_leg "scx_prism_fair"; detach; fi

if [[ "${SKIP_LAT:-0}" != "1" ]]; then
echo "== Part B: latency preserved (schbench HIGH under ${NOISE}-hog NORM noise, ${TRIALS}x${RUNTIME}s) =="
echo "scheduler,trial,p50_us,p99_us,p999_us" > "${LAT_CSV}"
latency_leg "baseline"
if attach "${OBJ_STRICT}"; then seed; latency_leg "scx_prism"; detach; fi
if attach "${OBJ_FAIR}";   then seed; latency_leg "scx_prism_fair"; detach; fi
fi

echo; echo "wrote ${TPUT_CSV}${SKIP_LAT:+ (throughput only)}"
[[ "${SKIP_LAT:-0}" == "1" ]] && exit 0
python3 - "$TPUT_CSV" "$LAT_CSV" <<'PY'
import csv,sys,statistics as st
tp,lat=sys.argv[1],sys.argv[2]
T={}
for r in csv.DictReader(open(tp)): T.setdefault(r['scheduler'],{})[r['lane']]=float(r['bogo_ops_per_s'])
L={}
for r in csv.DictReader(open(lat)):
    v=float(r['p99_us']);
    if v>0: L.setdefault(r['scheduler'],[]).append(v)
print("\n  scheduler          HIGH bops/s   NORM bops/s   NORM share   probe p99 median (us)")
for s in ['baseline','scx_prism','scx_prism_fair']:
    if s not in T: continue
    h=T[s].get('HIGH',0); n=T[s].get('NORM',0); tot=h+n; share=(100*n/tot) if tot else 0
    p99=st.median(L[s]) if s in L and L[s] else float('nan')
    print(f"  {s:<18} {h:>11.0f} {n:>13.0f} {share:>10.1f}% {p99:>18.0f}")
PY
