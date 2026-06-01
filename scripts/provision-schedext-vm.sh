#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# provision-schedext-vm.sh — take a fresh host from "stock distro" to
# "can compile AND load the Prism sched_ext scheduler (scx_prism)".
#
# Prism is a workload-identity bus: one pinned BPF hash map (prism_identity)
# that a net program, an observer, and a sched_ext scheduler all key off. The
# scheduler facet (bpf/scx_prism.bpf.c) is the only part that needs a special
# kernel: sched_ext (CONFIG_SCHED_CLASS_EXT) landed in Linux 6.12. This script
# provisions that kernel's userspace toolchain so the scheduler builds and the
# bus can be exercised end-to-end on a REAL kernel BPF map.
#
# Supported hosts (auto-detected):
#   - Ubuntu 25.04 "plucky"  (ships kernel 6.14, sched_ext enabled)  [apt]
#   - Fedora 41 / 42+        (6.11+/6.13+, sched_ext enabled)         [dnf]
# Both x86_64 and aarch64 (e.g. AWS Graviton) are handled — the only
# arch-specific bit is the clang -D__TARGET_ARCH_* define, derived below.
#
# What it does (each step is idempotent and loudly narrated):
#   1. detect distro + arch
#   2. install build deps (clang/llvm, libbpf-dev, bpftool, make, go>=1.22, git)
#   3. verify the running kernel actually has sched_ext
#   4. mount bpffs at /sys/fs/bpf if not already mounted
#   5. fetch + build the scx toolchain (sched-ext/scx) for its BPF headers
#      (<scx/common.bpf.h>) and the scx_loader/scheduler binaries
#   6. generate a 6.12+ vmlinux.h from the running kernel's BTF
#   7. compile bpf/compose_demo.bpf.o and bpf/scx_prism.bpf.o
#   8. build the prismd daemon
#   9. print exact next-steps to load the scheduler and run the bus
#
# It does NOT load the scheduler or start prismd for you — loading a system-wide
# scheduler is an explicit, privileged action you should run yourself. The exact
# commands are printed at the end.
#
# Run as root (or via sudo). Re-running is safe.

set -euo pipefail

# ---------------------------------------------------------------------------
# Pretty logging
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

# ---------------------------------------------------------------------------
# Locate the repo. This script lives in <repo>/scripts; the BPF sources are in
# <repo>/bpf. Allow override with PRISM_REPO for out-of-tree runs.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_DIR="${PRISM_REPO:-$(cd -- "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)}"
BPF_DIR="${REPO_DIR}/bpf"
# Where to clone/build the scx toolchain. Kept outside the repo by default.
SCX_DIR="${SCX_DIR:-/opt/scx}"
SCX_REF="${SCX_REF:-main}"   # pin to a tag/branch/commit for reproducibility

echo "${C_DIM}repo:    ${REPO_DIR}${C_RST}"
echo "${C_DIM}bpf:     ${BPF_DIR}${C_RST}"
echo "${C_DIM}scx:     ${SCX_DIR} (ref ${SCX_REF})${C_RST}"

[[ -d "${BPF_DIR}" ]] || die "expected BPF sources at ${BPF_DIR} (set PRISM_REPO=/path/to/prism)"

# ---------------------------------------------------------------------------
# 0. Privilege + sudo helper
# ---------------------------------------------------------------------------
if [[ "$(id -u)" -ne 0 ]]; then
	if command -v sudo >/dev/null 2>&1; then
		SUDO="sudo"
		warn "not root; using sudo for privileged steps (package install, mount, BTF read)"
	else
		die "must run as root (no sudo found). Re-run with: sudo $0"
	fi
else
	SUDO=""
fi

# ---------------------------------------------------------------------------
# 1. Detect distro + arch
# ---------------------------------------------------------------------------
step "1/9  Detecting distribution and architecture"
[[ -r /etc/os-release ]] || die "/etc/os-release missing; cannot detect distro"
# shellcheck disable=SC1091
. /etc/os-release
DISTRO_ID="${ID:-unknown}"
DISTRO_VER="${VERSION_ID:-unknown}"
info "distro: ${PRETTY_NAME:-${DISTRO_ID} ${DISTRO_VER}}"

PKG=""
case "${DISTRO_ID}" in
	ubuntu|debian)            PKG="apt" ;;
	fedora|rhel|centos|rocky|almalinux) PKG="dnf" ;;
	*) die "unsupported distro '${DISTRO_ID}'. This script targets Ubuntu 25.04+ (apt) or Fedora 41+ (dnf)." ;;
esac
ok "package manager: ${PKG}"

ARCH="$(uname -m)"
case "${ARCH}" in
	x86_64|amd64)  BPF_ARCH_DEF="-D__TARGET_ARCH_x86";   GOARCH="amd64" ;;
	aarch64|arm64) BPF_ARCH_DEF="-D__TARGET_ARCH_arm64"; GOARCH="arm64" ;;
	*) die "unsupported arch '${ARCH}' (need x86_64 or aarch64)" ;;
esac
ok "arch: ${ARCH} -> clang ${BPF_ARCH_DEF}, GOARCH=${GOARCH}"

KREL="$(uname -r)"
info "running kernel: ${KREL}"

# ---------------------------------------------------------------------------
# 2. Install build dependencies
# ---------------------------------------------------------------------------
step "2/9  Installing build dependencies"
# We need: a C/BPF compiler (clang/llvm + llvm tools for readelf/strip),
# libbpf headers, bpftool (for BTF dump + struct_ops loading), make/git, and a
# Go toolchain >= 1.22 to build prismd.
install_apt() {
	export DEBIAN_FRONTEND=noninteractive
	$SUDO apt-get update -y
	# linux-tools-common + linux-tools-$(uname -r) provide bpftool matching the
	# running kernel (the generic 'bpftool' package also works on recent Ubuntu).
	$SUDO apt-get install -y --no-install-recommends \
		clang llvm lld libelf-dev zlib1g-dev pkg-config \
		libbpf-dev \
		make gcc git ca-certificates curl \
		linux-tools-common "linux-tools-${KREL}" \
		golang-go meson ninja-build cargo rustc || {
			warn "full install failed; retrying without the kernel-specific linux-tools package"
			$SUDO apt-get install -y --no-install-recommends \
				clang llvm lld libelf-dev zlib1g-dev pkg-config libbpf-dev \
				make gcc git ca-certificates curl linux-tools-common bpftool \
				golang-go meson ninja-build cargo rustc
		}
}
install_dnf() {
	$SUDO dnf install -y \
		clang llvm lld llvm-tools elfutils-libelf-devel zlib-devel pkgconf-pkg-config \
		libbpf libbpf-devel bpftool \
		make gcc git ca-certificates curl \
		golang meson ninja-build cargo rust
}
case "${PKG}" in
	apt) install_apt ;;
	dnf) install_dnf ;;
esac

# Sanity-check the tools we actually invoke.
command -v clang   >/dev/null 2>&1 || die "clang not on PATH after install"
command -v make    >/dev/null 2>&1 || die "make not on PATH after install"
command -v git     >/dev/null 2>&1 || die "git not on PATH after install"
ok "clang: $(clang --version | head -1)"

# bpftool: the apt wrapper sometimes shells out to linux-tools-$(uname -r). Find
# a working binary; prefer one on PATH, else the versioned linux-tools path.
BPFTOOL=""
if command -v bpftool >/dev/null 2>&1 && bpftool version >/dev/null 2>&1; then
	BPFTOOL="bpftool"
else
	for cand in "/usr/lib/linux-tools-${KREL}/bpftool" /usr/lib/linux-tools-*/bpftool /usr/sbin/bpftool; do
		if [[ -x "${cand}" ]] && "${cand}" version >/dev/null 2>&1; then BPFTOOL="${cand}"; break; fi
	done
fi
[[ -n "${BPFTOOL}" ]] || die "no working bpftool found (needed for vmlinux.h + struct_ops loading)"
ok "bpftool: ${BPFTOOL} ($(${BPFTOOL} version 2>/dev/null | head -1))"

# Go >= 1.22.
if command -v go >/dev/null 2>&1; then
	GOVER="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
	info "go: ${GOVER}"
	# crude major.minor check: 1.22+ required
	GOMAJ="${GOVER%%.*}"; GOREST="${GOVER#*.}"; GOMIN="${GOREST%%.*}"
	if [[ "${GOMAJ:-0}" -lt 1 || ( "${GOMAJ}" -eq 1 && "${GOMIN:-0}" -lt 22 ) ]]; then
		warn "go ${GOVER} is < 1.22; prismd's go.mod requires 1.22+. Install a newer Go (e.g. from go.dev/dl) and re-run, or set GOTOOLCHAIN=auto to let Go fetch it."
	else
		ok "go ${GOVER} satisfies >= 1.22"
	fi
else
	warn "go not found; prismd build (step 8) will be skipped. Install Go >= 1.22 from your distro or go.dev/dl."
fi

# ---------------------------------------------------------------------------
# 3. Verify the kernel has sched_ext
# ---------------------------------------------------------------------------
step "3/9  Verifying the kernel supports sched_ext (CONFIG_SCHED_CLASS_EXT)"
SCHED_EXT_OK=1

# (a) the runtime sysfs surface that the scx loader uses to attach.
if [[ -d /sys/kernel/sched_ext ]]; then
	ok "/sys/kernel/sched_ext present (runtime sched_ext support is live)"
	if [[ -r /sys/kernel/sched_ext/state ]]; then
		info "current sched_ext state: $(cat /sys/kernel/sched_ext/state 2>/dev/null || echo '?')"
	fi
else
	warn "/sys/kernel/sched_ext is MISSING — this kernel was not built with sched_ext, or it is not loaded"
	SCHED_EXT_OK=0
fi

# (b) the build-time config, from whichever config source exists.
CONFIG_FOUND=""
if [[ -r "/boot/config-${KREL}" ]]; then
	CONFIG_FOUND="/boot/config-${KREL}"
	if grep -q '^CONFIG_SCHED_CLASS_EXT=y' "${CONFIG_FOUND}"; then
		ok "CONFIG_SCHED_CLASS_EXT=y in ${CONFIG_FOUND}"
	else
		warn "CONFIG_SCHED_CLASS_EXT not =y in ${CONFIG_FOUND}"; SCHED_EXT_OK=0
	fi
elif [[ -r /proc/config.gz ]] && command -v zgrep >/dev/null 2>&1; then
	CONFIG_FOUND="/proc/config.gz"
	if zgrep -q '^CONFIG_SCHED_CLASS_EXT=y' /proc/config.gz; then
		ok "CONFIG_SCHED_CLASS_EXT=y in /proc/config.gz"
	else
		warn "CONFIG_SCHED_CLASS_EXT not =y in /proc/config.gz"; SCHED_EXT_OK=0
	fi
else
	warn "no kernel config found (/boot/config-${KREL} or /proc/config.gz); relying on the /sys/kernel/sched_ext check above"
fi

# Kernel version floor: sched_ext is 6.12+. Warn (don't hard-fail) if older,
# since some distros backport.
KMAJ="${KREL%%.*}"; KREST="${KREL#*.}"; KMIN="${KREST%%.*}"; KMIN="${KMIN%%-*}"
if [[ "${KMAJ:-0}" -lt 6 || ( "${KMAJ}" -eq 6 && "${KMIN:-0}" -lt 12 ) ]]; then
	warn "kernel ${KREL} is < 6.12; mainline sched_ext requires 6.12+. If your distro backported it and /sys/kernel/sched_ext exists, you may still be fine."
fi

if [[ "${SCHED_EXT_OK}" -ne 1 ]]; then
	warn "sched_ext does not appear available on the RUNNING kernel."
	warn "You can still compile scx_prism.bpf.o below (it only needs the BTF + scx headers), but LOADING it will fail until you boot a 6.12+ sched_ext kernel (e.g. Ubuntu 25.04, Fedora 41+, or a custom build)."
fi

# ---------------------------------------------------------------------------
# 4. Mount bpffs
# ---------------------------------------------------------------------------
step "4/9  Ensuring bpffs is mounted at /sys/fs/bpf"
# The Prism map is pinned under /sys/fs/bpf/prism/ (abi.PinPath). The scheduler
# and prismd both need bpffs mounted to share the pinned map across processes.
if mountpoint -q /sys/fs/bpf 2>/dev/null || grep -qE '\s/sys/fs/bpf\s+bpf\s' /proc/mounts; then
	ok "bpffs already mounted at /sys/fs/bpf"
else
	info "mounting bpffs: mount -t bpf bpf /sys/fs/bpf"
	$SUDO mkdir -p /sys/fs/bpf
	$SUDO mount -t bpf bpf /sys/fs/bpf
	ok "bpffs mounted"
	info "to make this persistent across reboots, add to /etc/fstab:"
	echo "        ${C_DIM}bpffs  /sys/fs/bpf  bpf  defaults  0 0${C_RST}"
fi
# Prism's own pin directory (cilium/ebpf MkdirAll's this too, but pre-create so
# the layout is obvious).
$SUDO mkdir -p /sys/fs/bpf/prism
ok "Prism pin dir ready: /sys/fs/bpf/prism (map will pin as /sys/fs/bpf/prism/prism_identity)"

# ---------------------------------------------------------------------------
# 5. Fetch + build the scx toolchain (headers + loader)
# ---------------------------------------------------------------------------
step "5/9  Fetching + building the scx toolchain (sched-ext/scx)"
# bpf/scx_prism.bpf.c does #include <scx/common.bpf.h>, which ships in the scx
# repo (and the kernel's tools/sched_ext), NOT in libbpf-dev. We clone scx to
# get those headers AND the userspace scheduler binaries (scx_loader, scx_rusty,
# scx_simple, ...) that demonstrate loading a sched_ext scheduler.
if [[ -d "${SCX_DIR}/.git" ]]; then
	info "scx already cloned at ${SCX_DIR}; fetching ${SCX_REF}"
	$SUDO git -C "${SCX_DIR}" fetch --depth 1 origin "${SCX_REF}" || warn "git fetch failed (offline?); using existing checkout"
	$SUDO git -C "${SCX_DIR}" checkout -q "${SCX_REF}" 2>/dev/null || true
else
	info "cloning sched-ext/scx into ${SCX_DIR} (ref ${SCX_REF})"
	$SUDO mkdir -p "$(dirname "${SCX_DIR}")"
	$SUDO git clone --depth 1 --branch "${SCX_REF}" https://github.com/sched-ext/scx "${SCX_DIR}" \
		|| $SUDO git clone --depth 1 https://github.com/sched-ext/scx "${SCX_DIR}"
fi

# The scx BPF headers we need on clang's include path. Upstream layout puts the
# C-side scheduler headers (common.bpf.h, etc.) under scheds/include. Locate the
# directory that actually contains scx/common.bpf.h so we are robust to layout
# changes across scx versions.
SCX_INC=""
for cand in \
	"${SCX_DIR}/scheds/include" \
	"${SCX_DIR}/scheds/c/include" \
	"${SCX_DIR}/include" ; do
	if [[ -f "${cand}/scx/common.bpf.h" ]]; then SCX_INC="${cand}"; break; fi
done
if [[ -z "${SCX_INC}" ]]; then
	# last resort: find it anywhere in the tree
	found="$(find "${SCX_DIR}" -type f -name common.bpf.h -path '*/scx/*' 2>/dev/null | head -1 || true)"
	if [[ -n "${found}" ]]; then SCX_INC="$(dirname "$(dirname "${found}")")"; fi
fi
[[ -n "${SCX_INC}" ]] || die "could not locate scx/common.bpf.h under ${SCX_DIR}; the scx repo layout may have changed"
ok "scx BPF headers: ${SCX_INC} (provides <scx/common.bpf.h>)"

# Build the scx userspace schedulers + loader (meson). This gives you scx_loader
# and reference schedulers; it is optional for compiling scx_prism but required
# to LOAD a scheduler via the standard loader. Best-effort: a header-only goal
# still succeeds even if the full build is skipped.
if command -v meson >/dev/null 2>&1 && command -v ninja >/dev/null 2>&1; then
	if [[ ! -d "${SCX_DIR}/build" ]]; then
		info "configuring scx build (meson)"
		( cd "${SCX_DIR}" && $SUDO meson setup build 2>&1 | tail -5 ) || warn "meson setup failed; continuing with headers only"
	fi
	if [[ -d "${SCX_DIR}/build" ]]; then
		info "building scx schedulers (ninja) — this can take a few minutes"
		( cd "${SCX_DIR}" && $SUDO meson compile -C build 2>&1 | tail -10 ) || warn "scx build failed; you still have the headers to compile scx_prism, but no prebuilt scx_loader"
	fi
else
	warn "meson/ninja not available; skipping scx userspace build. You have the scx headers (enough to compile scx_prism), but you'll need your own loader (see step 9)."
fi

# ---------------------------------------------------------------------------
# 6. Generate a 6.12+ vmlinux.h
# ---------------------------------------------------------------------------
step "6/9  Generating vmlinux.h from the running kernel's BTF"
# scx_prism.bpf.c needs the sched_ext TYPES (struct sched_ext_ops,
# struct scx_init_task_args, SCX_SLICE_DFL, ...). Those only exist in a 6.12+
# kernel's BTF. We dump THIS kernel's BTF; on a 6.12+ host it contains them.
VMLINUX_H="${BPF_DIR}/vmlinux.h"
if [[ ! -r /sys/kernel/btf/vmlinux ]]; then
	die "/sys/kernel/btf/vmlinux missing — kernel was built without CONFIG_DEBUG_INFO_BTF. Cannot generate vmlinux.h."
fi
# Back up any vendored vmlinux.h (the repo ships a 5.15 dump that lacks scx types).
if [[ -f "${VMLINUX_H}" ]] && ! grep -q 'sched_ext_ops' "${VMLINUX_H}" 2>/dev/null; then
	info "existing ${VMLINUX_H} has no sched_ext types (likely the repo's 5.15 dump); backing it up to vmlinux.h.bak"
	cp -f "${VMLINUX_H}" "${VMLINUX_H}.bak" || true
fi
info "dumping BTF: ${BPFTOOL} btf dump file /sys/kernel/btf/vmlinux format c"
$SUDO "${BPFTOOL}" btf dump file /sys/kernel/btf/vmlinux format c > "${VMLINUX_H}.tmp"
# Verify the dump actually contains sched_ext types before overwriting.
if grep -q 'struct sched_ext_ops' "${VMLINUX_H}.tmp"; then
	mv -f "${VMLINUX_H}.tmp" "${VMLINUX_H}"
	ok "vmlinux.h regenerated with sched_ext types ($(wc -l < "${VMLINUX_H}") lines)"
else
	rm -f "${VMLINUX_H}.tmp"
	warn "the dumped BTF does NOT contain 'struct sched_ext_ops' — this kernel lacks sched_ext."
	warn "Keeping the existing ${VMLINUX_H}. scx_prism will NOT compile until you run this on a 6.12+ sched_ext kernel."
fi

# ---------------------------------------------------------------------------
# 7. Compile the BPF objects
# ---------------------------------------------------------------------------
step "7/9  Compiling the Prism BPF objects"

# 7a. compose_demo: generic program types, builds on any modern kernel. This is
# the net + observe facet of the bus and a good smoke test of the toolchain.
info "compiling compose_demo.bpf.o (net + observe facets)"
clang -O2 -g -target bpf ${BPF_ARCH_DEF} \
	-I"${BPF_DIR}" \
	-c "${BPF_DIR}/compose_demo.bpf.c" -o "${BPF_DIR}/compose_demo.bpf.o"
ok "built ${BPF_DIR}/compose_demo.bpf.o"

# 7b. scx_prism: the sched facet. Needs the scx headers + 6.12 BTF.
info "compiling scx_prism.bpf.o (sched facet — needs scx headers + 6.12 BTF)"
if grep -q 'struct sched_ext_ops' "${VMLINUX_H}" 2>/dev/null; then
	clang -O2 -g -target bpf ${BPF_ARCH_DEF} \
		-I"${BPF_DIR}" -I"${SCX_INC}" \
		-c "${BPF_DIR}/scx_prism.bpf.c" -o "${BPF_DIR}/scx_prism.bpf.o"
	ok "built ${BPF_DIR}/scx_prism.bpf.o"
	# Show the struct_ops section, which is what the loader attaches.
	if command -v llvm-readelf >/dev/null 2>&1; then
		info "sections: $(llvm-readelf -S "${BPF_DIR}/scx_prism.bpf.o" | grep -oE 'struct_ops|\.maps|\.BTF' | sort -u | tr '\n' ' ')"
	elif command -v readelf >/dev/null 2>&1; then
		info "sections: $(readelf -S "${BPF_DIR}/scx_prism.bpf.o" | grep -oE 'struct_ops|\.maps|\.BTF' | sort -u | tr '\n' ' ')"
	fi
else
	warn "skipping scx_prism.bpf.o: vmlinux.h lacks sched_ext types (not a 6.12+ kernel). See step 6."
fi

# ---------------------------------------------------------------------------
# 8. Build prismd
# ---------------------------------------------------------------------------
step "8/9  Building the prismd daemon"
if command -v go >/dev/null 2>&1; then
	info "go build (static, CGO disabled): cmd/prismd"
	( cd "${REPO_DIR}" && CGO_ENABLED=0 GOFLAGS=-mod=mod GOARCH="${GOARCH}" \
		go build -o "${REPO_DIR}/prismd" ./cmd/prismd )
	ok "built ${REPO_DIR}/prismd"
else
	warn "go missing; skipped prismd build. Install Go >= 1.22 and run: (cd ${REPO_DIR} && CGO_ENABLED=0 GOFLAGS=-mod=mod go build ./cmd/prismd)"
fi

# ---------------------------------------------------------------------------
# 9. Next steps
# ---------------------------------------------------------------------------
step "9/9  Done. Next steps to make the bus REAL on this kernel"
cat <<EOF

  Artifacts produced:
    ${BPF_DIR}/compose_demo.bpf.o   (net + observe facets)
    ${BPF_DIR}/scx_prism.bpf.o      (sched facet — present iff this is a 6.12+ kernel)
    ${REPO_DIR}/prismd              (the identity-sync daemon, iff Go was available)
    ${BPF_DIR}/vmlinux.h            (regenerated from this kernel's BTF)
    ${SCX_INC}                      (scx BPF headers)

  The bus is the ONE pinned map: /sys/fs/bpf/prism/prism_identity
  (abi.MapName=prism_identity, abi.PinPath=/sys/fs/bpf/prism/identity).

  ---------------------------------------------------------------------------
  A) Create the shared map + start the identity producer (prismd).
     prismd creates and pins the map, then fills it from Pod events. Run it as
     root (CAP_BPF/CAP_SYS_ADMIN to create+pin a BPF map) with the cgroup keyer
     so the key == the pod cgroup inode (real-kernel parity with the scheduler):

       sudo NODE_NAME=\$(hostname) ${REPO_DIR}/prismd \\
            -bpf=true -keyer=cgroup \\
            -cgroup-root=/sys/fs/cgroup -cgroup-driver=systemd \\
            -kubeconfig=/root/.kube/config        # or in-cluster

     Confirm the map exists:
       sudo ${BPFTOOL} map show name prism_identity
       sudo ls -l /sys/fs/bpf/prism/

  B) Load the identity-aware scheduler (scx_prism). It opens the SAME pinned
     map and schedules tasks by their Prism identity. struct_ops/sched_ext
     schedulers are loaded by a userspace program that attaches the ops struct.

     Option 1 — scx_loader (built in step 5, the standard scx way). Drop
     scx_prism.bpf.o where scx_loader looks for schedulers (the loader is a
     D-Bus service; see ${SCX_DIR}/README.md for the exact unit + config), then:
       sudo systemctl start scx_loader        # if installed as a service
       # or run a built reference loader directly from ${SCX_DIR}/build

     Option 2 — a minimal libbpf loader (most direct; what we recommend for the
     demo). bpftool can load+attach a struct_ops object and pin it:
       sudo ${BPFTOOL} struct_ops register ${BPF_DIR}/scx_prism.bpf.o \\
            /sys/fs/bpf/prism/scx_prism_ops
     While attached, the kernel reports it:
       cat /sys/kernel/sched_ext/state        # -> "enabled"
       cat /sys/kernel/sched_ext/root/ops 2>/dev/null   # -> "prism"
     To stop the scheduler, remove the pinned struct_ops link:
       sudo rm /sys/fs/bpf/prism/scx_prism_ops
     (sched_ext also auto-detaches if the loader dies — the kernel watchdog
      reverts to the default scheduler, so a crash can never wedge the box.)

  C) Verify the bus is shared: with prismd running AND scx_prism attached, the
     scheduler stamps PRISM_FLAG_SCHED_MANAGED on identities it manages. Dump
     the map and watch the flags change as pods are scheduled:
       sudo ${BPFTOOL} map dump name prism_identity

  Cross-ISA note: this script handled ${ARCH}. The only arch-specific build flag
  is clang ${BPF_ARCH_DEF}; everything else (scx headers, BTF, loader) is arch
  independent, so the same flow proves the bus on both x86_64 and aarch64
  (e.g. AWS Graviton) — that is the cross-ISA evaluation.

  See scripts/README.md for the full zero-to-real runbook and what each tier proves.

EOF
ok "provisioning complete"
