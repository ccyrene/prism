// SPDX-License-Identifier: Apache-2.0

// Command memprobe measures prismd's resident memory. It builds the controller
// over a fake clientset, creates N realistically-bloated Pods (managedFields +
// spec + status, like real Pods), runs the informer, then reports Go heap and
// process RSS. Run it with -trim=false and -trim=true (separate processes, for
// clean RSS isolation) to see what the cache-trim TransformFunc saves.
//
//	go run ./bench/cmd/memprobe -n 256 -trim=false
//	go run ./bench/cmd/memprobe -n 256 -trim=true
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	prismsync "github.com/prism-bus/prism/pkg/sync"
	"github.com/prism-bus/prism/pkg/sink"
)

// bloatedPod builds a Pod close to a real one's in-memory size: managedFields
// (usually the biggest part), a 2-container spec, and a populated status.
func bloatedPod(i int) *corev1.Pod {
	big := strings.Repeat(`{"f:metadata":{"f:labels":{"f:app":{},"f:tier":{}}},"f:spec":{"f:containers":{}}},`, 12)
	mf := make([]metav1.ManagedFieldsEntry, 3)
	for j := range mf {
		mf[j] = metav1.ManagedFieldsEntry{
			Manager: "kube-controller-manager", Operation: "Update",
			APIVersion: "v1", FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte("{" + big + `"f:end":{}}`)},
		}
	}
	cpu := resource.MustParse("100m")
	mem := resource.MustParse("128Mi")
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("workload-%d", i), Namespace: "prod",
			UID:           types.UID(fmt.Sprintf("uid-%08d", i)),
			Labels:        map[string]string{"app": fmt.Sprintf("svc%d", i%50), "tier": "backend", "pod-template-hash": fmt.Sprintf("%x", i)},
			Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{" + big + "}"},
			ManagedFields: mf,
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "app", Image: "registry.example.com/app:v1.2.3", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem}}},
				{Name: "sidecar", Image: "registry.example.com/proxy:v2"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, QOSClass: corev1.PodQOSGuaranteed,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true, RestartCount: 0, Image: "registry.example.com/app:v1.2.3", ContainerID: "containerd://" + strings.Repeat("a", 64)},
				{Name: "sidecar", Ready: true, Image: "registry.example.com/proxy:v2"},
			},
		},
	}
}

func heapInuse() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

func rssMB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "VmRSS:") {
			var kb float64
			fmt.Sscanf(ln, "VmRSS: %g kB", &kb)
			return kb / 1024
		}
	}
	return -1
}

func main() {
	n := flag.Int("n", 256, "number of pods")
	trim := flag.Bool("trim", true, "enable cache-trim TransformFunc")
	core := flag.Bool("core", false, "measure ONLY Prism's own structures (allocator+sink+byUID), no informer/client-go cache")
	flag.Parse()

	if *core {
		// Isolate Prism's data structures: drive HandlePod directly with light
		// pods (no client-go, no informer cache). The pod objects are transient
		// and GC'd, so the residual heap is allocator + sink + byUID only.
		c := prismsync.NewController(nil, sink.NewCompactSink())
		base := heapInuse()
		for i := 0; i < *n; i++ {
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				UID:    types.UID(fmt.Sprintf("uid-%08d", i)),
				Labels: map[string]string{"app": fmt.Sprintf("svc%d", i), "tier": "backend"}}}
			_ = c.HandlePod(p, prismsync.EventAdd)
		}
		runtime.GC()
		runtime.GC()
		now := heapInuse()
		var grew uint64
		if now > base { // guard GC noise underflow at tiny n
			grew = now - base
		}
		fmt.Printf("RESULT core   n=%-7d sink=%-7d ownHeap=%6.2fMB perIdentity=%4.0fB rss=%6.1fMB\n",
			*n, c.Sink().Len(), float64(grew)/(1024*1024), float64(grew)/float64(max(*n, 1)), rssMB())
		return
	}

	client := fake.NewSimpleClientset()
	snk := sink.NewCompactSink()
	c := prismsync.NewController(client, snk)
	c.TrimCache = *trim

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	for !c.Synced() {
		time.Sleep(5 * time.Millisecond)
	}

	api := client.CoreV1().Pods("prod")
	for i := 0; i < *n; i++ {
		if _, err := api.Create(ctx, bloatedPod(i), metav1.CreateOptions{}); err != nil {
			panic(err)
		}
	}
	// Wait until every pod has propagated to the sink.
	deadline := time.Now().Add(30 * time.Second)
	for snk.Len() < *n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	runtime.GC()
	runtime.GC() // settle
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	mb := func(b uint64) float64 { return float64(b) / (1024 * 1024) }

	fmt.Printf("RESULT trim=%-5v n=%-5d sink=%-5d heapInuse=%6.1fMB heapAlloc=%6.1fMB sys=%6.1fMB rss=%6.1fMB perPodHeap=%5.0fB\n",
		*trim, *n, snk.Len(), mb(ms.HeapInuse), mb(ms.HeapAlloc), mb(ms.Sys), rssMB(),
		float64(ms.HeapInuse)/float64(max(*n, 1)))
}