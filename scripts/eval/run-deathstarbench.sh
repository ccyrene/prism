#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-deathstarbench.sh — the realistic-microservice tail-latency experiment.
#
# Brings up DeathStarBench's socialNetwork (a 30+ microservice app: nginx, a set
# of Thrift services, MongoDB/Redis/Memcached) under docker compose, then drives
# it with wrk2 at a FIXED request rate, sweeps the offered load, and captures
# tail latency (p50/p99/p99.9) under each scheduler (baseline / scx_layered /
# scx_prism). This is the "does identity-aware scheduling help a REAL service
# mesh?" experiment, complementary to the synthetic run-sched-eval.sh.
#
# ===========================================================================
# WHY wrk2 AND NOT wrk  (the coordinated-omission point — read this)
# ===========================================================================
# Classic `wrk` is a CLOSED-LOOP load generator: each connection fires the next
# request only AFTER the previous response returns. When the server stalls, wrk
# stalls with it and simply STOPS issuing requests during the stall — so the
# requests that WOULD have been slow are never sent, and the slow responses that
# were "in flight" are under-counted. This is Gil Tene's "Coordinated Omission":
# the load generator coordinates with the very latency it is trying to measure,
# and the reported tail (p99/p99.9) is wildly optimistic — often off by orders
# of magnitude precisely where it matters most.
#
# wrk2 (https://github.com/giltene/wrk2) fixes this by being a CONSTANT-THROUGHPUT
# generator: you specify a target rate -R (req/s) and wrk2 schedules each request
# at its INTENDED send time. If the server stalls, the requests that should have
# been sent during the stall are still counted, and their latency is measured
# from their INTENDED start time (CO correction). The result is an honest tail.
# We therefore ALWAYS use wrk2 with an explicit -R, and we sweep -R to find where
# each scheduler's tail blows up. Reporting p99/p99.9 from plain wrk here would
# be scientific malpractice for a scheduling paper.
# ===========================================================================
#
# RUNS ONLY where Docker + wrk2 + (for the scx legs) a 6.12+ sched_ext kernel are
# available. Everything is presence-checked; on the dev box (no docker daemon /
# no wrk2 / 5.15) it prints exactly what is missing and exits without pretending.
#
# Usage:
#   sudo scripts/eval/run-deathstarbench.sh
#   RATES="500 1000 2000 4000" DURATION=30 scripts/eval/run-deathstarbench.sh
#   DSB_DIR=/opt/DeathStarBench scripts/eval/run-deathstarbench.sh
#
# Env knobs:
#   DSB_DIR     where to clone/find DeathStarBench    (default /opt/DeathStarBench)
#   RATES       wrk2 target rates (req/s) to sweep    (default "500 1000 2000 4000 8000")
#   DURATION    seconds per (rate,scheduler) point    (default 30)
#   CONNS       wrk2 connections                      (default 64)
#   THREADS     wrk2 threads                          (default "$(nproc)")
#   OUT         output dir                            (default scripts/eval/results)
#   PRISM_OBJ / LOADER / SCX_DIR  — as in run-sched-eval.sh
#   FRONTEND_URL  the socialNetwork nginx URL         (default http://localhost:8080)

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)}"

DSB_DIR="${DSB_DIR:-/opt/DeathStarBench}"
RATES="${RATES:-500 1000 2000 4000 8000}"
DURATION="${DURATION:-30}"
CONNS="${CONNS:-64}"
THREADS="${THREADS:-$(nproc 2>/dev/null || echo 4)}"
OUT="${OUT:-${SCRIPT_DIR}/results}"
SCX_DIR="${SCX_DIR:-/opt/scx}"
PRISM_OBJ="${PRISM_OBJ:-${REPO_DIR}/bpf/scx_prism.bpf.o}"
LOADER="${LOADER:-${REPO_DIR}/loader/scx_prism_loader}"
FRONTEND_URL="${FRONTEND_URL:-http://localhost:8080}"
ARCH="$(uname -m)"
KREL="$(uname -r)"

mkdir -p "${OUT}"
CSV="${OUT}/dsb_socialnet_${ARCH}.csv"

if [[ -t 1 ]]; then
	C_BLUE=$'\033[1;34m'; C_GREEN=$'\033[1;32m'; C_YELLOW=$'\033[1;33m'
	C_RED=$'\033[1;31m'; C_DIM=$'\033[2m'; C_RST=$'\033[0m'
else
	C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_DIM=""; C_RST=""
fi
step() { echo; echo "${C_BLUE}==> $*${C_RST}"; }
info() { echo "    $*"; }
ok()   { echo "    ${C_GREEN}OK${C_RST}: $*"; }
warn() { echo "    ${C_YELLOW}WARN${C_RST}: $*" >&2; }
die()  { echo "${C_RED}ERROR:${C_RST} $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

echo "${C_DIM}repo:   ${REPO_DIR}${C_RST}"
echo "${C_DIM}dsb:    ${DSB_DIR}${C_RST}"
echo "${C_DIM}arch:   ${ARCH}  kernel ${KREL}${C_RST}"
echo "${C_DIM}rates:  ${RATES} req/s  x ${DURATION}s${C_RST}"

SUDO=""
if [[ "$(id -u)" -ne 0 ]] && have sudo; then SUDO="sudo"; fi

# ---------------------------------------------------------------------------
# 1. Preflight: docker, compose, wrk2, git. Hard requirements for THIS script —
# unlike run-sched-eval.sh there is no useful "baseline-only" mode without the
# app and the CO-correct load gen, so we fail clearly if they are missing.
# ---------------------------------------------------------------------------
step "1/6  Preflight"

MISSING=()
have git || MISSING+=("git")
if have docker; then
	if ! docker info >/dev/null 2>&1; then
		warn "docker present but daemon not reachable (no root / not running)."
		MISSING+=("docker-daemon")
	else
		ok "docker: $(docker --version 2>/dev/null)"
	fi
else
	MISSING+=("docker")
fi

# compose: prefer the v2 plugin (`docker compose`), fall back to docker-compose.
COMPOSE=""
if docker compose version >/dev/null 2>&1; then
	COMPOSE="docker compose"
elif have docker-compose; then
	COMPOSE="docker-compose"
else
	MISSING+=("docker-compose")
fi
[[ -n "${COMPOSE}" ]] && ok "compose: ${COMPOSE}"

# wrk2 — REQUIRED (see CO note above). Accept a few common binary names.
WRK2=""
for cand in wrk2 wrk2-bin; do
	if have "${cand}"; then WRK2="${cand}"; break; fi
done
# Some packagers install wrk2 AS `wrk`. Detect the CO-correct variant: wrk2's
# usage string mentions a required rate flag (-R). Only accept `wrk` if it does.
if [[ -z "${WRK2}" ]] && have wrk; then
	if wrk --help 2>&1 | grep -qiE -- '-R[, ].*rate|requests.*second'; then
		WRK2="wrk"
		warn "using 'wrk' binary that advertises -R (looks like wrk2). Confirm it is the CO-correcting wrk2 fork."
	fi
fi
if [[ -n "${WRK2}" ]]; then
	ok "wrk2 load generator: ${WRK2}"
else
	MISSING+=("wrk2")
	warn "wrk2 not found. Build it:"
	warn "    git clone https://github.com/giltene/wrk2 && make -C wrk2 && sudo cp wrk2/wrk /usr/local/bin/wrk2"
	warn "  DO NOT substitute plain wrk: it suffers coordinated omission and will"
	warn "  under-report p99/p99.9 by orders of magnitude (see header)."
fi

if (( ${#MISSING[@]} > 0 )); then
	die "missing prerequisites: ${MISSING[*]} . Install them (see warnings) and re-run. This script needs Docker + a CO-correct wrk2."
fi

# sched_ext availability (gates the scx legs; baseline DSB run still works).
SCHED_EXT=0
[[ -d /sys/kernel/sched_ext ]] && SCHED_EXT=1
if [[ "${SCHED_EXT}" -eq 1 ]]; then ok "sched_ext live (scx legs enabled)"; else warn "no sched_ext: only the baseline leg will run"; fi

# ---------------------------------------------------------------------------
# 2. Clone DeathStarBench + locate socialNetwork compose.
# ---------------------------------------------------------------------------
step "2/6  DeathStarBench socialNetwork"
if [[ ! -d "${DSB_DIR}/.git" ]]; then
	info "cloning DeathStarBench into ${DSB_DIR}"
	$SUDO mkdir -p "$(dirname "${DSB_DIR}")"
	$SUDO git clone --depth 1 https://github.com/delimitrou/DeathStarBench "${DSB_DIR}" \
		|| die "git clone DeathStarBench failed"
else
	ok "DeathStarBench already at ${DSB_DIR}"
fi

SN_DIR="${DSB_DIR}/socialNetwork"
[[ -d "${SN_DIR}" ]] || die "socialNetwork dir missing under ${DSB_DIR}"
COMPOSE_FILE=""
for cand in "${SN_DIR}/docker-compose.yml" "${SN_DIR}/docker-compose.yaml"; do
	[[ -f "${cand}" ]] && { COMPOSE_FILE="${cand}"; break; }
done
[[ -n "${COMPOSE_FILE}" ]] || die "no docker-compose.yml in ${SN_DIR}"
ok "compose file: ${COMPOSE_FILE}"

# The wrk2 Lua script that composes the social-graph mix (compose-post, read
# home/user timelines) ships with DSB under wrk2/scripts/social-network/.
LUA_DIR="${SN_DIR}/wrk2/scripts/social-network"
LUA_MIXED="${LUA_DIR}/compose-post.lua"
if [[ -f "${LUA_DIR}/mixed-workload.lua" ]]; then LUA_MIXED="${LUA_DIR}/mixed-workload.lua"; fi
if [[ -f "${LUA_MIXED}" ]]; then
	ok "wrk2 workload script: ${LUA_MIXED}"
else
	warn "no DSB wrk2 lua script found under ${LUA_DIR}; will drive a plain GET on ${FRONTEND_URL}"
	LUA_MIXED=""
fi

# ---------------------------------------------------------------------------
# compose up / down helpers.
# ---------------------------------------------------------------------------
dsb_up() {
	info "bringing up socialNetwork (${COMPOSE} -f ... up -d)"
	( cd "${SN_DIR}" && $SUDO ${COMPOSE} -f "${COMPOSE_FILE}" up -d ) \
		|| die "compose up failed"
	# Wait for the frontend to answer. DSB also needs its social graph
	# initialised (scripts/init_social_graph.py) for the mixed workload; we run
	# it if present.
	info "waiting for frontend at ${FRONTEND_URL} ..."
	local i
	for i in $(seq 1 60); do
		if curl -fsS -o /dev/null "${FRONTEND_URL}" 2>/dev/null; then break; fi
		sleep 2
	done
	curl -fsS -o /dev/null "${FRONTEND_URL}" 2>/dev/null \
		|| warn "frontend not responding at ${FRONTEND_URL} after 120s (continuing; load gen may fail)"
	if [[ -f "${SN_DIR}/scripts/init_social_graph.py" ]] && have python3; then
		info "initialising social graph"
		( cd "${SN_DIR}" && python3 scripts/init_social_graph.py 2>/dev/null ) \
			|| warn "init_social_graph.py failed (graph may be empty; read mix still measures latency)"
	fi
}
dsb_down() {
	( cd "${SN_DIR}" && $SUDO ${COMPOSE} -f "${COMPOSE_FILE}" down -v >/dev/null 2>&1 ) || true
}

# ---------------------------------------------------------------------------
# scheduler attach/detach (shared shape with run-sched-eval.sh).
# ---------------------------------------------------------------------------
PRISM_PID=""; LAYERED_PID=""
attach_prism() {
	[[ "${SCHED_EXT}" -eq 1 && -x "${LOADER}" && -f "${PRISM_OBJ}" ]] || return 1
	$SUDO "${LOADER}" "${PRISM_OBJ}" >"${OUT}/.dsb_prism.log" 2>&1 & PRISM_PID=$!
	sleep 2
	[[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]] || { detach_prism; return 1; }
	ok "scx_prism attached"; return 0
}
detach_prism() { [[ -n "${PRISM_PID}" ]] || return 0; $SUDO kill -INT "${PRISM_PID}" 2>/dev/null || true; wait "${PRISM_PID}" 2>/dev/null || true; PRISM_PID=""; sleep 1; }
find_layered() {
	have scx_layered && { command -v scx_layered; return 0; }
	local c; c="$(find "${SCX_DIR}" -type f -name scx_layered -perm -u+x 2>/dev/null | head -1 || true)"
	[[ -n "${c}" ]] && { echo "${c}"; return 0; } ; return 1
}
attach_layered() {
	[[ "${SCHED_EXT}" -eq 1 ]] || return 1
	local b; b="$(find_layered)" || return 1
	$SUDO "${b}" >"${OUT}/.dsb_layered.log" 2>&1 & LAYERED_PID=$!
	sleep 2
	[[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]] || { detach_layered; return 1; }
	ok "scx_layered attached"; return 0
}
detach_layered() { [[ -n "${LAYERED_PID}" ]] || return 0; $SUDO kill -INT "${LAYERED_PID}" 2>/dev/null || true; wait "${LAYERED_PID}" 2>/dev/null || true; LAYERED_PID=""; sleep 1; }

cleanup() { detach_prism; detach_layered; dsb_down; }
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# run_wrk2: one (rate,scheduler) point. Echo "p50 p99 p999" in MILLISECONDS,
# parsed from wrk2's latency-distribution output (wrk2 prints a corrected HDR
# distribution when invoked with -R and the latency reporting).
# ---------------------------------------------------------------------------
run_wrk2() {
	local rate="$1"
	local args=(-t"${THREADS}" -c"${CONNS}" -d"${DURATION}s" -R"${rate}" -L)
	local out
	if [[ -n "${LUA_MIXED}" ]]; then
		out="$("${WRK2}" "${args[@]}" -s "${LUA_MIXED}" "${FRONTEND_URL}" 2>&1 || true)"
	else
		out="$("${WRK2}" "${args[@]}" "${FRONTEND_URL}" 2>&1 || true)"
	fi
	# Save the full report for the record.
	echo "${out}" > "${OUT}/.wrk2_${rate}.txt"
	# Parse the "Latency Distribution (HdrHistogram - ...)" block:
	#   50.000%   <v><unit>
	#   99.000%   <v><unit>
	#   99.900%   <v><unit>
	# convert each to milliseconds. wrk2 prints us/ms/s suffixes.
	local p50 p99 p999
	p50="$(awk '/50\.000%/  {print $2; exit}' <<<"${out}")"
	p99="$(awk '/99\.000%/  {print $2; exit}' <<<"${out}")"
	p999="$(awk '/99\.900%/ {print $2; exit}' <<<"${out}")"
	to_ms() { # strip unit suffix, normalise to ms
		local v="$1"
		case "${v}" in
			*us)  awk -v x="${v%us}" 'BEGIN{printf "%.4f", x/1000.0}' ;;
			*ms)  awk -v x="${v%ms}" 'BEGIN{printf "%.4f", x}' ;;
			*s)   awk -v x="${v%s}"  'BEGIN{printf "%.4f", x*1000.0}' ;;
			"")   echo "0" ;;
			*)    awk -v x="${v}"    'BEGIN{printf "%.4f", x}' ;;  # assume ms
		esac
	}
	echo "$(to_ms "${p50}") $(to_ms "${p99}") $(to_ms "${p999}")"
}

# ---------------------------------------------------------------------------
# 3-5. Sweep load x scheduler.
# CSV: scheduler,arch,kernel,rate_rps,p50_ms,p99_ms,p999_ms
# ---------------------------------------------------------------------------
step "3/6  Bring up the app"
dsb_up

step "4/6  Sweep load x scheduler (wrk2 fixed-rate, CO-corrected)"
echo "scheduler,arch,kernel,rate_rps,p50_ms,p99_ms,p999_ms" > "${CSV}"

sweep_under() {
	local sched="$1" r
	for r in ${RATES}; do
		read -r p50 p99 p999 < <(run_wrk2 "${r}")
		echo "${sched},${ARCH},${KREL},${r},${p50},${p99},${p999}" >> "${CSV}"
		printf "    %-12s %6s rps -> p50=%-8s p99=%-8s p99.9=%-8s ms\n" \
			"${sched}" "${r}" "${p50}" "${p99}" "${p999}"
	done
}

# (a) baseline (no scx).
info "[a] baseline (vanilla CFS/EEVDF)"
sweep_under "baseline"

# (b) scx_layered if available.
if [[ "${SCHED_EXT}" -eq 1 ]] && find_layered >/dev/null 2>&1 && attach_layered; then
	info "[b] scx_layered"
	sweep_under "scx_layered"
	detach_layered
else
	warn "[b] scx_layered SKIPPED (no sched_ext / binary / attach failed)."
fi

# (c) scx_prism.
if attach_prism; then
	info "[c] scx_prism (identity-aware)"
	sweep_under "scx_prism"
	detach_prism
else
	warn "[c] scx_prism SKIPPED (no sched_ext / loader / object / attach failed)."
fi

step "5/6  Tear down app"
dsb_down
ok "wrote ${CSV} ($(($(wc -l < "${CSV}") - 1)) rows)"

# ---------------------------------------------------------------------------
# 6. Plot the rate-vs-tail curve per scheduler.
# ---------------------------------------------------------------------------
step "6/6  Plot rate-vs-tail (the DSB money plot)"
if have python3; then
	python3 "${SCRIPT_DIR}/dsb_plot.py" "${CSV}" "${OUT}" || warn "plot step failed (matplotlib missing?). CSV is at ${CSV}."
else
	warn "python3 not found; skipping plot. Raw CSV: ${CSV}"
fi

echo
ok "DeathStarBench eval complete."
info "raw CSV : ${CSV}"
info "plot    : ${OUT}/dsb_money_plot_${ARCH}.png (if matplotlib present)"
info "Interpretation: the rate at which p99/p99.9 'hockey-sticks' upward is the"
info "knee; a higher knee (and lower tail before it) under scx_prism means the"
info "identity-aware scheduler sustains more load before tail-latency collapse."
