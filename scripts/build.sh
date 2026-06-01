#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# build.sh — build the Prism artifacts from a clean checkout.
#
#   1. prismd        : a fully static (CGO_ENABLED=0) linux binary, stripped.
#   2. compose_demo  : the composability-demo BPF object (net + observe facets).
#                      Compiles on any modern clang/kernel.
#   3. scx_prism     : the sched_ext scheduler BPF object. Only attempted when
#                      the scx tooling (<scx/common.bpf.h>) and a 6.12+ kernel
#                      are present; otherwise skipped with a clear note (see
#                      bpf/README.md for why it can't build on a 5.15 dev box).
#
# Usage:
#   scripts/build.sh                 # build everything it can
#   OUT_DIR=dist scripts/build.sh    # choose output dir (default: ./dist)
#   SKIP_BPF=1 scripts/build.sh      # Go binary only
#
# Env knobs:
#   OUT_DIR     output directory for built artifacts        (default: dist)
#   GOOS GOARCH cross-compile target for prismd             (default: host)
#   CLANG       clang binary for the BPF objects            (default: clang)
#   SCX_INCLUDE path to the scx include dir (has scx/common.bpf.h) for scx_prism
#   SKIP_BPF=1  skip the BPF objects entirely
set -euo pipefail

# Resolve repo root from this script's location so it works from any CWD.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"
cd "${REPO_ROOT}"

OUT_DIR="${OUT_DIR:-dist}"
CLANG="${CLANG:-clang}"
BPF_DIR="bpf"
GOFLAGS_VAL="${GOFLAGS:--mod=mod}"

mkdir -p "${OUT_DIR}"

echo "==> Prism build"
echo "    repo:    ${REPO_ROOT}"
echo "    out dir: ${OUT_DIR}"
echo

# ---------------------------------------------------------------------------
# 1. Static prismd binary.
# ---------------------------------------------------------------------------
echo "==> [1/3] building static prismd (CGO_ENABLED=0)"
GO_VERSION="$(go version 2>/dev/null || echo 'go: not found')"
echo "    ${GO_VERSION}"

CGO_ENABLED=0 GOFLAGS="${GOFLAGS_VAL}" \
  go build -trimpath -ldflags "-s -w" -o "${OUT_DIR}/prismd" ./cmd/prismd

echo "    wrote ${OUT_DIR}/prismd"
if command -v file >/dev/null 2>&1; then
  file "${OUT_DIR}/prismd"
  if file "${OUT_DIR}/prismd" | grep -q 'statically linked'; then
    echo "    OK: statically linked"
  else
    echo "    WARNING: binary does not look statically linked (CGO leaked in?)" >&2
  fi
fi
echo

# ---------------------------------------------------------------------------
# BPF objects (optional).
# ---------------------------------------------------------------------------
if [[ "${SKIP_BPF:-0}" == "1" ]]; then
  echo "==> BPF objects skipped (SKIP_BPF=1)"
  echo
  echo "==> build complete (Go binary only): ${OUT_DIR}/prismd"
  exit 0
fi

if ! command -v "${CLANG}" >/dev/null 2>&1; then
  echo "==> clang ('${CLANG}') not found — skipping BPF objects." >&2
  echo "    Install clang/LLVM (>=12, BPF target) or set CLANG=... to build them." >&2
  echo
  echo "==> build complete (Go binary only): ${OUT_DIR}/prismd"
  exit 0
fi

echo "    $("${CLANG}" --version | head -1)"
echo

# Common BPF compile flags. -Ibpf resolves "vmlinux.h"/"libprism.bpf.h";
# -Ibpf/include picks up the local libbpf shim when libbpf-dev is absent (on a
# host WITH libbpf-dev the system <bpf/bpf_helpers.h> shadows the shim). See
# bpf/README.md.
BPF_CFLAGS=(-O2 -g -target bpf -D__TARGET_ARCH_x86 -I"${BPF_DIR}" -I"${BPF_DIR}/include")

# ---------------------------------------------------------------------------
# 2. compose_demo.bpf.o — builds anywhere.
# ---------------------------------------------------------------------------
echo "==> [2/3] compiling compose_demo.bpf.c (net + observe facets)"
COMPOSE_OBJ="${OUT_DIR}/compose_demo.bpf.o"
"${CLANG}" "${BPF_CFLAGS[@]}" -c "${BPF_DIR}/compose_demo.bpf.c" -o "${COMPOSE_OBJ}"
echo "    wrote ${COMPOSE_OBJ}"

# Verify the expected program + map sections are present.
READELF=""
for cand in llvm-readelf llvm-readelf-18 readelf; do
  if command -v "${cand}" >/dev/null 2>&1; then READELF="${cand}"; break; fi
done
if [[ -n "${READELF}" ]]; then
  if "${READELF}" -S "${COMPOSE_OBJ}" 2>/dev/null \
       | grep -Eq 'cgroup_skb|sched_switch|\.maps'; then
    echo "    OK: found cgroup_skb / sched_switch / .maps sections"
  else
    echo "    WARNING: expected BPF sections not found in ${COMPOSE_OBJ}" >&2
  fi
fi
echo

# ---------------------------------------------------------------------------
# 3. scx_prism.bpf.o — needs a 6.12+ kernel BTF + the scx tooling.
# ---------------------------------------------------------------------------
echo "==> [3/3] sched_ext scheduler (scx_prism.bpf.c)"

# Locate scx/common.bpf.h: explicit SCX_INCLUDE wins, else probe a couple of
# well-known locations.
scx_inc=""
if [[ -n "${SCX_INCLUDE:-}" && -f "${SCX_INCLUDE}/scx/common.bpf.h" ]]; then
  scx_inc="${SCX_INCLUDE}"
else
  for cand in /usr/include /usr/local/include "${BPF_DIR}/include"; do
    if [[ -f "${cand}/scx/common.bpf.h" ]]; then scx_inc="${cand}"; break; fi
  done
fi

# Best-effort kernel-version gate (sched_ext needs >= 6.12). Informational only;
# the real gate is whether the header + 6.12 BTF actually compile.
kver="$(uname -r 2>/dev/null || echo 0.0)"
kmajor="${kver%%.*}"
rest="${kver#*.}"; kminor="${rest%%.*}"
kmajor="${kmajor//[!0-9]/}"; kminor="${kminor//[!0-9]/}"
kmajor="${kmajor:-0}"; kminor="${kminor:-0}"
scx_kernel_ok=0
if (( kmajor > 6 || (kmajor == 6 && kminor >= 12) )); then
  scx_kernel_ok=1
fi

if [[ -z "${scx_inc}" ]]; then
  echo "    SKIP: scx tooling not found (no scx/common.bpf.h)."
  echo "          Set SCX_INCLUDE=<path-to-scx>/include on a 6.12+ host. See bpf/README.md."
elif (( scx_kernel_ok == 0 )); then
  echo "    SKIP: kernel ${kver} < 6.12 — sched_ext unavailable; scx_prism cannot load here."
  echo "          (scx headers found at ${scx_inc}, but a 6.12+ vmlinux.h/BTF is required.) See bpf/README.md."
else
  SCX_OBJ="${OUT_DIR}/scx_prism.bpf.o"
  echo "    scx tooling: ${scx_inc} ; kernel ${kver} (>=6.12) — compiling"
  "${CLANG}" "${BPF_CFLAGS[@]}" -I"${scx_inc}" -c "${BPF_DIR}/scx_prism.bpf.c" -o "${SCX_OBJ}"
  echo "    wrote ${SCX_OBJ}"
fi
echo

echo "==> build complete. Artifacts in ${OUT_DIR}/:"
ls -la "${OUT_DIR}/"
