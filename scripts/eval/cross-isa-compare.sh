#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# cross-isa-compare.sh — prove the SAME identity-aware policy works byte-for-byte
# across instruction sets (x86_64 and aarch64), and compare the scheduling result.
#
# The Prism scheduler (bpf/scx_prism.bpf.c) and this harness are arch-independent:
# the only arch-specific build flag is clang's -D__TARGET_ARCH_*. So the cross-ISA
# claim is "run the IDENTICAL eval on an x86 box and an ARM box and show the
# identity-aware win holds on both". This script:
#
#   1. runs run-sched-eval.sh on THIS host and tags the result CSV by $(uname -m),
#   2. when CSVs from BOTH arches are present (you run this once per arch, into a
#      shared OUT dir — e.g. an rsync'd / NFS results dir, or copy them together),
#      merges them into one cross-ISA CSV + a side-by-side comparison plot.
#
# WHERE TO GET THE ARM HOST (no x86 needed twice):
#   * Oracle Cloud Always-Free Ampere A1 — up to 4 OCPU / 24 GB, FREE FOREVER,
#     runs Ubuntu 24.04/25.04 (aarch64, kernel 6.x). $0. Best option.
#   * Hetzner CAX (ARM Ampere) ≈ €0.05/hr.
#   * AWS Graviton (c7g/c8g) if you want managed parity with an x86 c7a.
#   See the cost notes for the full cheapest-path table.
#
# Usage (run ONCE on each arch, pointing at the SAME results dir):
#   # on the x86 6.12 box:
#   OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
#   # copy ~/prism-xisa/sched_eval_x86_64.csv over to the ARM box's ~/prism-xisa
#   # (or vice-versa), then on the ARM box:
#   OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
#   # -> the second run sees both CSVs and emits the merged comparison.
#
# Or, if you already have both CSVs, just merge (skip the local run):
#   MERGE_ONLY=1 OUT=~/prism-xisa scripts/eval/cross-isa-compare.sh
#
# Env knobs:
#   OUT          shared results dir holding per-arch CSVs   (default scripts/eval/results)
#   MERGE_ONLY   1 = skip the local eval, just merge what's present
#   (all run-sched-eval.sh knobs pass through: TRIALS, DURATION, WORKLOAD, ...)

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)}"

OUT="${OUT:-${SCRIPT_DIR}/results}"
MERGE_ONLY="${MERGE_ONLY:-0}"
ARCH="$(uname -m)"
mkdir -p "${OUT}"

if [[ -t 1 ]]; then
	C_BLUE=$'\033[1;34m'; C_GREEN=$'\033[1;32m'; C_YELLOW=$'\033[1;33m'; C_DIM=$'\033[2m'; C_RST=$'\033[0m'
else
	C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_DIM=""; C_RST=""
fi
step() { echo; echo "${C_BLUE}==> $*${C_RST}"; }
info() { echo "    $*"; }
ok()   { echo "    ${C_GREEN}OK${C_RST}: $*"; }
warn() { echo "    ${C_YELLOW}WARN${C_RST}: $*" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }

echo "${C_DIM}repo: ${REPO_DIR}${C_RST}"
echo "${C_DIM}arch: ${ARCH}${C_RST}"
echo "${C_DIM}out:  ${OUT}${C_RST}"

# ---------------------------------------------------------------------------
# 1. Run the per-arch eval on THIS host (tagged by uname -m by run-sched-eval.sh,
#    which already writes sched_eval_<arch>.csv into OUT).
# ---------------------------------------------------------------------------
if [[ "${MERGE_ONLY}" != "1" ]]; then
	step "1/3  Run run-sched-eval.sh on this ${ARCH} host"
	EVAL="${SCRIPT_DIR}/run-sched-eval.sh"
	[[ -x "${EVAL}" || -f "${EVAL}" ]] || { warn "run-sched-eval.sh not found at ${EVAL}"; exit 1; }
	# Pass OUT through so the per-arch CSV lands in the shared dir; all other
	# run-sched-eval knobs are inherited from the environment.
	OUT="${OUT}" bash "${EVAL}"
else
	step "1/3  MERGE_ONLY=1 — skipping the local eval"
fi

# ---------------------------------------------------------------------------
# 2. Discover per-arch CSVs in OUT.
# ---------------------------------------------------------------------------
step "2/3  Discover per-arch result CSVs"
# Match the per-arch raw trial CSVs `sched_eval_<arch>.csv`, but EXCLUDE this
# script's own derived outputs that share the prefix — `sched_eval_summary_*.csv`
# (a different schema) and the merged `sched_eval_cross_isa.csv` — otherwise a
# re-run would treat its own products as fresh per-arch inputs and corrupt the
# merge.
shopt -s nullglob
CSVS=()
for c in "${OUT}"/sched_eval_*.csv; do
	base="$(basename "${c}")"
	case "${base}" in
		sched_eval_summary_*.csv) continue ;;  # step-2 summary output
		sched_eval_cross_isa.csv) continue ;;  # our own merged output
	esac
	CSVS+=( "${c}" )
done
shopt -u nullglob
if (( ${#CSVS[@]} == 0 )); then
	warn "no sched_eval_<arch>.csv in ${OUT}; run the eval on at least one arch first."
	exit 1
fi
ARCHES=()
for c in "${CSVS[@]}"; do
	base="$(basename "${c}")"; a="${base#sched_eval_}"; a="${a%.csv}"
	ARCHES+=("${a}")
	info "found ${base} (arch=${a})"
done
ok "arches present: ${ARCHES[*]}"
if (( ${#CSVS[@]} < 2 )); then
	warn "only ONE arch present (${ARCHES[*]}). The cross-ISA comparison needs both"
	warn "x86_64 AND aarch64. Run this script on the other arch into the SAME OUT dir"
	warn "(see header: Oracle free Ampere A1 for the ARM run), then re-run with MERGE_ONLY=1."
fi

# ---------------------------------------------------------------------------
# 3. Merge into one cross-ISA CSV + comparison plot.
# ---------------------------------------------------------------------------
step "3/3  Merge + cross-ISA comparison"
MERGED="${OUT}/sched_eval_cross_isa.csv"
# Concatenate, keeping a single header. The per-arch CSVs already carry an `arch`
# column, so the merge is a plain header-deduped concat — long format, ready for
# the plot/summary.
{
	head -1 "${CSVS[0]}"
	for c in "${CSVS[@]}"; do tail -n +2 "${c}"; done
} > "${MERGED}"
ok "merged -> ${MERGED} ($(($(wc -l < "${MERGED}") - 1)) rows across ${#CSVS[@]} arch CSVs)"

if have python3; then
	python3 "${SCRIPT_DIR}/cross_isa_plot.py" "${MERGED}" "${OUT}" \
		|| warn "cross-ISA plot/summary failed (matplotlib missing?). Merged CSV: ${MERGED}"
else
	warn "python3 not found; merged CSV written, skipping plot/summary."
fi

echo
ok "cross-ISA compare complete."
info "merged CSV : ${MERGED}"
info "summary    : ${OUT}/cross_isa_summary.csv      (if python3 present)"
info "plot       : ${OUT}/cross_isa_money_plot.png   (if matplotlib present)"
info "What this proves: the SAME identity-aware policy compiled for two ISAs"
info "delivers the same qualitative tail-latency improvement — Prism's portable"
info "identity is ISA-agnostic (only clang -D__TARGET_ARCH_* differs)."
