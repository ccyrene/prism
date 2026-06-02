// SPDX-License-Identifier: Apache-2.0

// Command livedemo proves the Prism control plane end-to-end on any host (no
// root, no real cluster, no 6.12 kernel): it runs the REAL client-go informer
// over a fake clientset, creates actual Pod objects through the Kubernetes API,
// and shows their identities propagate into the sink — including replica dedup,
// relabel re-allocation, and delete release.
//
//	go run ./examples/livedemo
package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	prismsync "github.com/ccyrene/prism/pkg/sync"
	"github.com/ccyrene/prism/pkg/sink"
)

func pod(name, uid string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "shop", UID: types.UID(uid), Labels: labels,
	}}
}

func main() {
	client := fake.NewSimpleClientset()
	s := sink.NewSimSink()
	ctrl := prismsync.NewController(client, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ctrl.Run(ctx) }() // REAL informer: list-watch -> handlers -> sink
	time.Sleep(300 * time.Millisecond)

	api := client.CoreV1().Pods("shop")
	mustCreate := func(p *corev1.Pod) {
		if _, err := api.Create(ctx, p, metav1.CreateOptions{}); err != nil {
			panic(err)
		}
	}

	fmt.Println("→ creating real Pod objects through the k8s API (informer watches them)...")
	// Two replicas of the same Deployment: identical security-relevant labels,
	// different volatile pod-template-hash. They MUST share one identity.
	mustCreate(pod("checkout-aaa", "uid-checkout-1", map[string]string{
		"app": "checkout", "tier": "backend", "pod-template-hash": "5f7c"}))
	mustCreate(pod("checkout-bbb", "uid-checkout-2", map[string]string{
		"app": "checkout", "tier": "backend", "pod-template-hash": "9a2e"}))
	// A different workload.
	mustCreate(pod("frontend-xyz", "uid-frontend-1", map[string]string{
		"app": "frontend", "tier": "web"}))
	// One that we will relabel, then one we will delete.
	mustCreate(pod("cache-1", "uid-cache-1", map[string]string{"app": "cache"}))
	mustCreate(pod("temp-1", "uid-temp-1", map[string]string{"app": "temp"}))

	time.Sleep(400 * time.Millisecond)
	dump(s, ctrl, "after 5 creates (checkout x2 should SHARE one identity)")

	fmt.Println("\n→ relabel cache-1 app=cache → app=redis (identity must change)...")
	p, _ := api.Get(ctx, "cache-1", metav1.GetOptions{})
	p.Labels["app"] = "redis"
	api.Update(ctx, p, metav1.UpdateOptions{})

	fmt.Println("→ delete temp-1 (identity must be released)...")
	api.Delete(ctx, "temp-1", metav1.DeleteOptions{})

	time.Sleep(400 * time.Millisecond)
	dump(s, ctrl, "after relabel + delete")

	fmt.Printf("\nlive identities in allocator: %d   sink kind: %q   sink entries: %d\n",
		ctrl.Allocator().Len(), s.Kind(), s.Len())
}

func dump(s *sink.SimSink, ctrl *prismsync.Controller, title string) {
	fmt.Printf("\n--- bus state: %s ---\n", title)
	fmt.Printf("%-16s %-14s %-10s %-18s\n", "POD UID", "WORKLOAD KEY", "IDENTITY", "LABEL_HASH")
	for _, uid := range []string{"uid-checkout-1", "uid-checkout-2", "uid-frontend-1", "uid-cache-1", "uid-temp-1"} {
		k := prismsync.WorkloadKey(types.UID(uid))
		v, ok, _ := s.Lookup(k)
		if !ok {
			fmt.Printf("%-16s %-14s %-10s %s\n", uid, fmt.Sprintf("%#x", k), "-", "(released/absent)")
			continue
		}
		fmt.Printf("%-16s %#-12x %-10d %#018x\n", uid, k, v.Identity, v.LabelHash)
	}
}
