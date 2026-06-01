#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# build.sh — build BOTH scx_bpfland binaries (vanilla + Prism) from ONE scx tree.
#
# This enforces the eval's honesty guard — "vanilla and bpfland+Prism use
# identical knobs; the only difference is reading the bus" — by CONSTRUCTION:
#   * scx_bpfland_vanilla is built from main.bpf.c.orig (stock bpfland),
#   * scx_bpfland_prism   is built from main.bpf.c     (the +bus retrofit),
# with the SAME cargo invocation. The ONLY input that differs is the .bpf.c, and
# `diff main.bpf.c.orig main.bpf.c` is pure additions (a leading bus-read block;
# the task_dl heuristic body and every knob are byte-identical).
#
# Run INSIDE the privileged prism-eval container (needs scx v1.1.0 at $SCX and a
# cargo toolchain on a 6.12+ host so the BPF object can later load):
#   docker exec prism-eval bash /work/prism/integrations/bpfland/build.sh
#
# Env: SCX=/opt/scx  OUT=$SCX/target/release  CARGO=cargo
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCX="${SCX:-/opt/scx}"
OUT="${OUT:-${SCX}/target/release}"
BPF_SRC="${BPF_SRC:-${SCX}/scheds/rust/scx_bpfland/src/bpf/main.bpf.c}"
CARGO="${CARGO:-cargo}"
export PATH="${HOME}/.cargo/bin:${PATH}"

[[ -d "${SCX}" ]] || { echo "ERROR: scx tree not found at ${SCX} (set SCX=...)"; exit 1; }
[[ -f "${HERE}/main.bpf.c" && -f "${HERE}/main.bpf.c.orig" ]] || {
  echo "ERROR: missing main.bpf.c / main.bpf.c.orig in ${HERE}"; exit 1; }

# Restore the scheduler's original source on exit so the scx tree isn't left
# carrying our retrofit (keeps re-runs deterministic).
ORIG_BACKUP="$(mktemp)"; cp -f "${BPF_SRC}" "${ORIG_BACKUP}" 2>/dev/null || true
cleanup(){ [[ -s "${ORIG_BACKUP}" ]] && cp -f "${ORIG_BACKUP}" "${BPF_SRC}" 2>/dev/null || true; rm -f "${ORIG_BACKUP}"; }
trap cleanup EXIT

build_one(){ # <source .bpf.c>  <output binary name>
  local src="$1" name="$2"
  echo "== building ${name} from $(basename "${src}") =="
  cp -f "${src}" "${BPF_SRC}"
  ( cd "${SCX}" && ${CARGO} build --release -p scx_bpfland )
  cp -f "${OUT}/scx_bpfland" "${OUT}/${name}"
  echo "   -> ${OUT}/${name}"
}

# Same cargo command for both; the ONLY difference is which .bpf.c is in place.
build_one "${HERE}/main.bpf.c.orig" scx_bpfland_vanilla
build_one "${HERE}/main.bpf.c"      scx_bpfland_prism
echo "done: vanilla + prism built from one tree (identical knobs by construction)."
