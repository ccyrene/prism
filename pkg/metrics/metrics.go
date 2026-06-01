// SPDX-License-Identifier: Apache-2.0

// Package metrics is Prism's operability surface: Prometheus counters/gauges/
// histograms plus the HTTP handlers a per-node DaemonSet exposes. Every method
// is nil-receiver-safe, so the controller can be driven in tests and benchmarks
// with a nil *Metrics and pay nothing — only the daemon wires a real instance.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Prism's collectors registered against one registry.
type Metrics struct {
	reg               *prometheus.Registry
	podsProcessed     *prometheus.CounterVec // by event: add|update|delete
	identitiesAlloc   prometheus.Counter
	allocFailures     prometheus.Counter
	sinkErrors        *prometheus.CounterVec // by op: upsert|delete
	handlerPanics     prometheus.Counter
	liveIdentities    prometheus.Gauge
	propagationSecond prometheus.Histogram
}

// New builds and registers Prism's metrics on a fresh registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		podsProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_pods_processed_total",
			Help: "Pod events propagated to the identity bus, by event type.",
		}, []string{"event"}),
		identitiesAlloc: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "prism_identities_allocated_total",
			Help: "Numeric identities minted (new label sets).",
		}),
		allocFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "prism_alloc_failures_total",
			Help: "Identity allocation failures (e.g. 24-bit space exhausted).",
		}),
		sinkErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prism_sink_errors_total",
			Help: "Sink write failures, by operation.",
		}, []string{"op"}),
		handlerPanics: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "prism_handler_panics_total",
			Help: "Panics recovered in the pod-event handler (daemon survived).",
		}),
		liveIdentities: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "prism_live_identities",
			Help: "Distinct identities currently live in the allocator.",
		}),
		propagationSecond: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "prism_propagation_seconds",
			Help:    "Per-pod control-plane propagation time (handler entry to sink write).",
			Buckets: prometheus.ExponentialBuckets(1e-7, 4, 10), // 100ns .. ~26ms
		}),
	}
	reg.MustRegister(m.podsProcessed, m.identitiesAlloc, m.allocFailures,
		m.sinkErrors, m.handlerPanics, m.liveIdentities, m.propagationSecond)
	return m
}

// MetricsHandler serves the Prometheus exposition for this instance.
func (m *Metrics) MetricsHandler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

func (m *Metrics) PodProcessed(event string) {
	if m == nil {
		return
	}
	m.podsProcessed.WithLabelValues(event).Inc()
}

func (m *Metrics) IdentityAllocated() {
	if m == nil {
		return
	}
	m.identitiesAlloc.Inc()
}

func (m *Metrics) AllocFailure() {
	if m == nil {
		return
	}
	m.allocFailures.Inc()
}

func (m *Metrics) SinkError(op string) {
	if m == nil {
		return
	}
	m.sinkErrors.WithLabelValues(op).Inc()
}

func (m *Metrics) HandlerPanic() {
	if m == nil {
		return
	}
	m.handlerPanics.Inc()
}

func (m *Metrics) SetLive(n int) {
	if m == nil {
		return
	}
	m.liveIdentities.Set(float64(n))
}

func (m *Metrics) ObservePropagation(seconds float64) {
	if m == nil {
		return
	}
	m.propagationSecond.Observe(seconds)
}
