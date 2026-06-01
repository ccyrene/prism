#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-bpfland-eval.sh — does feeding Prism identity into a REAL production
# scheduler (scx_bpfland) change what it protects?
#
# Three legs, latency probe (schbench) in cgroup prism_probe under stress-ng
# noise in cgroup prism_noise:
#   baseline        : EEVDF (no scx)
#   bpfland         : vanilla scx_bpfland — prioritizes by its SLEEP HEURISTIC
#   bpfland_prism   : the retrofit — prioritizes by the per-identity latency
#                     CLASS the bus carries (operator-set), mapped natively to
#                     bpfland's own deadline knob. The probe is seeded
#                     class=critical; with GAMER=1 the gamer is seeded class=batch
#                     so identity OVERRIDES the sleep heuristic it fools (identity
#                     can't be gamed). Same prism_identity map net/sched/trace
#                     share. (Seed BEFORE launching the probe.)
#
# Env: TRIALS=20 RUNTIME=5 WARMUP=1 MSG=2 WORKERS=8 NOISE=16 FAST_ID=256
#      GAMER=0  (set 1 to add a "heuristic-gamer": a batch hog that sleeps to
#                look interactive — shows the heuristic is fooled, identity isn't)
#      PROBE_CLASS=1 PROBE_WEIGHT=0 GAMER_CLASS=3 (thick-bus policy: class
#                1=critical 2=normal 3=batch; weight 0..127, 0=unset)
set -euo pipefail
TRIALS="${TRIALS:-20}"; RUNTIME="${RUNTIME:-5}"; WARMUP="${WARMUP:-1}"
MSG="${MSG:-2}"; WORKERS="${WORKERS:-8}"; NOISE="${NOISE:-16}"; FAST_ID="${FAST_ID:-256}"; SETTLE="${SETTLE:-3}"
GAMER="${GAMER:-0}"
# Thick-bus policy published for the prism leg. The retrofit is CLASS-driven:
# the probe is seeded latency-critical (class 1); the optional heuristic-gamer is
# seeded BATCH (class 3) so identity overrides the sleep heuristic it fools.
PROBE_CLASS="${PROBE_CLASS:-1}"; PROBE_WEIGHT="${PROBE_WEIGHT:-0}"
GAMER_FAST_ID="${GAMER_FAST_ID:-257}"; GAMER_CLASS="${GAMER_CLASS:-3}"
OUT="${OUT:-/work/prism/scripts/eval/results}"
VANILLA="${VANILLA:-/opt/scx/target/release/scx_bpfland_vanilla}"
PRISM="${PRISM:-/opt/scx/target/release/scx_bpfland_prism}"
ARCH="$(uname -m)"; CG=/sys/fs/cgroup; PROBE_CG="${CG}/prism_probe"; NOISE_CG="${CG}/prism_noise"; GAMER_CG="${CG}/prism_gamer"
BPFTOOL="bpftool"; command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1 || \
  BPFTOOL="$(ls /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | head -1)"
mkdir -p "${OUT}"; CSV="${OUT}/bpfland_eval_${ARCH}.csv"
[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: root"; exit 1; }
mkdir -p "${PROBE_CG}" "${NOISE_CG}" "${GAMER_CG}"; PROBE_ID="$(stat -c %i "${PROBE_CG}")"; GAMER_ID="$(stat -c %i "${GAMER_CG}")"
SCHED_PID=""; NOISE_PID=""; GAMER_PID=""
u64h(){ printf "%016x" "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32h(){ printf "%08x"  "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
cleanup(){ pkill -f stress-ng 2>/dev/null||true; pkill -f scx_bpfland 2>/dev/null||true
  [[ -n "${SCHED_PID}" ]] && { kill -INT "${SCHED_PID}" 2>/dev/null||true; wait "${SCHED_PID}" 2>/dev/null||true; }
  for c in "${PROBE_CG}" "${NOISE_CG}" "${GAMER_CG}"; do [[ -d "$c" ]]||continue
    [[ -f "$c/cgroup.procs" ]] && while read -r p; do echo "$p">"${CG}/cgroup.procs" 2>/dev/null||true; done<"$c/cgroup.procs"
    rmdir "$c" 2>/dev/null||true; done; }
trap cleanup EXIT INT TERM
start_noise(){ ( echo $BASHPID>"${NOISE_CG}/cgroup.procs"; exec stress-ng --cpu "${NOISE}" --cpu-method matrixprod --timeout 100000s ) >/dev/null 2>&1 & NOISE_PID=$!; sleep 1; }
stop_noise(){ [[ -n "${NOISE_PID}" ]]||return 0; kill "${NOISE_PID}" 2>/dev/null||true; pkill -f stress-ng 2>/dev/null||true; wait "${NOISE_PID}" 2>/dev/null||true; NOISE_PID=""; }
# "gamer": batch CPU work that sleeps briefly+often to LOOK interactive to a heuristic.
start_gamer(){ [[ "${GAMER}" == "1" ]]||return 0
  ( echo $BASHPID>"${GAMER_CG}/cgroup.procs"; exec stress-ng --cpu "${NOISE}" --cpu-method matrixprod --cpu-load 90 --cpu-load-slice 5 --timeout 100000s ) >/dev/null 2>&1 & GAMER_PID=$!; sleep 1; }
stop_gamer(){ [[ -n "${GAMER_PID}" ]]||return 0; kill "${GAMER_PID}" 2>/dev/null||true; wait "${GAMER_PID}" 2>/dev/null||true; GAMER_PID=""; }
attach(){ "$1" >"${OUT}/.bpfland.log" 2>&1 & SCHED_PID=$!; sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; }
detach(){ [[ -n "${SCHED_PID}" ]]||return 0; kill -INT "${SCHED_PID}" 2>/dev/null||true; wait "${SCHED_PID}" 2>/dev/null||true; SCHED_PID=""; sleep 1; }
# Thick-bus flags word = SCHED_MANAGED(0x2) | class<<8 | weight<<11. This matches
# pkg/abi/abi.go EncodeSchedPolicy and bpf/prism_maps.bpf.h PRISM_SCHED_*; the
# bpfland retrofit reads the class out of these bits in task_dl().
# Clamp/mask to mirror abi.EncodeSchedPolicy exactly (class 3b, weight 7b) so the
# harness encoder stays a faithful byte-twin even for out-of-range operator input.
mkflags(){ local c=$(( $1 & 7 )) w="$2"; (( w > 127 )) && w=127; (( w < 0 )) && w=0; echo $(( 2 | (c << 8) | (w << 11) )); }
seed_one(){ ${BPFTOOL} map update name prism_identity key hex $(u64h "$1") \
  value hex $(u32h "$2") $(u32h "$3") 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 >/dev/null 2>&1
  # Read the key back so a silently-missed seed can't masquerade as an identity result.
  if ${BPFTOOL} map lookup name prism_identity key hex $(u64h "$1") >/dev/null 2>&1; then
    echo "    seeded+verified prism_identity[$1] id=$2 flags=$3 ($4)"
  else echo "    ERROR seed for key $1 NOT readable back ($4) — identity attribution would be INVALID"; fi; }
# Seed the probe latency-critical; with GAMER=1 also seed the gamer BATCH so the
# bus's identity-driven class overrides the sleep heuristic the gamer fools.
seed(){ seed_one "${PROBE_ID}" "${FAST_ID}" "$(mkflags "${PROBE_CLASS}" "${PROBE_WEIGHT}")" "probe class=${PROBE_CLASS} w=${PROBE_WEIGHT}"
  if [[ "${GAMER}" == "1" ]]; then seed_one "${GAMER_ID}" "${GAMER_FAST_ID}" "$(mkflags "${GAMER_CLASS}" 0)" "gamer class=${GAMER_CLASS}"; fi; }
probe_once(){ local jf="${OUT}/.bl.$$.json"
  ( echo $BASHPID>"${PROBE_CG}/cgroup.procs"; exec schbench -m "${MSG}" -t "${WORKERS}" -r "${RUNTIME}" -w "${WARMUP}" -i 3600 -j "$jf" ) >/dev/null 2>&1||true
  local a b c; a="$(jq -r '.int."wakeup_latency_pct50.0"//0' "$jf" 2>/dev/null)"; b="$(jq -r '.int."wakeup_latency_pct99.0"//0' "$jf" 2>/dev/null)"; c="$(jq -r '.int."wakeup_latency_pct99.9"//0' "$jf" 2>/dev/null)"; rm -f "$jf"; echo "${a:-0} ${b:-0} ${c:-0}"; }
measure(){ local s="$1" t p50 p99 p999; for ((t=1;t<=TRIALS;t++)); do read -r p50 p99 p999 < <(probe_once); echo "${s},${t},${p50},${p99},${p999}" >> "${CSV}"; done
  local m; m=$(awk -F, -v s="$s" '$1==s{print $4}' "${CSV}"|sort -n|awk '{a[NR]=$1}END{print a[int(NR/2)+1]}'); printf "    %-16s p99 median = %s us\n" "$s" "$m"; }

echo "scheduler,trial,p50_us,p99_us,p999_us" > "${CSV}"
echo "probe=schbench(-m ${MSG} -t ${WORKERS}) noise=${NOISE} gamer=${GAMER} trials=${TRIALS}x${RUNTIME}s"
rm -f /sys/fs/bpf/prism_identity 2>/dev/null || true

echo "== baseline (EEVDF) =="; start_noise; start_gamer; measure "baseline"; stop_gamer; stop_noise
echo "== bpfland (vanilla, sleep heuristic) =="
if attach "${VANILLA}"; then start_noise; start_gamer; measure "bpfland"; stop_gamer; stop_noise; detach
else echo "    WARN vanilla attach failed"; detach; fi
if [[ "${SKIP_PRISM:-0}" != "1" ]]; then
echo "== bpfland_prism (identity-driven) =="
if attach "${PRISM}"; then seed; start_noise; start_gamer; measure "bpfland_prism"; stop_gamer; stop_noise; detach
else echo "    WARN prism attach failed"; detach; fi
fi
echo "wrote ${CSV}"; column -t -s, "${CSV}" 2>/dev/null | head -1
