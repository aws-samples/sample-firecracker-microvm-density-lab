// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package metrics is the host agent's Prometheus exporter.
//
// The architecture diagram shows the Host Agent as the per-node control point for the
// microVM fleet; an operator needs to see fleet state without shelling into the node.
// This package exposes the gauges, counters, and histograms that describe the warm pool,
// in-use microVMs, launch behaviour, per-NUMA-node distribution, and the discovered
// topology facts. It uses github.com/prometheus/client_golang, the standard client.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the registered collectors. Construct one with New and pass it to the
// orchestrator and warm pool so they can report state.
type Metrics struct {
	reg *prometheus.Registry

	WarmPoolSize   prometheus.Gauge
	WarmPoolTarget prometheus.Gauge
	InUse          prometheus.Gauge
	OnDemand       prometheus.Gauge

	LaunchesTotal  *prometheus.CounterVec
	LaunchFailures prometheus.Counter
	AcquireTotal   *prometheus.CounterVec
	RefillsTotal   prometheus.Counter

	LaunchLatency prometheus.Histogram

	// VMsByNode counts currently-running microVMs per NUMA node.
	VMsByNode *prometheus.GaugeVec

	// TopologyInfo carries the discovered host facts as label values on a constant-1
	// gauge, the standard "info metric" pattern.
	TopologyInfo *prometheus.GaugeVec
}

// New builds and registers all collectors on a private registry, so the agent's metrics
// are not entangled with the default global registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		reg: reg,
		WarmPoolSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "hostagent",
			Name:      "warm_pool_size",
			Help:      "Number of pre-booted microVMs currently idle in the warm pool.",
		}),
		WarmPoolTarget: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "hostagent",
			Name:      "warm_pool_target",
			Help:      "Configured target size of the warm pool.",
		}),
		InUse: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "hostagent",
			Name:      "microvms_in_use",
			Help:      "Number of microVMs currently handed out to a workload.",
		}),
		OnDemand: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "hostagent",
			Name:      "microvms_on_demand",
			Help:      "Number of in-use microVMs launched on demand beyond the warm pool.",
		}),
		LaunchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "hostagent",
			Name:      "launches_total",
			Help:      "Total microVM launches, labelled by source (warm or ondemand).",
		}, []string{"source"}),
		LaunchFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "hostagent",
			Name:      "launch_failures_total",
			Help:      "Total microVM launch attempts that failed.",
		}),
		AcquireTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "hostagent",
			Name:      "acquire_total",
			Help:      "Total Acquire calls, labelled by outcome (warm, ondemand, error).",
		}, []string{"outcome"}),
		RefillsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "hostagent",
			Name:      "warm_pool_refills_total",
			Help:      "Total asynchronous warm pool refill launches.",
		}),
		LaunchLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "hostagent",
			Name:      "launch_latency_seconds",
			Help:      "Wall-clock time from launch start to guest-agent-ready.",
			// microVM boot is sub-second to a few seconds; bucket accordingly.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}),
		VMsByNode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "hostagent",
			Name:      "microvms_by_node",
			Help:      "Running microVMs pinned to each NUMA node.",
		}, []string{"node"}),
		TopologyInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "hostagent",
			Name:      "topology_info",
			Help:      "Discovered host topology facts, exposed as labels on a constant 1.",
		}, []string{
			"instance_type", "numa_nodes", "physical_cores",
			"threads_per_core", "has_smt", "cache_mode", "config",
		}),
	}

	reg.MustRegister(
		m.WarmPoolSize, m.WarmPoolTarget, m.InUse, m.OnDemand,
		m.LaunchesTotal, m.LaunchFailures, m.AcquireTotal, m.RefillsTotal,
		m.LaunchLatency, m.VMsByNode, m.TopologyInfo,
	)

	return m
}

// Handler returns an http.Handler serving the exposition format on this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// SetTopologyInfo records the discovered host facts on the info metric. Call once at
// startup after topology discovery.
func (m *Metrics) SetTopologyInfo(instanceType, config string, numaNodes, physicalCores, threadsPerCore int, hasSMT bool, cacheMode string) {
	m.TopologyInfo.WithLabelValues(
		instanceType,
		strconv.Itoa(numaNodes),
		strconv.Itoa(physicalCores),
		strconv.Itoa(threadsPerCore),
		strconv.FormatBool(hasSMT),
		cacheMode,
		config,
	).Set(1)
}

// SetVMsByNode replaces the per-node running counts. Passing the full map each time keeps
// the gauge consistent with reality rather than drifting on increments.
func (m *Metrics) SetVMsByNode(counts map[int]int) {
	m.VMsByNode.Reset()
	for node, n := range counts {
		label := strconv.Itoa(node)
		if node < 0 {
			label = "unpinned"
		}
		m.VMsByNode.WithLabelValues(label).Set(float64(n))
	}
}
