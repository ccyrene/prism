// SPDX-License-Identifier: Apache-2.0

// Command prismd is the Prism identity-bus sync daemon. It watches Kubernetes
// Pods and propagates a stable numeric workload identity into the shared
// identity sink (the pinned BPF map on a real 6.12+ kernel, a userspace map in
// simulation), where any BPF consumer — sched_ext scheduler, tc/XDP net policy,
// observer — reads it. One identity, many subsystems: that shared map is the bus.
//
// Two modes:
//
//   - Real cluster: in-cluster config, or a kubeconfig via -kubeconfig / the
//     KUBECONFIG env / ~/.kube/config.
//   - Simulation (-sim): an empty fake clientset. The daemon comes up and stays
//     idle; useful for smoke-testing wiring on a host with no cluster. Driving
//     pods through the fake client is what the bench/test harness does by
//     calling the prismsync.Controller directly.
//
// On this 5.15 WSL2 host BPF map creation needs root + bpffs, so the sink
// factory logs the reason and falls back to the simulation sink automatically;
// the daemon remains fully functional.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ccyrene/prism/pkg/key"
	"github.com/ccyrene/prism/pkg/metrics"
	"github.com/ccyrene/prism/pkg/sink"
	prismsync "github.com/ccyrene/prism/pkg/sync"
)

// version is overridable via -ldflags "-X main.version=v1.2.3"; otherwise we
// fall back to the Go build info (module version / VCS revision).
var version = "dev"

func main() {
	log.SetPrefix("prismd: ")
	log.SetFlags(log.Ltime | log.Lmsgprefix)

	var (
		kubeconfig  = flag.String("kubeconfig", "", "path to kubeconfig (default: in-cluster, then KUBECONFIG, then ~/.kube/config)")
		preferBPF   = flag.Bool("bpf", true, "prefer the kernel BPF sink; falls back to the simulation sink if unavailable")
		simMode     = flag.Bool("sim", false, "run against an empty in-memory fake cluster instead of a real one")
		nodeName    = flag.String("node", os.Getenv("NODE_NAME"), "scope to this node's pods (DaemonSet mode); default $NODE_NAME")
		keyer       = flag.String("keyer", "cgroup", "bus key strategy: cgroup (real-kernel parity, inode of pod cgroup) | uid (hash of pod UID)")
		cgroupRoot  = flag.String("cgroup-root", "/sys/fs/cgroup", "cgroup-v2 mount root (cgroup keyer)")
		cgroupDrv   = flag.String("cgroup-driver", "systemd", "kubelet cgroup driver: systemd | cgroupfs")
		metricsAddr = flag.String("metrics-addr", ":9464", "address for /metrics, /healthz, /readyz (empty disables)")
		showVer     = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("prismd %s\n", buildVersion())
		return
	}

	// Validate enum-ish flags up front (warn + safe default rather than fail).
	if *keyer != "cgroup" && *keyer != "uid" {
		log.Printf("warning: unknown -keyer=%q, defaulting to cgroup", *keyer)
		*keyer = "cgroup"
	}
	if *cgroupDrv != "systemd" && *cgroupDrv != "cgroupfs" {
		log.Printf("warning: unknown -cgroup-driver=%q, defaulting to systemd", *cgroupDrv)
		*cgroupDrv = "systemd"
	}
	log.Printf("prismd %s starting", buildVersion())

	// Sink: kernel BPF map when possible, userspace map otherwise. New never
	// fails on this path because the sim fallback always succeeds.
	snk, err := sink.New(*preferBPF)
	if err != nil {
		log.Fatalf("sink init: %v", err)
	}
	defer snk.Close()
	log.Printf("identity sink ready: kind=%s", snk.Kind())

	// Clientset: fake (sim) or real (in-cluster / kubeconfig).
	client, err := buildClient(*simMode, *kubeconfig)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	m := metrics.New()
	ctrl := prismsync.NewController(client, snk)
	ctrl.Metrics = m
	ctrl.NodeName = *nodeName
	switch {
	case *simMode || *keyer == "uid":
		ctrl.Keyer = key.UIDKeyer{} // sim/bench: cgroup tree isn't this pod's
		log.Printf("bus keyer: uid")
	default:
		ctrl.Keyer = key.CgroupKeyer{Root: *cgroupRoot, Driver: key.ParseDriver(*cgroupDrv)}
		log.Printf("bus keyer: %s (root=%s)", ctrl.Keyer.Name(), *cgroupRoot)
	}
	if *nodeName != "" {
		log.Printf("node-scoped: %s", *nodeName)
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Operability surface: /metrics (Prometheus), /healthz (process is up),
	// /readyz (informer cache synced -> we're propagating). Runs in its own
	// goroutine and shuts down with the context. A failure here is logged, not
	// fatal — losing metrics must not take scheduling identity down.
	srv := startHTTPServer(ctx, *metricsAddr, m, ctrl)

	log.Printf("starting pod informer (sim=%v)", *simMode)
	if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("controller: %v", err)
	}
	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
	log.Printf("shutdown complete; %d identities were live in sink", snk.Len())
}

// startHTTPServer launches the metrics/health endpoints. Returns nil if addr is
// empty (disabled). Never fatal: a bind error is logged and the daemon keeps
// propagating identities.
func startHTTPServer(ctx context.Context, addr string, m *metrics.Metrics, ctrl *prismsync.Controller) *http.Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.MetricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ctrl.Synced() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("syncing"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("metrics/health on %s (/metrics /healthz /readyz)", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("warning: metrics server: %v (continuing without it)", err)
		}
	}()
	return srv
}

// buildVersion prefers the -ldflags value, else the Go build info.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", ""
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) >= 12 {
					rev = s.Value[:12]
				} else {
					rev = s.Value
				}
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		if rev != "" {
			return rev + dirty
		}
	}
	return version
}

// buildClient returns a fake clientset in sim mode, otherwise a real one from
// in-cluster config or the supplied/standard kubeconfig.
func buildClient(simMode bool, kubeconfig string) (kubernetes.Interface, error) {
	if simMode {
		// Empty fake cluster: the daemon idles until something drives pod
		// events through the controller (as the bench harness does).
		return fake.NewSimpleClientset(), nil
	}

	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// restConfig resolves a *rest.Config: in-cluster first, then an explicit
// kubeconfig path, then KUBECONFIG / ~/.kube/config.
func restConfig(kubeconfig string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		log.Printf("using in-cluster config")
		return cfg, nil
	}

	if kubeconfig == "" {
		if env := os.Getenv("KUBECONFIG"); env != "" {
			kubeconfig = env
		} else if home, err := os.UserHomeDir(); err == nil {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	log.Printf("using kubeconfig: %s", kubeconfig)
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
