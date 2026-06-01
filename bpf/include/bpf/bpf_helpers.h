/* SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause) */
/*
 * MINIMAL LOCAL SHIM — NOT the real libbpf <bpf/bpf_helpers.h>.
 *
 * Purpose: let clang type-check the Prism BPF C on a dev host that has clang +
 * a generated vmlinux.h but NO libbpf-dev installed and NO network to fetch it
 * (our 5.15 WSL2 box). On any real build host the system libbpf header is found
 * first on the include path and shadows this file completely.
 *
 * It provides only the macros/attributes the Prism programs use:
 *   SEC(), __uint(), __type(), __array(), and the map-flag constants.
 * Semantics intentionally mirror upstream libbpf so the .c files are
 * byte-for-byte the same whether built here or on a 6.12 host.
 */
#ifndef __PRISM_SHIM_BPF_HELPERS_H__
#define __PRISM_SHIM_BPF_HELPERS_H__

#include "bpf_helper_defs.h"

/* Place a symbol in a named ELF section (program type, license, maps, ...). */
#ifndef SEC
#define SEC(name) __attribute__((section(name), used))
#endif

/* libbpf map-definition sugar: BTF-encoded fields inside an anonymous struct. */
#ifndef __uint
#define __uint(name, val) int(*name)[val]
#endif
#ifndef __type
#define __type(name, val) typeof(val) *name
#endif
#ifndef __array
#define __array(name, val) typeof(val) *name[]
#endif

/* Mark functions that must be inlined into the caller (helper libraries). */
#ifndef __always_inline
#define __always_inline inline __attribute__((always_inline))
#endif

/* Map update flags (also present in vmlinux.h as an enum, but libbpf programs
 * conventionally rely on these macros being available). */
#ifndef BPF_ANY
#define BPF_ANY     0 /* create new or overwrite existing */
#define BPF_NOEXIST 1 /* create new only; fail if it exists */
#define BPF_EXIST   2 /* overwrite existing only; fail if absent */
#endif

/* Map pinning modes (libbpf). LIBBPF_PIN_BY_NAME pins the map at
 * /sys/fs/bpf/<obj>/<map-name> so several programs/objects share one map. */
#ifndef LIBBPF_PIN_NONE
#define LIBBPF_PIN_NONE    0
#define LIBBPF_PIN_BY_NAME 1
#endif

/* bpf_printk: tiny debug print. Real libbpf has a fancier variadic version;
 * this trimmed form is enough for the Prism demos' optional debug lines. */
#ifndef bpf_printk
#define bpf_printk(fmt, ...)                                   \
	({                                                     \
		static const char ____fmt[] = fmt;             \
		bpf_trace_printk(____fmt, sizeof(____fmt),      \
				 ##__VA_ARGS__);               \
	})
#endif

#endif /* __PRISM_SHIM_BPF_HELPERS_H__ */
