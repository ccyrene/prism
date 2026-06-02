#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-selective-eval.sh — Case 4+5: per-identity classes + SELECTIVE / DETERMINISTIC
# control. Three IDENTICAL CPU-bound latency probes (`latjit`) run SIMULTANEOUSLY
# in three cgroups under the same stress-ng noise. They are byte-identical
# workloads, so bpfland's sleep heuristic CANNOT tell them apart — it mislabels all
# three as batch and starves them EQUALLY. The operator then assigns each a
# different latency CLASS purely on the identity bus (no workload change):
#
#   A -> CRITICAL (class 1)  : protect this one
#   B -> NORMAL   (class 2)  : leave it on the stock heuristic
#   C -> BATCH    (class 3)  : deliberately demote it
#
# Expectation:
#   baseline (EEVDF) : A ~= B ~= C   (no identity, no per-class knob)
#   bpfland (vanilla): A ~= B ~= C   (heuristic is blind to the three identities)
#   bpfland_prism    : A  <  B  <  C (operator's deterministic ordering, from the
#                                     bus alone — the heuristic offers no such knob)
#
# This demonstrates BOTH (Case 4) per-identity policy classes consumed natively by
# the real scheduler from ONE bus, and (Case 5) selective/deterministic protection:
# among workloads a behavioural heuristic treats identically, identity lets the
# operator pick — deterministically — who wins and who loses.
#
# Honesty guard (identical to the other legs): vanilla and +Prism share ONE source
# and identical knobs (build.sh, one cargo invocation); the only difference is that
# +Prism reads the bus. baseline/vanilla legs run with an EMPTY bus, so all three
# probes are genuinely indistinguishable there — any A/B/C separation under +Prism
# is attributable to the seeded class, nothing else.
#
# Env: TRIALS=20 RUNTIME=5 THREADS=2 NOISE=16
#      A_ID=256 B_ID=257 C_ID=258  A_CLASS=1 B_CLASS=2 C_CLASS=3  WEIGHT=0
#      LATJIT=/work/prism/integrations/bpfland/latjit
#      VANILLA=/opt/scx/target/release/scx_bpfland_vanilla
#      PRISM=/opt/scx/target/release/scx_bpfland_prism
#      LEGS="baseline bpfland bpfland_prism"
set -euo pipefail
TRIALS="${TRIALS:-20}"; RUNTIME="${RUNTIME:-5}"; THREADS="${THREADS:-2}"
NOISE="${NOISE:-16}"; SETTLE="${SETTLE:-3}"; WEIGHT="${WEIGHT:-0}"
A_ID="${A_ID:-256}"; B_ID="${B_ID:-257}"; C_ID="${C_ID:-258}"
A_CLASS="${A_CLASS:-1}"; B_CLASS="${B_CLASS:-2}"; C_CLASS="${C_CLASS:-3}"
OUT="${OUT:-/work/prism/scripts/eval/results}"
LATJIT="${LATJIT:-/work/prism/integrations/bpfland/latjit}"
VANILLA="${VANILLA:-/opt/scx/target/release/scx_bpfland_vanilla}"
PRISM="${PRISM:-/opt/scx/target/release/scx_bpfland_prism}"
LEGS="${LEGS:-baseline bpfland bpfland_prism}"
ARCH="$(uname -m)"; CG=/sys/fs/cgroup
A_CG="${CG}/prism_critical"; B_CG="${CG}/prism_normal"; C_CG="${CG}/prism_batch"; NOISE_CG="${CG}/prism_noise"
BPFTOOL="bpftool"; command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1 || \
  BPFTOOL="$(ls /usr/lib/linux-tools/*/bpftool /usr/lib/linux-tools-*/bpftool 2>/dev/null | head -1)"
mkdir -p "${OUT}"; CSV="${OUT}/selective_eval_${ARCH}.csv"
[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: root"; exit 1; }
[[ -x "${LATJIT}" ]] || { echo "ERROR: latjit not built at ${LATJIT} (cc -O2 -pthread -o latjit latjit.c)"; exit 1; }
mkdir -p "${A_CG}" "${B_CG}" "${C_CG}" "${NOISE_CG}"
A_INO="$(stat -c %i "${A_CG}")"; B_INO="$(stat -c %i "${B_CG}")"; C_INO="$(stat -c %i "${C_CG}")"
SCHED_PID=""; NOISE_PID=""
u64h(){ printf "%016x" "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32h(){ printf "%08x"  "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
# Thick-bus flags word = SCHED_MANAGED(0x2) | class<<8 | weight<<11 — mirrors
# pkg/abi/abi.go EncodeSchedPolicy and bpf/prism_maps.bpf.h PRISM_SCHED_* exactly.
mkflags(){ local c=$(( $1 & 7 )) w="$2"; (( w > 127 )) && w=127; (( w < 0 )) && w=0; echo $(( 2 | (c << 8) | (w << 11) )); }
cleanup(){ pkill -x stress-ng 2>/dev/null||true; pkill -f '[s]cx_bpfland' 2>/dev/null||true; pkill -x latjit 2>/dev/null||true
  [[ -n "${SCHED_PID}" ]] && { kill -INT "${SCHED_PID}" 2>/dev/null||true; wait "${SCHED_PID}" 2>/dev/null||true; }
  for c in "${A_CG}" "${B_CG}" "${C_CG}" "${NOISE_CG}"; do [[ -d "$c" ]]||continue
    [[ -f "$c/cgroup.procs" ]] && while read -r p; do echo "$p">"${CG}/cgroup.procs" 2>/dev/null||true; done<"$c/cgroup.procs"
    rmdir "$c" 2>/dev/null||true; done; }
trap cleanup EXIT INT TERM
start_noise(){ ( echo $BASHPID>"${NOISE_CG}/cgroup.procs"; exec stress-ng --cpu "${NOISE}" --cpu-method matrixprod --timeout 100000s ) >/dev/null 2>&1 & NOISE_PID=$!; sleep 1; }
stop_noise(){ [[ -n "${NOISE_PID}" ]]||return 0; kill "${NOISE_PID}" 2>/dev/null||true; pkill -x stress-ng 2>/dev/null||true; wait "${NOISE_PID}" 2>/dev/null||true; NOISE_PID=""; }
attach(){ "$1" >"${OUT}/.selective.sched.log" 2>&1 & SCHED_PID=$!; sleep "${SETTLE}"
  [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; }
detach(){ [[ -n "${SCHED_PID}" ]]||return 0; kill -INT "${SCHED_PID}" 2>/dev/null||true; wait "${SCHED_PID}" 2>/dev/null||true; SCHED_PID=""; sleep 1; }
seed_one(){ ${BPFTOOL} map update name prism_identity key hex $(u64h "$1") \
  value hex $(u32h "$2") $(u32h "$3") 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 >/dev/null 2>&1
  if ${BPFTOOL} map lookup name prism_identity key hex $(u64h "$1") >/dev/null 2>&1; then
    echo "    seeded+verified prism_identity[$1] id=$2 flags=$3 ($4)"
  else echo "    ERROR seed for key $1 NOT readable back ($4) — attribution INVALID"; fi; }
seed(){ seed_one "${A_INO}" "${A_ID}" "$(mkflags "${A_CLASS}" "${WEIGHT}")" "A critical class=${A_CLASS}"
        seed_one "${B_INO}" "${B_ID}" "$(mkflags "${B_CLASS}" "${WEIGHT}")" "B normal   class=${B_CLASS}"
        seed_one "${C_INO}" "${C_ID}" "$(mkflags "${C_CLASS}" 0)"           "C batch    class=${C_CLASS}"; }
# Launch all three IDENTICAL probes simultaneously so they contend with each other
# AND the noise; each lands in its own cgroup BEFORE exec so the bus key (leaf
# cgroup id) matches its seed. Collect all three results for the trial.
probe_trial(){ local af="${OUT}/.sel.A.$$" bf="${OUT}/.sel.B.$$" cf="${OUT}/.sel.C.$$"
  ( echo $BASHPID>"${A_CG}/cgroup.procs"; exec env THREADS="${THREADS}" DURATION="${RUNTIME}" "${LATJIT}" ) >"$af" 2>/dev/null &
  local pa=$!
  ( echo $BASHPID>"${B_CG}/cgroup.procs"; exec env THREADS="${THREADS}" DURATION="${RUNTIME}" "${LATJIT}" ) >"$bf" 2>/dev/null &
  local pb=$!
  ( echo $BASHPID>"${C_CG}/cgroup.procs"; exec env THREADS="${THREADS}" DURATION="${RUNTIME}" "${LATJIT}" ) >"$cf" 2>/dev/null &
  local pc=$!
  wait "$pa" "$pb" "$pc" 2>/dev/null||true
  RES_A="$(cat "$af" 2>/dev/null||echo '0 0 0')"; RES_B="$(cat "$bf" 2>/dev/null||echo '0 0 0')"; RES_C="$(cat "$cf" 2>/dev/null||echo '0 0 0')"
  rm -f "$af" "$bf" "$cf"; }
measure(){ local s="$1" t; for ((t=1;t<=TRIALS;t++)); do probe_trial
    echo "${s},A_critical,${t},${RES_A// /,}" >> "${CSV}"
    echo "${s},B_normal,${t},${RES_B// /,}"   >> "${CSV}"
    echo "${s},C_batch,${t},${RES_C// /,}"    >> "${CSV}"; done
  for pr in A_critical B_normal C_batch; do
    local m; m=$(awk -F, -v s="$s" -v p="$pr" '$1==s&&$2==p{print $5}' "${CSV}"|sort -n|awk '{a[NR]=$1}END{print a[int(NR/2)+1]}')
    printf "    %-16s %-12s p99 stall median = %s us\n" "$s" "$pr" "$m"; done; }

echo "scheduler,probe,trial,p50_us,p99_us,p999_us" > "${CSV}"
echo "probe=3x latjit(THREADS=${THREADS} CPU-bound, simultaneous) noise=${NOISE} trials=${TRIALS}x${RUNTIME}s legs='${LEGS}'"
echo "classes: A=critical(${A_CLASS}) B=normal(${B_CLASS}) C=batch(${C_CLASS})  ids A=${A_ID} B=${B_ID} C=${C_ID}"
rm -f /sys/fs/bpf/prism_identity 2>/dev/null || true

for leg in ${LEGS}; do
  # Clear any stale pin before every non-Prism leg so "vanilla/baseline = empty
  # bus" holds even if LEGS is reordered to run a Prism leg first (defensive: the
  # vanilla binary carries no prism_identity map, but don't rely on leg order).
  [[ "${leg}" != "bpfland_prism" ]] && rm -f /sys/fs/bpf/prism_identity 2>/dev/null || true
  case "${leg}" in
    baseline)      echo "== baseline (EEVDF, empty bus) =="; start_noise; measure "baseline"; stop_noise;;
    bpfland)       echo "== bpfland (vanilla, sleep heuristic, empty bus) =="
                   if attach "${VANILLA}"; then start_noise; measure "bpfland"; stop_noise; detach; else echo "    WARN attach"; detach; fi;;
    bpfland_prism) echo "== bpfland_prism (identity classes from the bus) =="
                   if attach "${PRISM}"; then seed; start_noise; measure "bpfland_prism"; stop_noise; detach; else echo "    WARN attach"; detach; fi;;
    *) echo "    WARN unknown leg '${leg}'";;
  esac
done
echo "wrote ${CSV}"
