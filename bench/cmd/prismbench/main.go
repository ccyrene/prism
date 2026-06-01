// SPDX-License-Identifier: Apache-2.0

// Command prismbench measures the Prism identity bus with the statistical rigor
// the project's own eval-methodology module prescribes: every scenario is run as
// many timed trials, and we report the full distribution (p50/p90/p99/p99.9),
// coefficient of variation, and a percentile-bootstrap 95% CI on the median —
// not a bare mean. Warmup trials are discarded; randomness is fixed-seed.
//
// All scenarios run on the userspace (sim) path so they execute on any host
// (no root / no sched_ext needed). The kernel BPF-map lookup is corroborated
// separately by bench/native/microbench.c.
package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/prism-bus/prism/pkg/abi"
	"github.com/prism-bus/prism/pkg/classify"
	"github.com/prism-bus/prism/pkg/identity"
	prismsync "github.com/prism-bus/prism/pkg/sync"
)

const (
	warmup = 8  // discarded trials
	trials = 60 // measured trials per scenario
)

var blackhole uint64 // defeats dead-code elimination

// timeNsPerOp runs fn ops times and returns nanoseconds per op for one trial.
func timeNsPerOp(ops int, fn func(i int)) float64 {
	start := time.Now()
	for i := 0; i < ops; i++ {
		fn(i)
	}
	return float64(time.Since(start).Nanoseconds()) / float64(ops)
}

// runTrials collects `trials` measured ns/op samples (after `warmup` discarded).
// setup runs once before each trial (not timed) and may return state via closure.
func runTrials(ops int, body func(i int)) []float64 {
	out := make([]float64, 0, trials)
	for t := 0; t < warmup+trials; t++ {
		runtime.GC()
		v := timeNsPerOp(ops, body)
		if t >= warmup {
			out = append(out, v)
		}
	}
	return out
}

// ---- workload generators -------------------------------------------------

func canonFor(i int) string {
	// A realistic-ish security-relevant label set; ~"app=svcN;tier=tK".
	return fmt.Sprintf("app=svc%d;tier=t%d", i, i%5)
}

func cgroupPathFor(i int) string {
	tiers := []string{"guaranteed", "burstable", "besteffort"}
	tier := tiers[i%3]
	return fmt.Sprintf(
		"/sys/fs/cgroup/kubepods.slice/kubepods-%s.slice/kubepods-%s-pod%08x_%04x.slice/cri-containerd-%016x.scope",
		tier, tier, i, i%9973, uint64(i)*0x9E3779B97F4A7C15)
}

func podFor(i int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pod-%d", i),
			Namespace: "bench",
			UID:       types.UID(fmt.Sprintf("uid-%08d-%d", i, i*2654435761)),
			Labels: map[string]string{
				"app":              fmt.Sprintf("svc%d", i),
				"tier":             fmt.Sprintf("t%d", i%5),
				"pod-template-hash": fmt.Sprintf("%x", i), // volatile, must be ignored
			},
		},
	}
}

// ---- scenarios -----------------------------------------------------------

// scenarioResolution: the headline per-decision identity-resolution cost.
// Prism = one O(1) sink lookup; baseline = scx_layered-style cgroup-path parse.
func scenarioResolution() (prism, base Stats, prismSamp, baseSamp []float64) {
	const pop = 4096
	const ops = 200000

	sink := newPopulatedSink(pop)
	keys := make([]abi.WorkloadKey, pop)
	for i := 0; i < pop; i++ {
		keys[i] = prismsync.WorkloadKey(types.UID(fmt.Sprintf("uid-%08d-%d", i, i*2654435761)))
	}
	idx := randIndices(ops, pop)

	prismSamp = runTrials(ops, func(i int) {
		v, _, _ := sink.Lookup(keys[idx[i]])
		blackhole += uint64(v.Identity)
	})

	m := classify.NewMatcher(classify.DefaultRules())
	paths := make([]string, pop)
	for i := 0; i < pop; i++ {
		paths[i] = cgroupPathFor(i)
	}
	baseSamp = runTrials(ops, func(i int) {
		_, id, _ := m.Classify(paths[idx[i]])
		blackhole += uint64(id)
	})

	return summarize("resolution_prism", "ns/op", prismSamp),
		summarize("resolution_baseline", "ns/op", baseSamp), prismSamp, baseSamp
}

// scenarioAlloc: cold (new label set) vs dedup (existing) allocation cost.
func scenarioAlloc() (cold, dedup Stats) {
	const ops = 50000
	canon := make([]string, ops)
	for i := range canon {
		canon[i] = canonFor(i)
	}

	coldSamp := make([]float64, 0, trials)
	for t := 0; t < warmup+trials; t++ {
		al := identity.NewAllocator()
		runtime.GC()
		v := timeNsPerOp(ops, func(i int) {
			id, _, _ := al.Allocate(canon[i])
			blackhole += uint64(id)
		})
		if t >= warmup {
			coldSamp = append(coldSamp, v)
		}
	}

	alDedup := identity.NewAllocator()
	for i := 0; i < ops; i++ {
		alDedup.Allocate(canon[i])
	}
	dedupSamp := runTrials(ops, func(i int) {
		id, _, _ := alDedup.Allocate(canon[i]) // existing -> dedup path
		blackhole += uint64(id)
	})

	return summarize("alloc_cold", "ns/op", coldSamp), summarize("alloc_dedup", "ns/op", dedupSamp)
}

// scenarioE2E: end-to-end per-pod control-plane propagation latency
// (pod object -> canonical -> allocate -> sink upsert), for fresh pods.
func scenarioE2E() Stats {
	const ops = 20000
	pods := make([]*corev1.Pod, ops)
	for i := range pods {
		pods[i] = podFor(i)
	}
	samp := make([]float64, 0, trials)
	for t := 0; t < warmup+trials; t++ {
		ctrl := prismsync.NewController(nil, newSim())
		runtime.GC()
		v := timeNsPerOp(ops, func(i int) {
			_ = ctrl.HandlePod(pods[i], prismsync.EventAdd)
		})
		if t >= warmup {
			samp = append(samp, v)
		}
	}
	return summarize("e2e_pod_add", "ns/op", samp)
}

// scenarioChurn: interleaved add+delete (allocator id-reuse + sink churn).
func scenarioChurn() Stats {
	const ops = 20000
	pods := make([]*corev1.Pod, ops)
	for i := range pods {
		pods[i] = podFor(i)
	}
	samp := make([]float64, 0, trials)
	for t := 0; t < warmup+trials; t++ {
		ctrl := prismsync.NewController(nil, newSim())
		runtime.GC()
		v := timeNsPerOp(ops, func(i int) {
			_ = ctrl.HandlePod(pods[i], prismsync.EventAdd)
			_ = ctrl.HandlePod(pods[i], prismsync.EventDelete)
		})
		if t >= warmup {
			samp = append(samp, v)
		}
	}
	return summarize("churn_add_delete", "ns/op", samp)
}

// scaleRow is one point of the scale sweep.
type scaleRow struct {
	Count       int     `json:"live_identities"`
	AllocNsOp   float64 `json:"alloc_ns_per_op"`
	LookupNsOp  float64 `json:"lookup_ns_per_op"`
	BaselineNs  float64 `json:"baseline_ns_per_op"`
}

// scenarioScale shows O(1) behavior as the live identity population grows.
func scenarioScale() []scaleRow {
	counts := []int{1000, 10000, 100000, 1000000}
	m := classify.NewMatcher(classify.DefaultRules())
	rows := make([]scaleRow, 0, len(counts))
	for _, n := range counts {
		al := identity.NewAllocator()
		sink := newSim()
		canon := make([]string, n)
		for i := 0; i < n; i++ {
			canon[i] = fmt.Sprintf("app=svc%d;tier=t%d", i, i%7)
		}
		// alloc cost at this scale (cold, single pass)
		allocNs := timeNsPerOp(n, func(i int) {
			id, _, _ := al.Allocate(canon[i])
			k := prismsync.WorkloadKey(types.UID(fmt.Sprintf("u%d", i)))
			sink.Upsert(k, abi.PrismIdentity{Identity: uint32(id)})
			blackhole += uint64(id)
		})
		// lookup cost at this scale
		keys := make([]abi.WorkloadKey, n)
		for i := 0; i < n; i++ {
			keys[i] = prismsync.WorkloadKey(types.UID(fmt.Sprintf("u%d", i)))
		}
		lops := 500000
		idx := randIndices(lops, n)
		lookupNs := median3(func() float64 {
			return timeNsPerOp(lops, func(i int) {
				v, _, _ := sink.Lookup(keys[idx[i]])
				blackhole += uint64(v.Identity)
			})
		})
		paths := make([]string, 1024)
		for i := range paths {
			paths[i] = cgroupPathFor(i)
		}
		baseNs := median3(func() float64 {
			return timeNsPerOp(lops, func(i int) {
				_, id, _ := m.Classify(paths[i&1023])
				blackhole += uint64(id)
			})
		})
		rows = append(rows, scaleRow{Count: n, AllocNsOp: allocNs, LookupNsOp: lookupNs, BaselineNs: baseNs})
		fmt.Printf("  scale n=%-8d alloc=%.1f ns  lookup=%.2f ns  baseline=%.1f ns\n", n, allocNs, lookupNs, baseNs)
	}
	return rows
}

// ---- helpers -------------------------------------------------------------

func newSim() abi.Sink {
	s, err := newSimSink()
	if err != nil {
		panic(err)
	}
	return s
}

func newPopulatedSink(pop int) abi.Sink {
	s := newSim()
	for i := 0; i < pop; i++ {
		k := prismsync.WorkloadKey(types.UID(fmt.Sprintf("uid-%08d-%d", i, i*2654435761)))
		s.Upsert(k, abi.PrismIdentity{Identity: uint32(identity.MinDynamicID) + uint32(i), Flags: abi.FlagSchedManaged})
	}
	return s
}

func randIndices(n, mod int) []int {
	rng := rand.New(rand.NewSource(1))
	out := make([]int, n)
	for i := range out {
		out[i] = rng.Intn(mod)
	}
	return out
}

func median3(f func() float64) float64 {
	a, b, c := f(), f(), f()
	if a > b {
		a, b = b, a
	}
	if b > c {
		b, c = c, b
	}
	if a > b {
		a, b = b, a
	}
	return b
}

// Report is the full machine-readable result bundle.
type Report struct {
	Host      map[string]string `json:"host"`
	Method    map[string]any    `json:"method"`
	Scenarios []Stats           `json:"scenarios"`
	Scale     []scaleRow        `json:"scale"`
	RatioMed  float64           `json:"resolution_ratio_median"`
}

func main() {
	outDir := "bench/results"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		panic(err)
	}

	fmt.Println("== Prism benchmark suite (sim path) ==")
	fmt.Println("[1/5] resolution (Prism O(1) lookup vs scx_layered-style cgroup-path classify)")
	prism, base, prismSamp, baseSamp := scenarioResolution()
	fmt.Printf("  Prism    p50=%.2f ns  p99=%.2f ns  CoV=%.3f\n", prism.Median, prism.P99, prism.CoV)
	fmt.Printf("  Baseline p50=%.2f ns  p99=%.2f ns  CoV=%.3f\n", base.Median, base.P99, base.CoV)
	ratio := base.Median / prism.Median
	fmt.Printf("  RATIO (median baseline/prism) = %.1fx\n", ratio)

	fmt.Println("[2/5] allocation (cold new vs dedup existing)")
	cold, dedup := scenarioAlloc()
	fmt.Printf("  cold  p50=%.1f ns  p99=%.1f ns\n", cold.Median, cold.P99)
	fmt.Printf("  dedup p50=%.1f ns  p99=%.1f ns\n", dedup.Median, dedup.P99)

	fmt.Println("[3/5] end-to-end pod-add propagation")
	e2e := scenarioE2E()
	fmt.Printf("  p50=%.1f ns  p99=%.1f ns  p99.9=%.1f ns\n", e2e.Median, e2e.P99, e2e.P999)

	fmt.Println("[4/5] churn (add+delete)")
	churn := scenarioChurn()
	fmt.Printf("  p50=%.1f ns  p99=%.1f ns\n", churn.Median, churn.P99)

	fmt.Println("[5/5] scale sweep")
	scale := scenarioScale()

	rep := Report{
		Host: map[string]string{
			"kernel":     readFirstLine("/proc/sys/kernel/osrelease"),
			"go_version": runtime.Version(),
			"goarch":     runtime.GOARCH,
			"goos":       runtime.GOOS,
			"cpu":        cpuModel(),
			"gomaxprocs": fmt.Sprintf("%d", runtime.GOMAXPROCS(0)),
		},
		Method: map[string]any{
			"warmup_trials":   warmup,
			"measured_trials": trials,
			"timing":          "single-goroutine ns/op per trial; GC before each trial; CLOCK via time.Now monotonic",
			"ci":              "percentile bootstrap, 2000 resamples, 95% on median",
			"note":            "trial-level distribution (per-op timing is below clock resolution at the Prism end, so we time batches and report the trial distribution)",
		},
		Scenarios: []Stats{prism, base, cold, dedup, e2e, churn},
		Scale:     scale,
		RatioMed:  ratio,
	}

	writeJSON(filepath.Join(outDir, "results.json"), rep)
	writeResolutionCSV(filepath.Join(outDir, "resolution_samples.csv"), prismSamp, baseSamp)
	writeScaleCSV(filepath.Join(outDir, "scale.csv"), scale)
	fmt.Printf("\nWrote %s, resolution_samples.csv, scale.csv\n", filepath.Join(outDir, "results.json"))
	fmt.Printf("blackhole=%d\n", blackhole)
}
