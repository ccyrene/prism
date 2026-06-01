// SPDX-License-Identifier: Apache-2.0

// consumer_overhead.c — what does it COST a famous eBPF tool (execsnoop) to get
// workload identity per event, the Prism way vs the do-it-yourself way?
//
// Models execsnoop's per-event hot path (fill pid/uid, copy comm) as the
// BASELINE, then measures the ADDED cost of two ways to attribute the event to
// a Kubernetes workload:
//   PRISM      : one O(1) lookup in the shared identity map (what the 4-line
//                integration in execsnoop_prism.bpf.c does, in-kernel).
//   SELF-DERIVE: parse /proc/<pid>/cgroup and pattern-match the slice path —
//                what a consumer must do today to map an event to a pod
//                (scx_layered-style classification; the userspace alternative).
//
// Standalone userspace C (clang -O2). Runs anywhere (no BPF/root). The PRISM
// number is the same hash-lookup cost that approximates the in-kernel BPF map.
//
//   clang -O2 -o /tmp/consumer_overhead bench/native/consumer_overhead.c
//   /tmp/consumer_overhead 2000000

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

/* ---- shared prism map model: open-addressing u64 cgroupid -> identity ---- */
struct pval { uint32_t identity, flags; uint64_t label_hash, updated_ns; };
struct slot { uint64_t key; int used; struct pval v; };
#define CAP 8192
static struct slot table[CAP];

static inline uint64_t mix(uint64_t x){ x+=0x9E3779B97F4A7C15ULL; x=(x^(x>>30))*0xBF58476D1CE4E5B9ULL;
	x=(x^(x>>27))*0x94D049BB133111EBULL; return x^(x>>31); }
static void put(uint64_t k, uint32_t id){ uint64_t i=mix(k)&(CAP-1);
	while(table[i].used){ if(table[i].key==k) break; i=(i+1)&(CAP-1);}
	table[i].used=1; table[i].key=k; table[i].v.identity=id; }
static inline const struct pval* get(uint64_t k){ uint64_t i=mix(k)&(CAP-1);
	for(;;){ if(!table[i].used) return 0; if(table[i].key==k) return &table[i].v; i=(i+1)&(CAP-1);} }

/* ---- self-derive model: tokenize a cgroup path + match scx_layered-style rules ---- */
static const char *RULES[] = {
	"kubepods-guaranteed","kubepods-burstable","kubepods-besteffort","kubepods",
	"ingress-nginx","coredns","prometheus","batch","system.slice","user.slice","init.scope"
};
#define NRULES (int)(sizeof(RULES)/sizeof(RULES[0]))
static uint32_t classify(const char *path){
	char buf[160]; size_t n=strlen(path); if(n>=sizeof(buf)) n=sizeof(buf)-1;
	memcpy(buf,path,n); buf[n]=0;
	for(char *seg=strtok(buf,"/"); seg; seg=strtok(0,"/"))
		for(int r=0;r<NRULES;r++)
			if(strstr(seg,RULES[r])) return (uint32_t)(256+r);
	return 3; /* unmanaged */
}

/* ---- baseline: execsnoop's per-event work (fill pid/uid, copy comm) ---- */
struct ev { uint32_t pid, uid; char comm[16]; uint32_t identity; };
static volatile uint64_t sink;

static double now_ns(void){ struct timespec t; clock_gettime(CLOCK_MONOTONIC,&t);
	return (double)t.tv_sec*1e9 + t.tv_nsec; }

int main(int argc,char**argv){
	long N = argc>1 ? atol(argv[1]) : 2000000;
	for(int i=0;i<4096;i++) put(mix((uint64_t)i*0x9E37+1), 256+(i%2000));
	const char *cgpath="/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/"
		"kubepods-burstable-pod1a2b3c4d_5e6f.slice/cri-containerd-abc123.scope";
	const char *src="prod-server\0\0\0\0";

	double t; struct ev e;
	/* baseline */
	t=now_ns();
	for(long i=0;i<N;i++){ e.pid=(uint32_t)i; e.uid=1000; memcpy(e.comm,src,16); sink+=e.pid+(unsigned char)e.comm[0]; }
	double base=(now_ns()-t)/N;
	/* + PRISM lookup */
	t=now_ns();
	for(long i=0;i<N;i++){ e.pid=(uint32_t)i; e.uid=1000; memcpy(e.comm,src,16);
		const struct pval*p=get(mix((uint64_t)(i&4095)*0x9E37+1)); e.identity=p?p->identity:0; sink+=e.identity; }
	double prism=(now_ns()-t)/N;
	/* + SELF-DERIVE (cgroup parse) */
	t=now_ns();
	for(long i=0;i<N;i++){ e.pid=(uint32_t)i; e.uid=1000; memcpy(e.comm,src,16);
		e.identity=classify(cgpath); sink+=e.identity; }
	double self=(now_ns()-t)/N;

	double padd=prism-base, sadd=self-base;
	/* A real exec event is NOT free: the execve() syscall itself is microseconds,
	 * and execsnoop's own handling (perf_event_output + the argv probe_read loop)
	 * is hundreds of ns. We use a deliberately conservative 1500 ns as "the real
	 * per-event cost a consumer already pays" and express the identity-attribution
	 * overhead as a fraction of THAT — not of our trivial struct-fill micro-baseline. */
	const double REAL_EVENT_NS = 1500.0;
	printf("MICRO_BASELINE_NS_PER_EVENT   %.2f   (struct fill + comm copy only)\n", base);
	printf("PRISM_ADD_NS_PER_EVENT        %.2f   (one shared-map lookup)\n", padd);
	printf("SELFDERIVE_ADD_NS_PER_EVENT   %.2f   (cgroup-path parse + classify)\n", sadd);
	printf("PRISM_OVERHEAD_VS_REAL_EVENT  %.2f%%   (%.2f ns of ~%.0f ns)\n", 100*padd/REAL_EVENT_NS, padd, REAL_EVENT_NS);
	printf("SELFDERIVE_VS_REAL_EVENT      %.2f%%\n", 100*sadd/REAL_EVENT_NS);
	printf("PRISM_CHEAPER_THAN_SELFDERIVE %.0fx\n", sadd/(padd>0?padd:1e-9));
	printf("ITERATIONS %ld  (sink %llu)\n", N, (unsigned long long)sink);
	return 0;
}
