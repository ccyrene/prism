// SPDX-License-Identifier: Apache-2.0

// net_overhead.c — per-packet cost a NETWORK consumer pays to attribute a packet
// to a workload via the Prism bus, vs re-deriving it itself, and what that means
// at line rate. Standalone userspace C (clang -O2), runs anywhere (no BPF/root).
// The PRISM number is the same shared-map lookup that approximates the in-kernel
// BPF map a cgroup-skb program hits per packet.
//
//   clang -O2 -o /tmp/net_overhead bench/native/net_overhead.c && /tmp/net_overhead 5000000

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

struct pval { uint32_t identity, flags; uint64_t h, t; };
struct slot { uint64_t key; int used; struct pval v; };
#define CAP 8192
static struct slot tbl[CAP];
static inline uint64_t mix(uint64_t x){ x+=0x9E3779B97F4A7C15ULL; x=(x^(x>>30))*0xBF58476D1CE4E5B9ULL;
	x=(x^(x>>27))*0x94D049BB133111EBULL; return x^(x>>31); }
static void put(uint64_t k,uint32_t id){ uint64_t i=mix(k)&(CAP-1);
	while(tbl[i].used){ if(tbl[i].key==k) break; i=(i+1)&(CAP-1);} tbl[i].used=1; tbl[i].key=k; tbl[i].v.identity=id; }
static inline const struct pval* get(uint64_t k){ uint64_t i=mix(k)&(CAP-1);
	for(;;){ if(!tbl[i].used) return 0; if(tbl[i].key==k) return &tbl[i].v; i=(i+1)&(CAP-1);} }

// scx_layered-style cgroup-path classify, the "derive it yourself per packet" path.
static const char *R[]={"kubepods-guaranteed","kubepods-burstable","kubepods-besteffort",
	"kubepods","ingress-nginx","coredns","prometheus","system.slice","user.slice","init.scope"};
#define NR (int)(sizeof(R)/sizeof(R[0]))
static uint32_t classify(const char*p){ char b[160]; size_t n=strlen(p); if(n>=sizeof(b))n=sizeof(b)-1;
	memcpy(b,p,n); b[n]=0; for(char*s=strtok(b,"/");s;s=strtok(0,"/")) for(int r=0;r<NR;r++) if(strstr(s,R[r])) return 256+r; return 3; }

static volatile uint64_t sink;
static double now(void){ struct timespec t; clock_gettime(CLOCK_MONOTONIC,&t); return t.tv_sec*1e9+t.tv_nsec; }

int main(int argc,char**argv){
	long N=argc>1?atol(argv[1]):5000000;
	for(int i=0;i<4096;i++) put(mix((uint64_t)i*0x9E37+1),256+(i%2000));
	const char*cg="/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/"
		"kubepods-burstable-pod1a2b3c4d_5e6f.slice/cri-containerd-abc.scope";
	uint64_t *keys=malloc(sizeof(uint64_t)*4096); for(int i=0;i<4096;i++) keys[i]=mix((uint64_t)i*0x9E37+1);

	double t=now(); for(long i=0;i<N;i++){ const struct pval*p=get(keys[i&4095]); sink+=p?p->identity:0; }
	double prism=(now()-t)/N;
	t=now(); for(long i=0;i<N;i++) sink+=classify(cg);
	double self=(now()-t)/N;

	// At a given packet rate, CPU spent ON ATTRIBUTION = ns/pkt * pps  (ns per second of one core).
	double pps=1e6;
	printf("NET_PRISM_NS_PER_PKT       %.3f\n", prism);
	printf("NET_SELFDERIVE_NS_PER_PKT  %.3f\n", self);
	printf("PRISM_CHEAPER             %.0fx\n", self/(prism>0?prism:1e-9));
	printf("PCT_1CORE_AT_1Mpps_PRISM   %.3f%%   (%.1f ns/pkt x 1e6 pps)\n", 100.0*prism*pps/1e9, prism);
	printf("PCT_1CORE_AT_1Mpps_SELF    %.2f%%\n", 100.0*self*pps/1e9);
	printf("ITERATIONS %ld (sink %llu)\n", N,(unsigned long long)sink);
	free(keys); return 0;
}
