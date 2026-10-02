// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package topology

import (
	"path/filepath"
	"strconv"
	"testing"
)

// TestDiscoverAgainstCapturedM8i replays the exact sysfs values recorded from a live
// m8i.metal-48xl, rather than a synthesised approximation. If Discover misreads real
// kernel output, this is the test that catches it.
//
// Captured 2026-07-30 from Amazon Linux 2023, kernel 6.1.176-223.369.amzn2023:
//
//	CPU(s):              192
//	Core(s) per socket:  96
//	Socket(s):           1
//	NUMA node(s):        3
//	NUMA node0 CPU(s):   0-31,96-127
//	NUMA node1 CPU(s):   32-63,128-159
//	NUMA node2 CPU(s):   64-95,160-191
func TestDiscoverAgainstCapturedM8i(t *testing.T) {
	root := t.TempDir()

	// Node membership exactly as reported by lscpu on the captured host.
	nodeRanges := map[int][2][2]int{
		0: {{0, 31}, {96, 127}},
		1: {{32, 63}, {128, 159}},
		2: {{64, 95}, {160, 191}},
	}
	nodeOfCPU := map[int]int{}
	for node, ranges := range nodeRanges {
		for _, r := range ranges {
			for cpu := r[0]; cpu <= r[1]; cpu++ {
				nodeOfCPU[cpu] = node
			}
		}
	}
	if len(nodeOfCPU) != 192 {
		t.Fatalf("fixture covers %d CPUs, want 192", len(nodeOfCPU))
	}

	mustWrite(t, filepath.Join(root, "present"), "0-191")
	mustWrite(t, filepath.Join(root, "smt/control"), "on")

	for cpu := 0; cpu < 192; cpu++ {
		// Observed pattern: logical CPU n and n+96 share physical core n%96.
		core := cpu % 96
		node := nodeOfCPU[cpu]

		dir := filepath.Join(root, "cpu"+strconv.Itoa(cpu))
		mustWrite(t, filepath.Join(dir, "topology/core_id"), strconv.Itoa(core))
		mustWrite(t, filepath.Join(dir, "topology/physical_package_id"), "0")
		mustWrite(t, filepath.Join(dir, "topology/thread_siblings_list"), strconv.Itoa(core)+","+strconv.Itoa(core+96))
		mustWrite(t, filepath.Join(dir, "node"+strconv.Itoa(node)+"/.keep"), "")

		// Captured verbatim: ONE L3 instance of 480 MiB shared by all 192 CPUs.
		// lscpu reported "L3 cache: 480 MiB (1 instance)" and every CPU's
		// shared_cpu_list read "0-191". The single cache domain therefore spans all
		// three NUMA nodes.
		mustWrite(t, filepath.Join(dir, "cache/index3/level"), "3")
		mustWrite(t, filepath.Join(dir, "cache/index3/shared_cpu_list"), "0-191")
		mustWrite(t, filepath.Join(dir, "cache/index3/size"), "491520K")
	}

	withFixture(t, root)

	top, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// The headline numbers must match what lscpu reported on the real host.
	if got := len(top.CPUs); got != 192 {
		t.Errorf("logical CPUs = %d, want 192", got)
	}
	if got := top.PhysicalCores(); got != 96 {
		t.Errorf("PhysicalCores() = %d, want 96 (lscpu: Core(s) per socket 96)", got)
	}
	if got := top.ThreadsPerCore(); got != 2 {
		t.Errorf("ThreadsPerCore() = %d, want 2", got)
	}
	if got := len(top.Nodes); got != 3 {
		t.Errorf("NUMA nodes = %d, want 3", got)
	}

	// Node 0 must contain exactly the CPUs lscpu listed.
	want0 := map[int]bool{}
	for cpu := 0; cpu <= 31; cpu++ {
		want0[cpu] = true
	}
	for cpu := 96; cpu <= 127; cpu++ {
		want0[cpu] = true
	}
	got0 := top.CPUsInNode(0)
	if len(got0) != len(want0) {
		t.Errorf("CPUsInNode(0) = %d CPUs, want %d", len(got0), len(want0))
	}
	for _, cpu := range got0 {
		if !want0[cpu] {
			t.Errorf("CPUsInNode(0) contains %d, which lscpu put in another node", cpu)
		}
	}

	// 32 physical cores per node, which is the real allocation unit for placement.
	for _, n := range top.Nodes {
		if got := len(top.PrimarySiblingsInNode(n)); got != 32 {
			t.Errorf("PrimarySiblingsInNode(%d) = %d, want 32", n, got)
		}
	}

	// Sibling offset of 96, as observed.
	for _, cpu := range []int{0, 31, 32, 95} {
		sib, ok := top.SiblingOf(cpu)
		if !ok || sib != cpu+96 {
			t.Errorf("SiblingOf(%d) = (%d, %v), want (%d, true)", cpu, sib, ok, cpu+96)
		}
	}

	// The captured host has exactly one L3 domain covering all 192 CPUs, so it spans
	// every NUMA node. This is the measurement that classifies the machine.
	if got := len(top.L3Domains); got != 1 {
		t.Errorf("L3 domains = %d, want 1 (lscpu: L3 cache 480 MiB, 1 instance)", got)
	}
	if !top.L3SpansNodes() {
		t.Error("L3SpansNodes() = false, but the captured host has one cache domain over 3 nodes")
	}
	if got := top.CacheMode(); got != CacheUnified {
		t.Errorf("CacheMode() = %q, want %q", got, CacheUnified)
	}

	// L3PerNode reports 1 per node here, which is why it cannot be read alone: the same
	// value would appear on a genuine one-cache-per-node machine. L3SpansNodes is what
	// separates the two cases.
	l3 := top.L3PerNode()
	for _, n := range top.Nodes {
		if l3[n] != 1 {
			t.Errorf("L3PerNode()[%d] = %d, want 1", n, l3[n])
		}
	}
}

// A machine where each node owns its own cache domain must classify differently from the
// captured m8i, otherwise the discriminator is useless.
func TestCacheModePerNode(t *testing.T) {
	withFixture(t, writeFixture(t, 96, 2, 3, 1))
	top, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if top.L3SpansNodes() {
		t.Error("L3SpansNodes() = true on a per-node-cache fixture")
	}
	if got := top.CacheMode(); got != CachePerNode {
		t.Errorf("CacheMode() = %q, want %q", got, CachePerNode)
	}
}
