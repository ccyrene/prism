// SPDX-License-Identifier: GPL-2.0
//
// scx_prism_loader.c — a minimal libbpf userspace loader that LOADS and ATTACHES
// bpf/scx_prism.bpf.c as a sched_ext (scx) struct_ops scheduler on Linux 6.12+.
//
// ---------------------------------------------------------------------------
// WHAT THIS IS
// ---------------------------------------------------------------------------
// scx_prism.bpf.c is the SCHED facet of the Prism identity bus: a sched_ext BPF
// scheduler that READS each task's Prism workload identity from the shared
// `prism_identity` map and picks a dispatch queue + time slice from it. A
// sched_ext scheduler is loaded into the kernel as a `struct_ops` map of type
// BPF_MAP_TYPE_STRUCT_OPS whose value is a `struct sched_ext_ops`; ATTACHING
// that struct_ops map is what tells the kernel "install this scheduler". The
// kernel then routes scheduling decisions to our BPF callbacks until the
// struct_ops link is destroyed (or the kernel's sched_ext watchdog reverts to
// the builtin scheduler if we die — a stuck scx scheduler can never wedge the
// box).
//
// This loader does exactly the canonical libbpf lifecycle:
//     open  -> load -> attach struct_ops -> poll until SIGINT -> detach (auto)
//
// ---------------------------------------------------------------------------
// SKELETON vs GENERIC OBJECT API
// ---------------------------------------------------------------------------
// There are two libbpf ways to do this; this file supports BOTH and picks at
// compile time so it works whether or not you have generated a skeleton:
//
//   (1) SKELETON API (preferred; what upstream scx schedulers use).
//       `bpftool gen skeleton scx_prism.bpf.o > scx_prism.skel.h` produces a
//       typed header with scx_prism__open() / __load() / and per-struct_ops
//       attach. The skeleton exposes our ops as `skel->maps.prism_ops` (the
//       SCX_OPS_DEFINE'd symbol name from scx_prism.bpf.c) and we attach it with
//       bpf_map__attach_struct_ops(). Build with -DUSE_SKEL and put the skeleton
//       on the include path. The struct_ops map name MUST match the
//       SCX_OPS_DEFINE() name in the BPF source: `prism_ops`.
//
//   (2) GENERIC OBJECT API (no skeleton needed; one fewer build step).
//       bpf_object__open_file("scx_prism.bpf.o") -> bpf_object__load() ->
//       find the struct_ops map by name ("prism_ops") with
//       bpf_object__find_map_by_name() -> bpf_map__attach_struct_ops().
//       This is the default build (no -DUSE_SKEL) and needs only the .bpf.o.
//
// Both paths converge on bpf_map__attach_struct_ops(), which returns a
// `struct bpf_link *` representing the live scheduler. Destroying that link
// detaches the scheduler.
//
// ---------------------------------------------------------------------------
// BUILD (only works on a 6.12+ host with libbpf-dev >= 1.0 and clang) — see
// loader/Makefile and loader/README.md for the full story. The exact lines:
//
//   # 0. provision a 6.12+ box and compile the BPF object + regenerate vmlinux.h:
//   sudo scripts/provision-schedext-vm.sh        # builds bpf/scx_prism.bpf.o
//
//   # (1) skeleton build:
//   bpftool gen skeleton bpf/scx_prism.bpf.o name scx_prism > loader/scx_prism.skel.h
//   clang -O2 -g -Wall -DUSE_SKEL -I loader -I /usr/include \
//         loader/scx_prism_loader.c -lbpf -lelf -lz -o loader/scx_prism_loader
//
//   # (2) generic (no skeleton) build:
//   clang -O2 -g -Wall \
//         loader/scx_prism_loader.c -lbpf -lelf -lz -o loader/scx_prism_loader
//
//   # run (root; loads a SYSTEM-WIDE scheduler):
//   sudo ./loader/scx_prism_loader bpf/scx_prism.bpf.o
//
// ---------------------------------------------------------------------------
// WHY THIS DOES NOT BUILD ON THE 5.15 DEV BOX
// ---------------------------------------------------------------------------
//   * <bpf/libbpf.h> / <bpf/bpf.h> come from libbpf-dev, which is not installed
//     here (the BPF *program* only needs the headers vendored under bpf/include;
//     this USERSPACE loader needs the full libbpf userspace library + headers).
//   * The 6.12 sched_ext kfuncs/types only exist in a 6.12 kernel's BTF; even
//     with libbpf-dev, ATTACH would fail to load on 5.15 (no
//     CONFIG_SCHED_CLASS_EXT). So this is COMPILE-only-on-6.12 by design.
//   * clang -fsyntax-only here fails at `#include <bpf/libbpf.h>` — that is the
//     EXPECTED, documented dependency, not a code defect (README records the
//     exact error string).
// ---------------------------------------------------------------------------

#include <bpf/libbpf.h> // libbpf userspace API (open/load/attach); from libbpf-dev
#include <bpf/bpf.h>    // low-level bpf() syscall wrappers; from libbpf-dev

#include <errno.h>
#include <signal.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h> // struct timespec + nanosleep() for the poll loop
#include <unistd.h>

// The struct_ops map name. This MUST equal the SCX_OPS_DEFINE() name in
// bpf/scx_prism.bpf.c: `SCX_OPS_DEFINE(prism_ops, ...)`. Both API paths look the
// ops map up by this name.
#define PRISM_OPS_MAP_NAME "prism_ops"

#ifdef USE_SKEL
// Generated with: bpftool gen skeleton bpf/scx_prism.bpf.o name scx_prism
// > loader/scx_prism.skel.h  (only present after you run that on a 6.12 host).
#include "scx_prism.skel.h"
#endif

// ---------------------------------------------------------------------------
// SIGINT/SIGTERM -> graceful detach. We just flip a flag; the poll loop notices
// and falls through to link destroy. (Even if we were killed -9, the kernel's
// sched_ext watchdog auto-reverts to the default scheduler — documented in
// scripts/provision-schedext-vm.sh.)
// ---------------------------------------------------------------------------
static volatile sig_atomic_t exiting;
static void on_signal(int sig)
{
	(void)sig;
	exiting = 1;
}

// Route libbpf's internal logging to stderr. Verbose by default because the
// interesting failures (verifier rejects, missing BTF, attach EOPNOTSUPP on a
// non-sched_ext kernel) all surface here.
static int libbpf_print(enum libbpf_print_level level, const char *fmt,
			va_list args)
{
	if (level == LIBBPF_DEBUG)
		return 0; // drop DEBUG spam; keep INFO/WARN
	return vfprintf(stderr, fmt, args);
}

int main(int argc, char **argv)
{
	// On a real 6.12 host the only argument is the path to the compiled BPF
	// object (default bpf/scx_prism.bpf.o). The skeleton build embeds the object
	// and ignores this path (marked (void) below so -Wunused-variable is clean
	// in that mode).
	const char *obj_path =
		(argc > 1) ? argv[1] : "bpf/scx_prism.bpf.o";

	struct bpf_link *link = NULL;
	int err = 0;

	libbpf_set_print(libbpf_print);

	// Install signal handlers BEFORE we attach, so a Ctrl-C during/after attach
	// always lands in our clean detach path.
	struct sigaction sa = { .sa_handler = on_signal };
	sigemptyset(&sa.sa_mask);
	sigaction(SIGINT, &sa, NULL);
	sigaction(SIGTERM, &sa, NULL);

#ifdef USE_SKEL
	// -------------------------------------------------------------------
	// PATH 1: SKELETON API.
	// -------------------------------------------------------------------
	fprintf(stderr, "scx_prism_loader: using skeleton API\n");
	(void)obj_path; // object is embedded in the skeleton; path arg is unused here

	// open(): parse the embedded object, create (but do not yet load) maps and
	// programs. Returns a typed skeleton handle.
	struct scx_prism *skel = scx_prism__open();
	if (!skel) {
		fprintf(stderr, "ERROR: scx_prism__open() failed: %s\n",
			strerror(errno));
		return 1;
	}

	// load(): create maps in the kernel, load+verify programs. This is where the
	// verifier runs; a RDONLY_PROG violation or a bad scx callback would fail
	// HERE with a verifier log on stderr.
	err = scx_prism__load(skel);
	if (err) {
		fprintf(stderr, "ERROR: scx_prism__load() failed: %d (%s)\n",
			err, strerror(-err));
		fprintf(stderr,
			"  Hint: on a non-6.12 kernel struct_ops load fails (no "
			"CONFIG_SCHED_CLASS_EXT). See loader/README.md.\n");
		goto cleanup_skel;
	}

	// attach the struct_ops: install the scheduler. The skeleton exposes the
	// SCX_OPS_DEFINE'd ops as a map; attach it to get the live link. (Older
	// skeletons auto-attach struct_ops in scx_prism__attach(); we attach the
	// specific map explicitly so the intent — "this map IS the scheduler" — is
	// obvious and so it works across libbpf versions.)
	link = bpf_map__attach_struct_ops(skel->maps.prism_ops);
	if (!link) {
		err = -errno;
		fprintf(stderr,
			"ERROR: bpf_map__attach_struct_ops(prism_ops) failed: %s\n",
			strerror(errno));
		goto cleanup_skel;
	}
#else
	// -------------------------------------------------------------------
	// PATH 2: GENERIC OBJECT API (no skeleton; only needs the .bpf.o).
	// -------------------------------------------------------------------
	fprintf(stderr,
		"scx_prism_loader: using generic object API, object=%s\n",
		obj_path);

	// open_file(): parse the ELF object from disk.
	struct bpf_object *obj = bpf_object__open_file(obj_path, NULL);
	if (!obj || libbpf_get_error(obj)) {
		fprintf(stderr, "ERROR: bpf_object__open_file(%s) failed: %s\n",
			obj_path, strerror(errno));
		return 1;
	}

	// load(): create maps + load/verify programs in the kernel.
	err = bpf_object__load(obj);
	if (err) {
		fprintf(stderr, "ERROR: bpf_object__load() failed: %d (%s)\n",
			err, strerror(-err));
		fprintf(stderr,
			"  Hint: on a non-6.12 kernel struct_ops load fails (no "
			"CONFIG_SCHED_CLASS_EXT). See loader/README.md.\n");
		goto cleanup_obj;
	}

	// Find the struct_ops map by the SCX_OPS_DEFINE() name and attach it. This
	// is the exact operation the skeleton path does, just spelled out.
	struct bpf_map *ops_map =
		bpf_object__find_map_by_name(obj, PRISM_OPS_MAP_NAME);
	if (!ops_map) {
		fprintf(stderr,
			"ERROR: struct_ops map '%s' not found in %s. The name must "
			"match SCX_OPS_DEFINE() in scx_prism.bpf.c.\n",
			PRISM_OPS_MAP_NAME, obj_path);
		err = -ENOENT;
		goto cleanup_obj;
	}

	link = bpf_map__attach_struct_ops(ops_map);
	if (!link) {
		err = -errno;
		fprintf(stderr,
			"ERROR: bpf_map__attach_struct_ops(%s) failed: %s\n",
			PRISM_OPS_MAP_NAME, strerror(errno));
		goto cleanup_obj;
	}
#endif

	// -------------------------------------------------------------------
	// ATTACHED. The Prism identity-aware scheduler is now live system-wide.
	// -------------------------------------------------------------------
	fprintf(stderr,
		"scx_prism_loader: scheduler ATTACHED — Prism is now scheduling.\n"
		"  Verify:   cat /sys/kernel/sched_ext/state       (-> 'enabled')\n"
		"            cat /sys/kernel/sched_ext/root/ops     (-> 'prism')\n"
		"  Identity: sudo bpftool map dump name prism_identity\n"
		"  Stop:     Ctrl-C (or SIGTERM) — reverts to the default scheduler.\n");

	// Poll until a signal. We have no userspace ring buffer to service here (the
	// scheduler is fully in-kernel), so we just sleep in 1s ticks and watch the
	// exit flag. A real production loader might also poll ops.exit info via a
	// shared map; we keep it minimal.
	while (!exiting) {
		// If the scheduler self-aborted in the kernel (ops.exit fired, e.g. a
		// runtime error), libbpf marks the link autodetached; the kernel has
		// already reverted to the default scheduler. We just keep ticking until
		// the operator signals us. (pause()/nanosleep keeps us off-CPU.)
		struct timespec tick = { .tv_sec = 1, .tv_nsec = 0 };
		nanosleep(&tick, NULL);
	}

	fprintf(stderr, "\nscx_prism_loader: signal received, detaching...\n");

	// -------------------------------------------------------------------
	// DETACH. Destroying the struct_ops link uninstalls the scheduler; the
	// kernel reverts to the default (CFS/EEVDF). Maps + programs are then freed
	// by the object/skeleton destroy below.
	// -------------------------------------------------------------------
	bpf_link__destroy(link);
	link = NULL;
	fprintf(stderr,
		"scx_prism_loader: detached — default scheduler restored.\n");

#ifdef USE_SKEL
cleanup_skel:
	if (link)
		bpf_link__destroy(link);
	scx_prism__destroy(skel);
	return err ? 1 : 0;
#else
cleanup_obj:
	if (link)
		bpf_link__destroy(link);
	bpf_object__close(obj);
	return err ? 1 : 0;
#endif
}
