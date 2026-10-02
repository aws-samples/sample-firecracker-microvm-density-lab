// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package hostinfo wraps lab/topology discovery for the host agent's startup path.
//
// This is the first of the four duties the architecture diagram assigns the Host Agent:
// discover the CPU topology of the metal node it runs on. The agent cannot assume a layout
// from the instance type, because the number of NUMA nodes on a single socket depends on
// firmware settings and SMT sibling numbering is not portable. Everything downstream
// (placement, cgroups, memory sizing) reads from what this package discovers.
package hostinfo

import (
	"fmt"
	"log/slog"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
)

// Host is the discovered topology plus a few derived facts the agent logs and exports.
type Host struct {
	Topology       *topology.Topology
	NodeCount      int
	PhysicalCores  int
	LogicalCPUs    int
	ThreadsPerCore int
	HasSMT         bool
	CacheMode      topology.CacheMode
	L3SpansNodes   bool
	SMTState       string
}

// Discover reads the current host's topology via lab/topology.Discover and derives the
// summary facts. It does not log; call Log to emit a startup summary.
func Discover() (*Host, error) {
	top, err := topology.Discover()
	if err != nil {
		return nil, fmt.Errorf("discover host topology: %w", err)
	}
	return &Host{
		Topology:       top,
		NodeCount:      len(top.Nodes),
		PhysicalCores:  top.PhysicalCores(),
		LogicalCPUs:    len(top.CPUs),
		ThreadsPerCore: top.ThreadsPerCore(),
		HasSMT:         top.HasSMT(),
		CacheMode:      top.CacheMode(),
		L3SpansNodes:   top.L3SpansNodes(),
		SMTState:       top.SMTState,
	}, nil
}

// Log emits a one-line-per-fact startup summary. The diagram's first Host Agent duty is
// "discover CPU topology"; this is the operator-visible evidence that it happened.
func (h *Host) Log(logger *slog.Logger) {
	logger.Info("host topology discovered",
		"numa_nodes", h.NodeCount,
		"physical_cores", h.PhysicalCores,
		"logical_cpus", h.LogicalCPUs,
		"threads_per_core", h.ThreadsPerCore,
		"has_smt", h.HasSMT,
		"smt_state", h.SMTState,
		"cache_mode", string(h.CacheMode),
		"l3_spans_nodes", h.L3SpansNodes,
		"numa_node_ids", h.Topology.Nodes,
	)
}
