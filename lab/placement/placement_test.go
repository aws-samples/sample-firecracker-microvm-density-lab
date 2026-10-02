// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package placement

import (
	"errors"
	"testing"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
)

// m8iTopology builds the shape measured on a real m8i.metal-48xl: 96 physical cores,
// 2 threads each, 3 NUMA nodes on one socket, SMT siblings offset by 96.
func m8iTopology() *topology.Topology {
	return synth(96, 2, 3)
}

// m8aTopology builds the m8a.metal-48xl shape: 192 physical cores, one thread each.
func m8aTopology() *topology.Topology {
	return synth(192, 1, 4)
}

func synth(cores, threads, nodes int) *topology.Topology {
	t := &topology.Topology{}
	coresPerNode := cores / nodes
	total := cores * threads

	for cpu := 0; cpu < total; cpu++ {
		core := cpu % cores
		node := core / coresPerNode
		sibs := []int{core}
		if threads > 1 {
			sibs = []int{core, core + cores}
		}
		t.CPUs = append(t.CPUs, topology.CPU{
			ID: cpu, Core: core, Socket: 0, Node: node, Siblings: sibs, L3: node,
		})
	}
	for n := 0; n < nodes; n++ {
		t.Nodes = append(t.Nodes, n)
	}
	t.SMTState = "on"
	if threads == 1 {
		t.SMTState = "notsupported"
	}
	return t
}

func TestRequiresSMT(t *testing.T) {
	smtOnly := map[Config]bool{ConfigC: true, ConfigE: true, ConfigF: true}
	for _, c := range AllConfigs {
		if got, want := c.RequiresSMT(), smtOnly[c]; got != want {
			t.Errorf("Config %s RequiresSMT() = %v, want %v", c, got, want)
		}
	}
}

// The central guard: configs C, E, and F must refuse to run on a host with no SMT
// rather than silently producing data that measures nothing.
func TestSMTConfigsRejectedOnNoSMTHost(t *testing.T) {
	top := m8aTopology()
	if top.HasSMT() {
		t.Fatal("fixture should have no SMT")
	}
	for _, c := range []Config{ConfigC, ConfigE, ConfigF} {
		if _, err := Build(top, c, 100, DefaultOptions()); !errors.Is(err, ErrSMTRequired) {
			t.Errorf("Build(%s) on no-SMT host: err = %v, want ErrSMTRequired", c, err)
		}
	}
	// A and B are vendor-neutral and must still work.
	for _, c := range []Config{ConfigA, ConfigB, ConfigD} {
		if _, err := Build(top, c, 100, DefaultOptions()); err != nil {
			t.Errorf("Build(%s) on no-SMT host: unexpected error %v", c, err)
		}
	}
}

func TestConfigAIsUnpinned(t *testing.T) {
	p, err := Build(m8iTopology(), ConfigA, 120, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.VMs) != 120 {
		t.Errorf("planned %d VMs, want 120", len(p.VMs))
	}
	for _, vm := range p.VMs {
		if vm.VCPUSet != "" {
			t.Errorf("vm %d has cpuset %q under unpinned config, want empty", vm.Index, vm.VCPUSet)
		}
	}
	if err := p.Validate(m8iTopology()); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Config B gives each microVM one physical core, so the ceiling is the number of
// allocatable cores. Requesting more must be reported, not silently oversubscribed.
func TestConfigBCeilingIsReported(t *testing.T) {
	top := m8iTopology()
	opts := DefaultOptions()

	// 96 cores, minus 2 reserve per node across 3 nodes = 6, leaves 90.
	p, err := Build(top, ConfigB, 180, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.VMs) >= 180 {
		t.Errorf("planned %d VMs at requested 180; strict pinning cannot exceed core count", len(p.VMs))
	}
	if len(p.Notes) == 0 {
		t.Error("exceeding the ceiling produced no note; the shortfall must be visible in results")
	}
	if err := p.Validate(top); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// No two microVMs may share a CPU except under the oversubscribed config.
func TestNoSharedCPUsExceptConfigE(t *testing.T) {
	top := m8iTopology()
	for _, c := range []Config{ConfigB, ConfigC, ConfigD} {
		p, err := Build(top, c, 60, DefaultOptions())
		if err != nil {
			t.Fatalf("Build(%s): %v", c, err)
		}
		seen := map[int]bool{}
		for _, vm := range p.VMs {
			cpus, err := topology.ParseCPUList(vm.VCPUSet)
			if err != nil {
				t.Fatal(err)
			}
			for _, cpu := range cpus {
				if seen[cpu] {
					t.Errorf("config %s: cpu %d assigned to more than one microVM", c, cpu)
				}
				seen[cpu] = true
			}
		}
		if err := p.Validate(top); err != nil {
			t.Errorf("config %s Validate: %v", c, err)
		}
	}
}

// Config C must put the vCPU and VMM on different threads of the same physical core.
func TestConfigCUsesSiblingPairs(t *testing.T) {
	top := m8iTopology()
	p, err := Build(top, ConfigC, 30, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range p.VMs {
		vcpu, _ := topology.ParseCPUList(vm.VCPUSet)
		vmm, _ := topology.ParseCPUList(vm.VMMSet)
		if len(vcpu) != 1 || len(vmm) != 1 {
			t.Fatalf("vm %d: want one vcpu and one vmm cpu, got %v and %v", vm.Index, vcpu, vmm)
		}
		sib, ok := top.SiblingOf(vcpu[0])
		if !ok || sib != vmm[0] {
			t.Errorf("vm %d: vmm cpu %d is not the sibling of vcpu %d (sibling is %d)",
				vm.Index, vmm[0], vcpu[0], sib)
		}
	}
}

// Config E is the only config permitted to place two microVMs on one physical core.
func TestConfigEOversubscribes(t *testing.T) {
	top := m8iTopology()
	opts := DefaultOptions()

	strict, err := Build(top, ConfigB, 1000, opts)
	if err != nil {
		t.Fatal(err)
	}
	over, err := Build(top, ConfigE, 1000, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(over.VMs) != 2*len(strict.VMs) {
		t.Errorf("ConfigE capacity = %d, want twice ConfigB's %d", len(over.VMs), len(strict.VMs))
	}
	if err := over.Validate(top); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Configs D and F confine VMM threads to a shared pool, and that pool must not overlap
// the CPUs handed to vCPU threads.
func TestVMMPoolDisjointFromVCPUs(t *testing.T) {
	top := m8iTopology()
	for _, c := range []Config{ConfigD, ConfigF} {
		p, err := Build(top, c, 60, DefaultOptions())
		if err != nil {
			t.Fatalf("Build(%s): %v", c, err)
		}
		if p.VMMPool == "" {
			t.Fatalf("config %s: no VMM pool allocated", c)
		}
		pool, err := topology.ParseCPUList(p.VMMPool)
		if err != nil {
			t.Fatal(err)
		}
		inPool := map[int]bool{}
		for _, cpu := range pool {
			inPool[cpu] = true
		}
		for _, vm := range p.VMs {
			cpus, _ := topology.ParseCPUList(vm.VCPUSet)
			for _, cpu := range cpus {
				if inPool[cpu] {
					t.Errorf("config %s: vm %d vcpu %d overlaps the VMM pool", c, vm.Index, cpu)
				}
			}
		}
	}
}

// A microVM confined to the wrong node would defeat the purpose of NUMA-aware placement,
// so every assignment must sit inside its declared node.
func TestVMsDoNotStraddleNUMANodes(t *testing.T) {
	top := m8iTopology()
	for _, c := range []Config{ConfigB, ConfigC, ConfigD, ConfigE} {
		p, err := Build(top, c, 90, DefaultOptions())
		if err != nil {
			t.Fatalf("Build(%s): %v", c, err)
		}
		for _, vm := range p.VMs {
			nodeCPUs := map[int]bool{}
			for _, id := range top.CPUsInNode(vm.Node) {
				nodeCPUs[id] = true
			}
			for _, set := range []string{vm.VCPUSet, vm.VMMSet} {
				cpus, _ := topology.ParseCPUList(set)
				for _, cpu := range cpus {
					if !nodeCPUs[cpu] {
						t.Errorf("config %s vm %d: cpu %d outside assigned node %d",
							c, vm.Index, cpu, vm.Node)
					}
				}
			}
		}
		if err := p.Validate(top); err != nil {
			t.Errorf("config %s Validate: %v", c, err)
		}
	}
}

// Low-density runs must spread across nodes. Filling node 0 first would measure one
// node's behaviour and call it the host's.
func TestLowDensitySpreadsAcrossNodes(t *testing.T) {
	top := m8iTopology()
	p, err := Build(top, ConfigB, 6, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[int]int{}
	for _, vm := range p.VMs {
		nodes[vm.Node]++
	}
	if len(nodes) != 3 {
		t.Errorf("6 microVMs landed on %d nodes, want 3 (round-robin)", len(nodes))
	}
	for n, count := range nodes {
		if count != 2 {
			t.Errorf("node %d got %d microVMs, want 2 for an even spread", n, count)
		}
	}
}

func TestHostReserveNeverAssigned(t *testing.T) {
	top := m8iTopology()
	for _, c := range []Config{ConfigB, ConfigC, ConfigD, ConfigE} {
		p, err := Build(top, c, 1000, DefaultOptions()) // saturate
		if err != nil {
			t.Fatalf("Build(%s): %v", c, err)
		}
		if err := p.Validate(top); err != nil {
			t.Errorf("config %s: %v", c, err)
		}
	}
}

func TestParentCPUSetCoversAssignments(t *testing.T) {
	top := m8iTopology()
	p, err := Build(top, ConfigD, 60, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := topology.ParseCPUList(p.ParentCPUSet)
	if err != nil {
		t.Fatal(err)
	}
	inParent := map[int]bool{}
	for _, cpu := range parent {
		inParent[cpu] = true
	}
	for _, vm := range p.VMs {
		for _, set := range []string{vm.VCPUSet, vm.VMMSet} {
			cpus, _ := topology.ParseCPUList(set)
			for _, cpu := range cpus {
				if !inParent[cpu] {
					t.Errorf("vm %d cpu %d missing from ParentCPUSet; the parent cgroup would not own it",
						vm.Index, cpu)
				}
			}
		}
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	top := m8iTopology()
	if _, err := Build(nil, ConfigB, 10, DefaultOptions()); err == nil {
		t.Error("Build(nil topology) = nil error, want error")
	}
	for _, d := range []int{0, -1} {
		if _, err := Build(top, ConfigB, d, DefaultOptions()); err == nil {
			t.Errorf("Build(density=%d) = nil error, want error", d)
		}
	}
}

// A host too small to host anything after reserves must say so.
func TestNoCapacityIsAnError(t *testing.T) {
	tiny := synth(2, 1, 1)
	_, err := Build(tiny, ConfigD, 1, Options{VMMPoolCores: 8, HostReserveCores: 4})
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

// Validate must catch a hand-corrupted plan, otherwise it provides no protection.
func TestValidateCatchesSharedCPU(t *testing.T) {
	top := m8iTopology()
	p := &Plan{
		Config: ConfigB,
		VMs: []VM{
			{Index: 0, Node: 0, VCPUSet: "5"},
			{Index: 1, Node: 0, VCPUSet: "5"}, // duplicate
		},
	}
	if err := p.Validate(top); err == nil {
		t.Error("Validate accepted two microVMs sharing cpu 5 under ConfigB")
	}
}

func TestValidateCatchesCrossNodeAssignment(t *testing.T) {
	top := m8iTopology()
	// CPU 40 is in node 1 on this layout, so claiming node 0 is wrong.
	p := &Plan{
		Config: ConfigB,
		VMs:    []VM{{Index: 0, Node: 0, VCPUSet: "40"}},
	}
	if err := p.Validate(top); err == nil {
		t.Error("Validate accepted a microVM whose cpu is outside its declared node")
	}
}
