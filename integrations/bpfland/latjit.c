// SPDX-License-Identifier: Apache-2.0

// latjit.c — CPU-bound latency-critical probe (bpfland heuristic blind spot #2).
//
// WHY: scx_bpfland prioritizes by a SLEEP-FREQUENCY heuristic — a task that
// sleeps often and runs in short bursts is treated as latency-critical. A task
// that is latency-critical but ALSO CPU-bound (rarely sleeps) is mislabeled as
// batch and deprioritized under contention. Prism fixes this: the operator marks
// the workload CRITICAL on the identity bus and the retrofit protects it
// regardless of sleep behaviour.
//
// MODEL: a periodic CPU-bound deadline task (think: a control loop / frame
// render / packet-batch that must finish each period on time). N worker threads,
// each repeatedly does a FIXED amount of real arithmetic (one "period") and
// times the WALL-CLOCK it took. With no contention wall ≈ the pure compute time;
// under contention a DEPRIORITIZED task's period inflates because it waits in the
// runqueue for CPU. The threads never sleep between periods, so bpfland's sleep
// heuristic sees them as batch. We report p50/p99/p99.9 of per-period wall time
// in microseconds — lower = the scheduler let the CPU-bound task meet its
// deadline. (Per-period sampling, not per-iteration: each period is one sample,
// so the rare-but-large scheduling stalls land in meaningful percentiles.)
//
// Build: cc -O2 -pthread -o latjit latjit.c
// Run:   THREADS=8 DURATION=5 PERIOD_ITERS=2000 ./latjit   -> "p50 p99 p999" us
// Place its PID in a cgroup BEFORE exec (the eval harness does this); children
// inherit the cgroup, so the bus key (leaf cgroup id) matches the seed.

#define _GNU_SOURCE
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <time.h>
#include <string.h>

#ifndef NBUCKETS
#define NBUCKETS 100001          /* 0..100000 us, then overflow into last bucket */
#endif

static int      g_threads;
static double   g_duration;
static int      g_period_iters;
static volatile int g_go = 0;

static inline uint64_t now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ull + ts.tv_nsec;
}

struct thr {
    pthread_t  id;
    uint64_t  *hist;             /* per-period wall-time histogram, bucket = us  */
    uint64_t   periods;
    double     acc;              /* keeps the compiler from eliding the work     */
};

/* A fixed, compiler-opaque chunk of arithmetic (~a few tens of ns). */
static inline double work_chunk(double x) {
    for (int i = 0; i < 64; i++)
        x = x * 1.000001 + 0.5;
    return x;
}

static void *worker(void *arg) {
    struct thr *t = arg;
    int iters = g_period_iters;
    double x = 1.0;
    while (!__atomic_load_n(&g_go, __ATOMIC_ACQUIRE)) ;
    uint64_t end = now_ns() + (uint64_t)(g_duration * 1e9);
    for (;;) {
        uint64_t t0 = now_ns();
        for (int i = 0; i < iters; i++)         /* ONE period of fixed CPU work  */
            x = work_chunk(x);
        uint64_t t1 = now_ns();
        uint64_t wall_us = (t1 - t0) / 1000;    /* per-period wall time in us     */
        if (wall_us >= NBUCKETS) wall_us = NBUCKETS - 1;
        t->hist[wall_us]++;
        t->periods++;
        if (t1 >= end) break;
    }
    t->acc = x;
    return NULL;
}

static uint64_t pct(const uint64_t *h, uint64_t total, double q) {
    uint64_t target = (uint64_t)(q * (double)total);
    uint64_t acc = 0;
    for (int i = 0; i < NBUCKETS; i++) {
        acc += h[i];
        if (acc >= target) return (uint64_t)i;
    }
    return NBUCKETS - 1;
}

int main(void) {
    g_threads      = getenv("THREADS")      ? atoi(getenv("THREADS"))      : 8;
    g_duration     = getenv("DURATION")     ? atof(getenv("DURATION"))     : 5.0;
    g_period_iters = getenv("PERIOD_ITERS") ? atoi(getenv("PERIOD_ITERS")) : 2000;
    if (g_threads < 1) g_threads = 1;
    if (g_period_iters < 1) g_period_iters = 1;

    struct thr *T = calloc(g_threads, sizeof *T);
    for (int i = 0; i < g_threads; i++) {
        T[i].hist = calloc(NBUCKETS, sizeof(uint64_t));
        if (!T[i].hist) { perror("calloc"); return 1; }
        pthread_create(&T[i].id, NULL, worker, &T[i]);
    }
    __atomic_store_n(&g_go, 1, __ATOMIC_RELEASE);
    for (int i = 0; i < g_threads; i++) pthread_join(T[i].id, NULL);

    uint64_t *h = calloc(NBUCKETS, sizeof(uint64_t)), total = 0;
    for (int i = 0; i < g_threads; i++) {
        for (int b = 0; b < NBUCKETS; b++) h[b] += T[i].hist[b];
        total += T[i].periods;
    }
    uint64_t p50 = pct(h, total, 0.50), p99 = pct(h, total, 0.99), p999 = pct(h, total, 0.999);
    printf("%llu %llu %llu\n",
           (unsigned long long)p50, (unsigned long long)p99, (unsigned long long)p999);
    return 0;
}
