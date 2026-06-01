// SPDX-License-Identifier: Apache-2.0

// microbench.c — standalone userspace microbenchmark contrasting the two
// per-scheduling-decision identity-resolution strategies in Prism's evaluation.
//
// THIS IS NOT A BPF PROGRAM. It is an ordinary C program (clang -O2) that runs
// on any host (incl. this 5.15 WSL2 box where BPF load needs root and there is
// no sched_ext). It exists to measure, in isolation, the cost of the two hot
// paths so the headline ratio can be reported honestly:
//
//   (1) PRISM path    — a single lookup in a u64 -> struct{u32;u32;u64;u64}
//                       hash table. This emulates what the Prism-aware scheduler
//                       does per decision: read the precomputed identity for the
//                       task's cgroup id from the (pinned) BPF hash map. O(1).
//                       The table layout mirrors `struct prism_identity` (24B
//                       value) keyed by cgroup id (u64), as in prism_maps.bpf.h.
//
//   (2) BASELINE path — re-derive the classification from the cgroup PATH STRING
//                       every time, scx_layered-style: tokenize on '/', then
//                       walk an ordered rule list testing substring/prefix/suffix
//                       predicates against the segments, first match wins. This
//                       is the same algorithm as pkg/classify, in C, so the two
//                       sides are measured under identical conditions.
//
// Output is machine-parseable:
//   PRISM_NS_PER_OP <x>
//   BASELINE_NS_PER_OP <y>
//   RATIO <y/x>
// plus a p50/p90/p99 percentile summary per path (sampled batches).
//
// Build:  clang -O2 -o /tmp/prism_ubench bench/native/microbench.c
// Run:    /tmp/prism_ubench [iterations]   (default 1000000)

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

// ---------------------------------------------------------------------------
// PRISM path: open-addressing (linear probing) hash table, u64 -> value.
// ---------------------------------------------------------------------------

// Mirrors `struct prism_identity` from bpf/prism_maps.bpf.h (24 bytes).
struct prism_identity {
    uint32_t identity;
    uint32_t flags;
    uint64_t label_hash;
    uint64_t updated_ns;
};

struct slot {
    uint64_t key;              // cgroup id; 0 == empty (we never insert key 0)
    struct prism_identity val;
};

// Power-of-two capacity so masking replaces modulo. Load factor kept < 0.5.
#define PRISM_CAP_BITS 16
#define PRISM_CAP (1u << PRISM_CAP_BITS)
#define PRISM_MASK (PRISM_CAP - 1u)

static struct slot g_table[PRISM_CAP];

// splitmix64 finalizer: good integer hash for the cgroup-id key.
static inline uint64_t hash_u64(uint64_t x) {
    x += 0x9E3779B97F4A7C15ull;
    x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9ull;
    x = (x ^ (x >> 27)) * 0x94D049BB133111EBull;
    return x ^ (x >> 31);
}

static void prism_insert(uint64_t key, struct prism_identity val) {
    uint64_t i = hash_u64(key) & PRISM_MASK;
    while (g_table[i].key != 0) {
        if (g_table[i].key == key) { g_table[i].val = val; return; }
        i = (i + 1) & PRISM_MASK;
    }
    g_table[i].key = key;
    g_table[i].val = val;
}

// prism_lookup emulates the scheduler's per-decision map read. Returns 1 on hit
// and writes the value through *out. This is THE operation we time.
static inline int prism_lookup(uint64_t key, struct prism_identity *out) {
    uint64_t i = hash_u64(key) & PRISM_MASK;
    for (;;) {
        uint64_t k = g_table[i].key;
        if (k == key) { *out = g_table[i].val; return 1; }
        if (k == 0) return 0;
        i = (i + 1) & PRISM_MASK;
    }
}

// ---------------------------------------------------------------------------
// BASELINE path: scx_layered-style cgroup-path classifier (re-derived per call).
// ---------------------------------------------------------------------------

enum pred_kind { PRED_CONTAINS, PRED_PREFIX, PRED_SUFFIX };

struct predicate {
    enum pred_kind kind;
    const char *value;
    size_t vlen;
};

#define MAX_PREDS 3
struct rule {
    const char *class;
    uint32_t id;
    struct predicate preds[MAX_PREDS];
    int npreds;
};

#define MAX_SEGMENTS 24
#define MAX_PATH 512

// Tokenize path on '/'. Stores pointers + lengths into the caller's scratch copy
// (we NUL-terminate in place). Returns the number of segments.
static int tokenize(char *path, const char **segs, size_t *lens) {
    int n = 0;
    char *p = path;
    while (*p && n < MAX_SEGMENTS) {
        while (*p == '/') p++;          // skip separators (and leading '/')
        if (!*p) break;
        const char *start = p;
        while (*p && *p != '/') p++;
        segs[n] = start;
        lens[n] = (size_t)(p - start);
        n++;
    }
    return n;
}

static inline int seg_contains(const char *s, size_t slen, const char *v, size_t vlen) {
    if (vlen == 0) return 1;
    if (vlen > slen) return 0;
    for (size_t i = 0; i + vlen <= slen; i++)
        if (memcmp(s + i, v, vlen) == 0) return 1;
    return 0;
}

static inline int pred_match(const struct predicate *pr,
                             const char **segs, const size_t *lens, int nseg) {
    for (int i = 0; i < nseg; i++) {
        const char *s = segs[i];
        size_t sl = lens[i];
        switch (pr->kind) {
        case PRED_PREFIX:
            if (sl >= pr->vlen && memcmp(s, pr->value, pr->vlen) == 0) return 1;
            break;
        case PRED_SUFFIX:
            if (sl >= pr->vlen && memcmp(s + sl - pr->vlen, pr->value, pr->vlen) == 0) return 1;
            break;
        case PRED_CONTAINS:
        default:
            if (seg_contains(s, sl, pr->value, pr->vlen)) return 1;
            break;
        }
    }
    return 0;
}

// classify re-derives the class from the cgroup path; first matching rule wins,
// else returns 0 (unmanaged). *out_id receives the identity. This is THE
// baseline operation we time. `path` is mutated (tokenized) so callers pass a
// fresh copy each iteration — exactly the per-decision cost we want to capture.
static int classify(char *path, const struct rule *rules, int nrules, uint32_t *out_id) {
    const char *segs[MAX_SEGMENTS];
    size_t lens[MAX_SEGMENTS];
    int nseg = tokenize(path, segs, lens);

    for (int r = 0; r < nrules; r++) {
        int all = rules[r].npreds > 0;
        for (int k = 0; k < rules[r].npreds; k++) {
            if (!pred_match(&rules[r].preds[k], segs, lens, nseg)) { all = 0; break; }
        }
        if (all) { *out_id = rules[r].id; return 1; }
    }
    *out_id = 3 /* IDUnmanaged */;
    return 0;
}

// Representative rule set, kept in lock-step with pkg/classify.DefaultRules().
#define P(k, v) { (k), (v), sizeof(v) - 1 }
static struct rule g_rules[] = {
    { "ingress-nginx",   256, { P(PRED_PREFIX, "kubepods"), P(PRED_CONTAINS, "ingress-nginx") }, 2 },
    { "kube-system-dns", 257, { P(PRED_CONTAINS, "kube-system"), P(PRED_CONTAINS, "coredns") }, 2 },
    { "monitoring",      258, { P(PRED_PREFIX, "kubepods"), P(PRED_CONTAINS, "prometheus") }, 2 },
    { "batch-job",       259, { P(PRED_PREFIX, "kubepods"), P(PRED_CONTAINS, "job-"), P(PRED_SUFFIX, ".scope") }, 3 },
    { "pod-guaranteed",  260, { P(PRED_PREFIX, "kubepods.slice"), P(PRED_PREFIX, "kubepods-pod"), P(PRED_SUFFIX, ".scope") }, 3 },
    { "pod-burstable",   261, { P(PRED_CONTAINS, "kubepods-burstable"), P(PRED_SUFFIX, ".scope") }, 2 },
    { "pod-besteffort",  262, { P(PRED_CONTAINS, "kubepods-besteffort"), P(PRED_SUFFIX, ".scope") }, 2 },
    { "pod-generic",     263, { P(PRED_PREFIX, "kubepods"), P(PRED_SUFFIX, ".scope") }, 2 },
    { "system",            1, { P(PRED_PREFIX, "system.slice") }, 1 },
    { "user",            264, { P(PRED_PREFIX, "user.slice") }, 1 },
    { "init",              5, { P(PRED_PREFIX, "init.scope") }, 1 },
};
static const int g_nrules = (int)(sizeof(g_rules) / sizeof(g_rules[0]));

// ---------------------------------------------------------------------------
// Timing helpers.
// ---------------------------------------------------------------------------

static inline uint64_t now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

static int cmp_u64(const void *a, const void *b) {
    uint64_t x = *(const uint64_t *)a, y = *(const uint64_t *)b;
    return (x > y) - (x < y);
}

// A small representative set of realistic cgroup paths to classify, rotated over
// so the branch predictor / cache see variety (as the real hot path would).
static const char *g_paths[] = {
    "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod3f2a1b9c_1234.slice/cri-containerd-9ab3c7de.scope",
    "/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod7c1d.slice/cri-containerd-deadbeef0123.scope",
    "/sys/fs/cgroup/kubepods.slice/kubepods-pod5e6f.slice/cri-containerd-ingress-nginx-controller-abc.scope",
    "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaa11.slice/cri-containerd-prometheus-server.scope",
    "/sys/fs/cgroup/system.slice/containerd.service",
    "/sys/fs/cgroup/user.slice/user-1000.slice/session-3.scope",
};
static const int g_npaths = (int)(sizeof(g_paths) / sizeof(g_paths[0]));

#define PERCENTILE_SAMPLES 4096   // per-batch timings collected for percentiles
#define BATCH 64                   // ops per timed batch (amortizes clock cost)

int main(int argc, char **argv) {
    uint64_t iters = 1000000;
    if (argc > 1) {
        long v = strtol(argv[1], NULL, 10);
        if (v > 0) iters = (uint64_t)v;
    }

    // --- populate the Prism map with a realistic number of live workloads ---
    const uint64_t n_workloads = 4096; // typical busy node fan-out
    for (uint64_t i = 0; i < n_workloads; i++) {
        struct prism_identity v = {
            .identity = (uint32_t)(256 + (i % 1024)),
            .flags = 0x4 /* OBSERVED */,
            .label_hash = hash_u64(i * 2654435761ull),
            .updated_ns = now_ns(),
        };
        prism_insert(i + 1, v); // keys 1..n_workloads (0 reserved as empty)
    }

    // volatile sinks to stop the optimizer from eliding the work.
    volatile uint64_t sink_id = 0;
    volatile int sink_ok = 0;

    uint64_t *samples = malloc(sizeof(uint64_t) * PERCENTILE_SAMPLES);
    if (!samples) { perror("malloc"); return 1; }

    // ===================== PRISM PATH =====================
    {
        // warmup
        for (uint64_t i = 0; i < 10000; i++) {
            struct prism_identity out;
            sink_ok ^= prism_lookup((i % n_workloads) + 1, &out);
            sink_id ^= out.identity;
        }
        int s = 0;
        uint64_t t0 = now_ns();
        for (uint64_t i = 0; i < iters; i += BATCH) {
            uint64_t b0 = now_ns();
            for (int j = 0; j < BATCH; j++) {
                struct prism_identity out;
                uint64_t key = (((i + (uint64_t)j) * 1099511628211ull) % n_workloads) + 1;
                sink_ok ^= prism_lookup(key, &out);
                sink_id ^= out.identity ^ out.label_hash;
            }
            uint64_t b1 = now_ns();
            if (s < PERCENTILE_SAMPLES) samples[s++] = (b1 - b0) / BATCH;
        }
        uint64_t t1 = now_ns();
        double ns_per_op = (double)(t1 - t0) / (double)iters;

        qsort(samples, s, sizeof(uint64_t), cmp_u64);
        printf("PRISM_NS_PER_OP %.3f\n", ns_per_op);
        printf("PRISM_P50_NS %llu\n", (unsigned long long)samples[s * 50 / 100]);
        printf("PRISM_P90_NS %llu\n", (unsigned long long)samples[s * 90 / 100]);
        printf("PRISM_P99_NS %llu\n", (unsigned long long)samples[(s * 99 / 100 < s) ? s * 99 / 100 : s - 1]);

        // ===================== BASELINE PATH =====================
        char scratch[MAX_PATH];
        // warmup
        for (uint64_t i = 0; i < 10000; i++) {
            uint32_t id;
            strncpy(scratch, g_paths[i % g_npaths], sizeof(scratch) - 1);
            scratch[sizeof(scratch) - 1] = '\0';
            sink_ok ^= classify(scratch, g_rules, g_nrules, &id);
            sink_id ^= id;
        }
        s = 0;
        double prism_ns_per_op = ns_per_op;
        t0 = now_ns();
        for (uint64_t i = 0; i < iters; i += BATCH) {
            uint64_t b0 = now_ns();
            for (int j = 0; j < BATCH; j++) {
                uint32_t id;
                const char *src = g_paths[(i + (uint64_t)j) % (uint64_t)g_npaths];
                // Copy per iteration: classify tokenizes in place, mirroring the
                // real per-decision cost of reading+parsing the cgroup path.
                strncpy(scratch, src, sizeof(scratch) - 1);
                scratch[sizeof(scratch) - 1] = '\0';
                sink_ok ^= classify(scratch, g_rules, g_nrules, &id);
                sink_id ^= id;
            }
            uint64_t b1 = now_ns();
            if (s < PERCENTILE_SAMPLES) samples[s++] = (b1 - b0) / BATCH;
        }
        t1 = now_ns();
        double base_ns_per_op = (double)(t1 - t0) / (double)iters;

        qsort(samples, s, sizeof(uint64_t), cmp_u64);
        printf("BASELINE_NS_PER_OP %.3f\n", base_ns_per_op);
        printf("BASELINE_P50_NS %llu\n", (unsigned long long)samples[s * 50 / 100]);
        printf("BASELINE_P90_NS %llu\n", (unsigned long long)samples[s * 90 / 100]);
        printf("BASELINE_P99_NS %llu\n", (unsigned long long)samples[(s * 99 / 100 < s) ? s * 99 / 100 : s - 1]);

        printf("RATIO %.2f\n", base_ns_per_op / prism_ns_per_op);
        printf("ITERATIONS %llu\n", (unsigned long long)iters);
    }

    // Touch the sinks so they cannot be optimized away.
    if (sink_id == 0xDEADBEEFCAFEBABEull && sink_ok == 123456789) {
        fprintf(stderr, "unreachable %llu %d\n",
                (unsigned long long)sink_id, sink_ok);
    }
    free(samples);
    return 0;
}
