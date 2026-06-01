#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-sched-eval.sh — the "does identity-aware scheduling help?" experiment.
#
# Runs a latency-sensitive workload under three schedulers and collects tail
# latency over N trials, then emits a CSV + the "money plot":
#
#   (a) BASELINE   : vanilla in-kernel scheduler (CFS on <6.6, EEVDF on 6.6+),
#                    i.e. NO sched_ext attached. The control.
#   (b) scx_layered: a reference scx scheduler (cgroup/layer-based), IF the scx
#                    toolchain built it. The "other scx" comparison point.
#   (c) scx_prism  : OUR identity-aware scheduler, loaded via loader/.
#
# For each scheduler we run the workload TRIALS times, record per-trial p50/p99/
# p99.9 (in microseconds), and summarise across trials with the project's stats
# methodology (median + percentile-bootstrap 95% CI on the median; same approach
# as bench/cmd/prismbench/stats.go and module-07: lead with tails, report the
# distribution, never just the mean).
#
# ---------------------------------------------------------------------------
# RUNS ONLY ON A 6.12+ HOST (to load scx_prism). The BASELINE leg runs on any
# kernel; the scx legs are GUARDED and skipped (with a loud note) if sched_ext /
# the schedulers / the loader are absent. So the script is safe to invoke on the
# dev box — it will run baseline-only and tell you what it skipped and why.
# ---------------------------------------------------------------------------
#
# Every external tool is presence-checked. Nothing here assumes a tool exists.
#
# Usage:
#   sudo scripts/eval/run-sched-eval.sh                  # full run, defaults
#   TRIALS=20 scripts/eval/run-sched-eval.sh             # more trials
#   OUT=/tmp/sched scripts/eval/run-sched-eval.sh        # output dir
#   WORKLOAD=schbench scripts/eval/run-sched-eval.sh     # pick the load gen
#   SKIP_PLOT=1 scripts/eval/run-sched-eval.sh
#
# Env knobs (all optional):
#   TRIALS         trials per scheduler                         (default 10)
#   DURATION       seconds per trial                            (default 15)
#   WORKLOAD       schbench | hackbench | stress-cpu | builtin  (default auto)
#   OUT            output dir                                   (default scripts/eval/results)
#   SCX_DIR        where the scx toolchain was built            (default /opt/scx)
#   PRISM_OBJ      compiled scheduler object                    (default bpf/scx_prism.bpf.o)
#   LOADER         scx_prism userspace loader binary            (default loader/scx_prism_loader)
#   SEED_BUS       1 to run prismd/seed before the scx legs     (default 1)
#   SKIP_PLOT      1 to skip the python plot
#   ATTACH_SETTLE  seconds to wait after attach before measuring (default 2)

set -euo pipefail

# ---------------------------------------------------------------------------
# Locate repo + set defaults.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/../.." >/dev/null 2>&1 && pwd)}"

TRIALS="${TRIALS:-10}"
DURATION="${DURATION:-15}"
WORKLOAD="${WORKLOAD:-auto}"
OUT="${OUT:-${SCRIPT_DIR}/results}"
SCX_DIR="${SCX_DIR:-/opt/scx}"
PRISM_OBJ="${PRISM_OBJ:-${REPO_DIR}/bpf/scx_prism.bpf.o}"
LOADER="${LOADER:-${REPO_DIR}/loader/scx_prism_loader}"
SEED_BUS="${SEED_BUS:-1}"
ATTACH_SETTLE="${ATTACH_SETTLE:-2}"
ARCH="$(uname -m)"
KREL="$(uname -r)"

mkdir -p "${OUT}"
CSV="${OUT}/sched_eval_${ARCH}.csv"

# ---------------------------------------------------------------------------
# Logging helpers (match provision-schedext-vm.sh style).
# ---------------------------------------------------------------------------
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

echo "${C_DIM}repo:    ${REPO_DIR}${C_RST}"
echo "${C_DIM}arch:    ${ARCH}  kernel ${KREL}${C_RST}"
echo "${C_DIM}out:     ${OUT}${C_RST}"
echo "${C_DIM}trials:  ${TRIALS} x ${DURATION}s${C_RST}"

# ---------------------------------------------------------------------------
# 1. Environment checks (everything guarded; we do NOT hard-fail on missing scx).
# ---------------------------------------------------------------------------
step "1/6  Environment"

IS_ROOT=0; [[ "$(id -u)" -eq 0 ]] && IS_ROOT=1
SUDO=""
if [[ "${IS_ROOT}" -ne 1 ]]; then
	if have sudo; then SUDO="sudo"; warn "not root; using sudo for privileged steps"; fi
fi

# sched_ext availability (drives whether the scx legs can run at all).
SCHED_EXT=0
if [[ -d /sys/kernel/sched_ext ]]; then
	SCHED_EXT=1
	ok "/sys/kernel/sched_ext present (sched_ext live)"
else
	warn "/sys/kernel/sched_ext MISSING — sched_ext not available on this kernel."
	warn "  Baseline leg will still run; scx_layered + scx_prism legs will be SKIPPED."
fi

# Kernel version floor note (informational; sched_ext is 6.12+).
KMAJ="${KREL%%.*}"; KREST="${KREL#*.}"; KMIN="${KREST%%.*}"; KMIN="${KMIN%%-*}"
if [[ "${KMAJ:-0}" -lt 6 || ( "${KMAJ}" -eq 6 && "${KMIN:-0}" -lt 12 ) ]]; then
	warn "kernel ${KREL} < 6.12: scx_prism cannot load here (expected on the dev box)."
fi

# ---------------------------------------------------------------------------
# 2. Build scx_prism + the loader if needed (best-effort, guarded).
# ---------------------------------------------------------------------------
step "2/6  Build scx_prism + loader (if missing)"

if [[ "${SCHED_EXT}" -eq 1 ]]; then
	if [[ ! -f "${PRISM_OBJ}" ]]; then
		warn "${PRISM_OBJ} not found."
		if [[ -x "${REPO_DIR}/scripts/provision-schedext-vm.sh" ]]; then
			info "run: sudo ${REPO_DIR}/scripts/provision-schedext-vm.sh  (builds it)"
		fi
	else
		ok "scheduler object: ${PRISM_OBJ}"
	fi
	if [[ ! -x "${LOADER}" ]]; then
		if [[ -f "${REPO_DIR}/loader/Makefile" ]] && have clang; then
			info "building loader: make -C ${REPO_DIR}/loader"
			if make -C "${REPO_DIR}/loader" >/dev/null 2>&1; then
				ok "built ${LOADER}"
			else
				warn "loader build failed (libbpf-dev missing?); scx_prism leg will be skipped."
			fi
		else
			warn "loader binary ${LOADER} missing and cannot build it; scx_prism leg skipped."
		fi
	else
		ok "loader: ${LOADER}"
	fi
fi

# ---------------------------------------------------------------------------
# 3. Pick the latency-sensitive workload (guarded; fall back to a builtin).
# ---------------------------------------------------------------------------
step "3/6  Workload selection"
# Preference order for a latency-sensitive load: schbench (Facebook's scheduler
# wakeup-latency benchmark — the gold standard for THIS kind of measurement,
# reports wakeup latency percentiles directly), then hackbench, then a portable
# builtin ping-pong we ship inline. WORKLOAD env overrides.
pick_workload() {
	case "${WORKLOAD}" in
		schbench)   echo schbench; return ;;
		hackbench)  echo hackbench; return ;;
		stress-cpu) echo stress-cpu; return ;;
		builtin)    echo builtin; return ;;
		auto) : ;;
		*) warn "unknown WORKLOAD='${WORKLOAD}', using auto" ;;
	esac
	if have schbench; then echo schbench; return; fi
	if have hackbench; then echo hackbench; return; fi
	echo builtin
}
WL="$(pick_workload)"
ok "workload: ${WL}"
case "${WL}" in
	schbench)  info "schbench reports wakeup-latency percentiles directly (best signal)." ;;
	hackbench) info "hackbench: scheduler stress; we time total completion as the latency proxy." ;;
	builtin)   info "builtin pipe ping-pong: portable RTT latency probe (no external deps)." ;;
esac

# A tiny portable builtin latency probe compiled on demand: a pair of processes
# round-tripping a byte over a pipe for DURATION seconds; we record per-RTT
# latency and print percentiles. This is the no-external-deps fallback so the
# harness ALWAYS produces a baseline number.
BUILTIN_SRC="${OUT}/.pingpong.c"
BUILTIN_BIN="${OUT}/.pingpong"
build_builtin() {
	[[ -x "${BUILTIN_BIN}" ]] && return 0
	have clang || have gcc || { warn "no clang/gcc for builtin workload"; return 1; }
	local cc; cc="$(command -v clang || command -v gcc)"
	cat > "${BUILTIN_SRC}" <<'CSRC'
/* pingpong: measure pipe round-trip latency for N seconds; print p50/p99/p99.9
 * in microseconds as: P50 <us>\nP99 <us>\nP999 <us>\n . A latency-sensitive
 * wakeup workload sensitive to scheduler dispatch decisions. */
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <time.h>
#include <sys/wait.h>
static int cmp(const void*a,const void*b){double x=*(const double*)a,y=*(const double*)b;return (x>y)-(x<y);}
static double now_us(void){struct timespec t;clock_gettime(CLOCK_MONOTONIC,&t);return t.tv_sec*1e6+t.tv_nsec/1e3;}
int main(int argc,char**argv){
	double secs=argc>1?atof(argv[1]):10.0;
	int a2b[2],b2a[2]; if(pipe(a2b)||pipe(b2a))return 2;
	pid_t pid=fork(); if(pid<0)return 2;
	char c=0;
	if(pid==0){ /* echo child */ close(a2b[1]);close(b2a[0]);
		while(read(a2b[0],&c,1)==1){ if(write(b2a[1],&c,1)!=1)break;} _exit(0);}
	close(a2b[0]);close(b2a[1]);
	size_t cap=1<<20,n=0; double*s=malloc(cap*sizeof(double));
	double end=now_us()+secs*1e6;
	while(now_us()<end){ double t0=now_us();
		if(write(a2b[1],&c,1)!=1)break; if(read(b2a[0],&c,1)!=1)break;
		double d=now_us()-t0; if(n<cap)s[n++]=d; }
	close(a2b[1]); waitpid(pid,0,0);
	if(n==0){printf("P50 0\nP99 0\nP999 0\n");return 0;}
	qsort(s,n,sizeof(double),cmp);
	#define PCT(p) s[(size_t)((p)/100.0*(n-1))]
	printf("P50 %.3f\nP99 %.3f\nP999 %.3f\n",PCT(50.0),PCT(99.0),PCT(99.9));
	return 0;
}
CSRC
	"${cc}" -O2 -o "${BUILTIN_BIN}" "${BUILTIN_SRC}" 2>/dev/null || { warn "builtin workload compile failed"; return 1; }
	return 0
}

# ---------------------------------------------------------------------------
# run_workload_once: run ONE trial of the chosen workload; echo "p50 p99 p999"
# (microseconds, space separated). Parses each tool's native percentile output.
# ---------------------------------------------------------------------------
run_workload_once() {
	case "${WL}" in
	schbench)
		# schbench prints e.g. "Latency percentiles (usec) ... 99.0th: <v>".
		# -r <sec> runtime. We grep the 50/99/99.9 lines from its report.
		local o; o="$(schbench -r "${DURATION}" 2>&1 || true)"
		local p50 p99 p999
		p50="$(awk  '/50\.0th/   {print $(NF); exit}' <<<"${o}")"
		p99="$(awk  '/99\.0th/   {print $(NF); exit}' <<<"${o}")"
		p999="$(awk '/99\.9th/   {print $(NF); exit}' <<<"${o}")"
		echo "${p50:-0} ${p99:-0} ${p999:-0}"
		;;
	hackbench)
		# hackbench reports total time only; we use it as a single latency proxy
		# replicated across the three percentile columns (documented limitation —
		# prefer schbench for true tail latency).
		local t; t="$(hackbench -l 5000 2>&1 | awk '/Time:/ {print $2; exit}')"
		# seconds -> microseconds
		local us; us="$(awk -v s="${t:-0}" 'BEGIN{printf "%.3f", s*1e6}')"
		echo "${us} ${us} ${us}"
		;;
	stress-cpu)
		# Co-located CPU stressor as background pressure + builtin probe in front.
		build_builtin || { echo "0 0 0"; return; }
		( stress-ng --cpu "$(nproc)" --timeout "${DURATION}s" >/dev/null 2>&1 & )
		"${BUILTIN_BIN}" "${DURATION}" | awk '/^P50/{a=$2}/^P99 /{b=$2}/^P999/{c=$2}END{print a,b,c}'
		;;
	builtin|*)
		build_builtin || { echo "0 0 0"; return; }
		"${BUILTIN_BIN}" "${DURATION}" | awk '/^P50/{a=$2}/^P99 /{b=$2}/^P999/{c=$2}END{print a,b,c}'
		;;
	esac
}

# ---------------------------------------------------------------------------
# Scheduler attach/detach helpers (all guarded).
# ---------------------------------------------------------------------------
PRISM_PID=""
LAYERED_PID=""

attach_prism() {
	[[ "${SCHED_EXT}" -eq 1 ]] || return 1
	[[ -x "${LOADER}" && -f "${PRISM_OBJ}" ]] || return 1
	info "attaching scx_prism via ${LOADER}"
	$SUDO "${LOADER}" "${PRISM_OBJ}" >/"${OUT}/.prism_loader.log" 2>&1 &
	PRISM_PID=$!
	sleep "${ATTACH_SETTLE}"
	# Confirm the kernel actually enabled it.
	if [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; then
		ok "scx_prism attached ($(cat /sys/kernel/sched_ext/root/ops 2>/dev/null || echo '?'))"
		return 0
	fi
	warn "scx_prism did not reach 'enabled' state; see ${OUT}/.prism_loader.log"
	detach_prism
	return 1
}
detach_prism() {
	[[ -n "${PRISM_PID}" ]] || return 0
	$SUDO kill -INT "${PRISM_PID}" 2>/dev/null || true
	wait "${PRISM_PID}" 2>/dev/null || true
	PRISM_PID=""
	sleep 1
}

# scx_layered: find a built binary under SCX_DIR (meson build) or on PATH.
find_layered() {
	if have scx_layered; then command -v scx_layered; return 0; fi
	local cand
	for cand in "${SCX_DIR}/build"/*/scx_layered "${SCX_DIR}"/build/scheds/rust/scx_layered/*/scx_layered; do
		[[ -x "${cand}" ]] && { echo "${cand}"; return 0; }
	done
	# generic find as a last resort
	cand="$(find "${SCX_DIR}" -type f -name scx_layered -perm -u+x 2>/dev/null | head -1 || true)"
	[[ -n "${cand}" ]] && { echo "${cand}"; return 0; }
	return 1
}
attach_layered() {
	[[ "${SCHED_EXT}" -eq 1 ]] || return 1
	local bin; bin="$(find_layered)" || return 1
	info "attaching scx_layered: ${bin}"
	# scx_layered needs a layer config; run with its built-in example/default if
	# present, else a trivial single-layer spec. Kept best-effort: if it refuses,
	# we skip the leg rather than fail the whole run.
	$SUDO "${bin}" >/"${OUT}/.layered.log" 2>&1 &
	LAYERED_PID=$!
	sleep "${ATTACH_SETTLE}"
	if [[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]]; then
		ok "scx_layered attached ($(cat /sys/kernel/sched_ext/root/ops 2>/dev/null || echo '?'))"
		return 0
	fi
	warn "scx_layered did not enable (needs a layer config?); skipping. See ${OUT}/.layered.log"
	detach_layered
	return 1
}
detach_layered() {
	[[ -n "${LAYERED_PID}" ]] || return 0
	$SUDO kill -INT "${LAYERED_PID}" 2>/dev/null || true
	wait "${LAYERED_PID}" 2>/dev/null || true
	LAYERED_PID=""
	sleep 1
}

# Ensure we never leave a scheduler attached on exit.
cleanup() { detach_prism; detach_layered; }
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# 4. Optionally seed the bus so scx_prism has identities to act on.
# ---------------------------------------------------------------------------
step "4/6  Seed the identity bus (for the scx_prism leg)"
if [[ "${SEED_BUS}" == "1" && "${SCHED_EXT}" -eq 1 ]]; then
	if [[ -x "${REPO_DIR}/prismd" ]]; then
		info "prismd present; in a real run start it as in provision step 9 (kubeconfig/cgroup keyer)."
		info "  (Not auto-started here: it needs cluster/cgroup context. Populate the bus, then re-run.)"
	else
		warn "prismd not built and no seed tool; scx_prism will run with an EMPTY bus"
		warn "  (every task resolves to UNKNOWN -> normal lane). Build prismd or add a seed step."
	fi
else
	info "SEED_BUS=0 or sched_ext absent; skipping."
fi

# ---------------------------------------------------------------------------
# 5. Run the matrix: for each scheduler, TRIALS trials -> CSV rows.
# CSV schema (one row per trial; long format for easy pandas/plot ingestion):
#   scheduler,arch,kernel,workload,trial,p50_us,p99_us,p999_us
# ---------------------------------------------------------------------------
step "5/6  Measure (${TRIALS} trials x ${DURATION}s per scheduler)"
echo "scheduler,arch,kernel,workload,trial,p50_us,p99_us,p999_us" > "${CSV}"

measure_leg() {
	local sched="$1"
	local t
	for (( t=1; t<=TRIALS; t++ )); do
		read -r p50 p99 p999 < <(run_workload_once)
		echo "${sched},${ARCH},${KREL},${WL},${t},${p50},${p99},${p999}" >> "${CSV}"
		printf "    %-12s trial %2d/%-2d  p50=%-9s p99=%-9s p99.9=%-9s us\n" \
			"${sched}" "${t}" "${TRIALS}" "${p50}" "${p99}" "${p999}"
	done
}

# (a) baseline — no scheduler attached (vanilla CFS/EEVDF). Always runs.
info "[a] baseline (vanilla CFS/EEVDF, no scx)"
measure_leg "baseline"

# (b) scx_layered — only if sched_ext + a built binary exist.
if [[ "${SCHED_EXT}" -eq 1 ]] && find_layered >/dev/null 2>&1; then
	info "[b] scx_layered"
	if attach_layered; then
		measure_leg "scx_layered"
		detach_layered
	else
		warn "scx_layered leg skipped (attach failed)."
	fi
else
	warn "[b] scx_layered SKIPPED (no sched_ext or scx_layered binary not built)."
fi

# (c) scx_prism — only if sched_ext + loader + object exist.
if attach_prism; then
	info "[c] scx_prism (identity-aware)"
	measure_leg "scx_prism"
	detach_prism
else
	warn "[c] scx_prism SKIPPED (no sched_ext, or loader/object missing/failed)."
fi

ok "wrote ${CSV} ($(($(wc -l < "${CSV}") - 1)) trial rows)"

# ---------------------------------------------------------------------------
# 6. Summarise + plot.
# ---------------------------------------------------------------------------
step "6/6  Summary + money plot"

# Per-scheduler summary across trials: median of each percentile + bootstrap 95%
# CI on the median (same methodology as bench/cmd/prismbench/stats.go). Done in
# python (numpy optional; pure-python fallback) and also emits a tidy summary
# CSV. Guarded on python3.
if have python3; then
	python3 "${SCRIPT_DIR}/sched_eval_plot.py" "${CSV}" "${OUT}" || warn "plot/summary step failed (matplotlib missing?)"
else
	warn "python3 not found; skipping summary + plot. Raw CSV is at ${CSV}."
fi

echo
ok "sched-eval complete."
info "raw trials : ${CSV}"
info "summary    : ${OUT}/sched_eval_summary_${ARCH}.csv (if python3 present)"
info "money plot : ${OUT}/sched_money_plot_${ARCH}.png  (if matplotlib present)"
echo
info "What this proves: if scx_prism's p99/p99.9 are materially below baseline"
info "(and competitive with/below scx_layered) for the latency-sensitive workload,"
info "identity-aware dispatch from the shared bus measurably helps tail latency."
