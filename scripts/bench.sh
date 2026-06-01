#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# bench.sh — run the full Prism evaluation and print the headline numbers.
#
# Three layers, all of which run on any host (no root / no sched_ext needed):
#   1. Go control-plane bench  : go run ./bench/cmd/prismbench  -> bench/results/
#        results.json, resolution_samples.csv, scale.csv
#   2. Native C microbench     : clang -O2 bench/native/microbench.c, then parse
#        its key/value output into bench/results/native.json (consumed by plot.py
#        for the "Native C (~ kernel BPF map)" layer of the bar chart).
#   3. Plots                   : python3 bench/plot.py -> money_plot_cdf.png,
#        scale.png, layers_bar.png in bench/results/.
#
# Usage:
#   scripts/bench.sh                 # full run, default output dir bench/results
#   scripts/bench.sh <results-dir>   # override output dir
#   ITERS=2000000 scripts/bench.sh   # native microbench iteration count
#   SKIP_PLOT=1 scripts/bench.sh     # skip the matplotlib step
#
# Env knobs:
#   ITERS       native microbench iterations           (default: 1000000)
#   CLANG       clang binary for the native microbench  (default: clang)
#   SKIP_PLOT=1 skip python3 bench/plot.py
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"
cd "${REPO_ROOT}"

RESULTS_DIR="${1:-bench/results}"
CLANG="${CLANG:-clang}"
ITERS="${ITERS:-1000000}"
GOFLAGS_VAL="${GOFLAGS:--mod=mod}"

mkdir -p "${RESULTS_DIR}"

echo "==> Prism benchmark suite"
echo "    repo:    ${REPO_ROOT}"
echo "    results: ${RESULTS_DIR}"
echo

# ---------------------------------------------------------------------------
# 1. Go control-plane benchmark.
# ---------------------------------------------------------------------------
echo "==> [1/3] Go control-plane bench (go run ./bench/cmd/prismbench)"
GOFLAGS="${GOFLAGS_VAL}" go run ./bench/cmd/prismbench "${RESULTS_DIR}"
echo

# ---------------------------------------------------------------------------
# 2. Native C microbench.
# ---------------------------------------------------------------------------
echo "==> [2/3] native C microbench (clang -O2 bench/native/microbench.c)"
NATIVE_JSON="${RESULTS_DIR}/native.json"
if ! command -v "${CLANG}" >/dev/null 2>&1; then
  echo "    WARNING: clang ('${CLANG}') not found — skipping native microbench." >&2
  if [[ -f "${NATIVE_JSON}" ]]; then
    echo "    Reusing existing ${NATIVE_JSON} for plotting." >&2
  else
    echo "    No existing native.json; the layers_bar plot will be skipped." >&2
  fi
else
  UBENCH_BIN="$(mktemp -t prism_ubench.XXXXXX)"
  trap 'rm -f "${UBENCH_BIN}"' EXIT
  "${CLANG}" -O2 -Wall -Wextra -o "${UBENCH_BIN}" bench/native/microbench.c
  echo "    running ${ITERS} iterations per path..."
  UBENCH_OUT="$("${UBENCH_BIN}" "${ITERS}")"
  echo "${UBENCH_OUT}" | sed 's/^/    /'

  # Parse the machine-readable key/value output into native.json (the schema
  # bench/plot.py expects: prism_ns, baseline_ns, ratio, *_p99_ns).
  get() { echo "${UBENCH_OUT}" | awk -v k="$1" '$1==k {print $2; exit}'; }
  prism_ns="$(get PRISM_NS_PER_OP)"
  base_ns="$(get BASELINE_NS_PER_OP)"
  ratio="$(get RATIO)"
  prism_p99="$(get PRISM_P99_NS)"
  base_p99="$(get BASELINE_P99_NS)"

  cat > "${NATIVE_JSON}" <<EOF
{"prism_ns": ${prism_ns}, "baseline_ns": ${base_ns}, "ratio": ${ratio}, "prism_p99_ns": ${prism_p99}, "baseline_p99_ns": ${base_p99}, "iterations": ${ITERS}, "note": "standalone userspace C, clang -O2, open-addressing hash lookup vs scx_layered-style cgroup-path classify; closest available proxy for the kernel BPF-map hot path"}
EOF
  echo "    wrote ${NATIVE_JSON}"
fi
echo

# ---------------------------------------------------------------------------
# 3. Plots.
# ---------------------------------------------------------------------------
echo "==> [3/3] plots (python3 bench/plot.py)"
if [[ "${SKIP_PLOT:-0}" == "1" ]]; then
  echo "    SKIP: SKIP_PLOT=1"
elif ! command -v python3 >/dev/null 2>&1; then
  echo "    WARNING: python3 not found — skipping plots." >&2
else
  # plot.py reads/writes its own bench/results dir; it needs matplotlib + numpy.
  if python3 -c 'import matplotlib, numpy' >/dev/null 2>&1; then
    python3 bench/plot.py | sed 's/^/    /'
  else
    echo "    WARNING: matplotlib/numpy not importable — skipping plots." >&2
    echo "             pip install matplotlib numpy   (then re-run, or SKIP_PLOT=1)" >&2
  fi
fi
echo

# ---------------------------------------------------------------------------
# Headline numbers (parsed back out of results.json + native.json).
# ---------------------------------------------------------------------------
echo "==> headline numbers"
RESULTS_JSON="${RESULTS_DIR}/results.json"
if command -v python3 >/dev/null 2>&1 && [[ -f "${RESULTS_JSON}" ]]; then
  python3 - "${RESULTS_JSON}" "${NATIVE_JSON}" <<'PY'
import json, os, sys
res_path, native_path = sys.argv[1], sys.argv[2]
res = json.load(open(res_path))
scen = {s["scenario"]: s for s in res.get("scenarios", [])}
def med(name):
    s = scen.get(name)
    return s["p50"] if s else float("nan")
host = res.get("host", {})
print(f"    host        : {host.get('cpu','?')}  kernel {host.get('kernel','?')}  {host.get('go_version','?')}")
print(f"    Go resolve  : prism p50={med('resolution_prism'):.2f} ns   "
      f"baseline p50={med('resolution_baseline'):.1f} ns   "
      f"ratio={res.get('resolution_ratio_median', float('nan')):.1f}x")
print(f"    Go e2e add  : p50={med('e2e_pod_add'):.1f} ns   churn p50={med('churn_add_delete'):.1f} ns")
if os.path.isfile(native_path):
    n = json.load(open(native_path))
    print(f"    Native C    : prism={n['prism_ns']:.2f} ns   baseline={n['baseline_ns']:.1f} ns   "
          f"ratio={n['ratio']:.0f}x  (~ kernel BPF-map lookup proxy)")
print(f"    Plots       : {os.path.dirname(res_path)}/{{money_plot_cdf,scale,layers_bar}}.png")
PY
else
  echo "    (results.json not found or python3 missing; see ${RESULTS_DIR}/)"
fi
echo
echo "==> bench complete. All outputs under ${RESULTS_DIR}/"
