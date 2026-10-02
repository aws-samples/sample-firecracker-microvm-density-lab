// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package placement turns a discovered topology into per-microVM CPU assignments.
//
// This is the core of the density experiment. Each Config expresses one hypothesis about
// how to lay microVMs onto a host, and Build produces the concrete cpuset values the lab
// applies and the production placement agent will later apply too.
//
// Two constraints shape the design:
//
// A microVM should not straddle a NUMA node. Crossing nodes means guest memory accesses
// can land on remote memory, which is the effect NUMA-aware placement exists to avoid.
// Every assignment here keeps a microVM's vCPU and its memory node together.
//
// The cpuset partition flag is owned by the parent cgroup and is not delegatable, so the
// agent must create the parent hierarchy and set partition state itself. The jailer's
// --cgroup flag can write cpuset.cpus for a microVM but cannot make that cgroup a
// partition root. Build therefore emits both the per-VM cpuset and the parent-level
// exclusive CPU set the caller needs to establish first.
package placement

import (
	"errors"
	"fmt"
	"sort"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
)

// Config identifies one placement strategy under test.
type Config string

const (
	// ConfigA is the unpinned baseline: no cpuset, the host scheduler decides.
	ConfigA Config = "A"
	// ConfigB gives each microVM one full physical core. On an SMT host the sibling
	// thread is left idle, which trades density for freedom from sibling interference.
	ConfigB Config = "B"
	// ConfigC splits an SMT pair: the vCPU thread on one sibling, the VMM thread on the
	// other. Requires SMT.
	ConfigC Config = "C"
	// ConfigD pins vCPUs one per core and confines all VMM threads to a small shared
	// pool, on the basis that VMM threads are near-idle for compute-bound work.
	ConfigD Config = "D"
	// ConfigE oversubscribes: two vCPU threads per physical core. Requires SMT.
	ConfigE Config = "E"
	// ConfigF runs with SMT disabled at the host level, one vCPU per core, VMM pool.
	// Requires that SMT be togglable, so it is not runnable where SMT is absent.
	ConfigF Config = "F"
)

// AllConfigs is the sweep order used by the lab.
var AllConfigs = []Config{ConfigA, ConfigB, ConfigC, ConfigD, ConfigE, ConfigF}

// RequiresSMT reports whether a config can only run on an SMT-capable host.
//
// This matters because AMD EC2 metal instances expose one thread per core, so configs
// C, E, and F have nothing to act on there. Attempting them would silently produce
// meaningless data rather than an error, which is why callers should check.
func (c Config) RequiresSMT() bool {
	switch c {
	case ConfigC, ConfigE, ConfigF:
		return true
	default:
		return false
	}
}

// Description returns a short human-readable summary, used in reports.
func (c Config) Description() string {
	switch c {
	case ConfigA:
		return "unpinned baseline (host scheduler decides)"
	case ConfigB:
		return "one physical core per microVM, SMT siblings idle"
	case ConfigC:
		return "SMT split: vCPU on one sibling, VMM on the other"
	case ConfigD:
		return "vCPU pinned per core, VMM threads on a shared pool"
	case ConfigE:
		return "oversubscribed: two vCPU threads per physical core"
	case ConfigF:
		return "SMT disabled at host level, one vCPU per core, VMM pool"
	default:
		return "unknown"
	}
}

// VM is the CPU assignment for a single microVM.
type VM struct {
	Index int // 0-based position in the plan
	Node  int // NUMA node, which is also cpuset.mems

	// VCPUSet is written to cpuset.cpus for the microVM's cgroup. Empty means
	// unconstrained, used by the unpinned baseline.
	VCPUSet string
	// VMMSet is where the VMM and API threads belong, when the config separates them
	// from the vCPU thread. Empty means the VMM shares VCPUSet.
	VMMSet string
}

// Plan is a complete placement for one config at one density level.
type Plan struct {
	Config  Config
	Density int
	VMs     []VM

	// VMMPool is the shared VMM pool, for configs that use one. Callers should exclude
	// these CPUs from vCPU assignment.
	VMMPool string
	// HostReserve is held back for the host OS and the agent itself, never assigned to
	// a microVM.
	HostReserve string
	// ParentCPUSet is the union of all CPUs the plan uses. The caller writes this to
	// the parent cgroup's cpuset.cpus and cpuset.cpus.exclusive before creating child
	// cgroups, because the partition flag is not delegatable.
	ParentCPUSet string

	// Notes records anything a reader of the results needs to know, for instance that
	// the requested density exceeded what strict pinning allows.
	Notes []string
}

// Options tunes a plan.
type Options struct {
	// VMMPoolCores is the number of physical cores dedicated to the shared VMM pool in
	// configs D and F. The lab plan uses 8.
	VMMPoolCores int
	// HostReserveCores is held back for the host OS and agent.
	HostReserveCores int
}

// DefaultOptions matches the three-tier allocation in the analysis: vCPU cores, a small
// VMM pool, and a host reserve.
func DefaultOptions() Options {
	return Options{VMMPoolCores: 8, HostReserveCores: 4}
}

var (
	// ErrSMTRequired is returned when a config needs SMT and the host has none.
	ErrSMTRequired = errors.New("config requires SMT but host reports one thread per core")
	// ErrNoCapacity is returned when even one microVM cannot be placed.
	ErrNoCapacity = errors.New("no capacity for any microVM after reserves")
)

// Build computes CPU assignments for a config at a target density.
//
// A returned plan may contain fewer VMs than requested: strict pinning has a hard
// ceiling of one microVM per physical core, and reporting the shortfall in Notes is more
// useful than either failing or silently oversubscribing, since the ceiling itself is a
// result the lab wants to record.
func Build(top *topology.Topology, cfg Config, density int, opts Options) (*Plan, error) {
	if top == nil {
		return nil, errors.New("nil topology")
	}
	if density <= 0 {
		return nil, fmt.Errorf("density must be positive, got %d", density)
	}
	if cfg.RequiresSMT() && !top.HasSMT() {
		return nil, fmt.Errorf("%w: config %s", ErrSMTRequired, cfg)
	}

	p := &Plan{Config: cfg, Density: density}

	if cfg == ConfigA {
		// Unpinned: every microVM is unconstrained. Still record the node as -1 so
		// downstream reporting can distinguish "no node" from "node 0".
		for i := 0; i < density; i++ {
			p.VMs = append(p.VMs, VM{Index: i, Node: -1})
		}
		p.Notes = append(p.Notes, "unpinned: no cpuset applied, host scheduler places all threads")
		var all []int
		for _, c := range top.CPUs {
			all = append(all, c.ID)
		}
		p.ParentCPUSet = topology.FormatCPUList(all)
		return p, nil
	}

	nodes := top.Nodes
	if len(nodes) == 0 {
		nodes = []int{-1} // host exposes no NUMA information
	}

	// Build the per-node pool of allocatable cores, after carving out the host reserve
	// and, where the config needs one, the shared VMM pool.
	type nodePool struct {
		node    int
		vcpu    []int // primary siblings available for vCPU threads
		vmmPool []int
	}

	reservePerNode := divCeil(opts.HostReserveCores, len(nodes))
	vmmPerNode := 0
	if cfg == ConfigD || cfg == ConfigF {
		vmmPerNode = divCeil(opts.VMMPoolCores, len(nodes))
	}

	var pools []nodePool
	var allUsed, vmmAll, reserveAll []int

	for _, n := range nodes {
		var cores []int
		if n == -1 {
			for _, c := range top.CPUs {
				if c.IsPrimarySibling() {
					cores = append(cores, c.ID)
				}
			}
			sort.Ints(cores)
		} else {
			cores = top.PrimarySiblingsInNode(n)
		}

		if len(cores) <= reservePerNode+vmmPerNode {
			// This node cannot host any microVM once reserves are taken.
			continue
		}

		// Reserve from the low end so the numbering of assigned cores stays readable.
		reserve := cores[:reservePerNode]
		rest := cores[reservePerNode:]

		var pool []int
		if vmmPerNode > 0 {
			pool = rest[:vmmPerNode]
			rest = rest[vmmPerNode:]
		}

		reserveAll = append(reserveAll, reserve...)
		vmmAll = append(vmmAll, pool...)
		pools = append(pools, nodePool{node: n, vcpu: rest, vmmPool: pool})
	}

	if len(pools) == 0 {
		return nil, ErrNoCapacity
	}

	p.HostReserve = topology.FormatCPUList(reserveAll)
	if len(vmmAll) > 0 {
		p.VMMPool = topology.FormatCPUList(vmmAll)
	}

	// Capacity per config. Only ConfigE packs two microVMs onto one physical core.
	perCore := 1
	if cfg == ConfigE {
		perCore = 2
	}

	capacity := 0
	for _, pl := range pools {
		capacity += len(pl.vcpu) * perCore
	}
	if capacity == 0 {
		return nil, ErrNoCapacity
	}

	target := density
	if target > capacity {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"requested density %d exceeds config %s ceiling of %d; planned %d",
			density, cfg, capacity, capacity))
		target = capacity
	}

	// Distribute round-robin across nodes so nodes fill evenly. Filling one node before
	// starting the next would make low-density runs measure a single node's behaviour
	// rather than the host's.
	idx := 0
	offsets := make([]int, len(pools)) // next core index within each node
	slot := make([]int, len(pools))    // for ConfigE, which half of the core

	for idx < target {
		placed := false
		for pi := range pools {
			if idx >= target {
				break
			}
			pl := &pools[pi]

			var coreIdx int
			switch cfg {
			case ConfigE:
				// Two VMs per core: the first takes the primary sibling, the second
				// takes the sibling thread of the same core.
				coreIdx = offsets[pi]
				if coreIdx >= len(pl.vcpu) {
					continue
				}
			default:
				coreIdx = offsets[pi]
				if coreIdx >= len(pl.vcpu) {
					continue
				}
			}

			primary := pl.vcpu[coreIdx]
			vm := VM{Index: idx, Node: pl.node}

			switch cfg {
			case ConfigB:
				// One physical core, sibling deliberately unused.
				vm.VCPUSet = topology.FormatCPUList([]int{primary})
				allUsed = append(allUsed, primary)
				offsets[pi]++

			case ConfigC:
				sib, ok := top.SiblingOf(primary)
				if !ok {
					return nil, fmt.Errorf("%w: cpu %d has no sibling", ErrSMTRequired, primary)
				}
				vm.VCPUSet = topology.FormatCPUList([]int{primary})
				vm.VMMSet = topology.FormatCPUList([]int{sib})
				allUsed = append(allUsed, primary, sib)
				offsets[pi]++

			case ConfigD, ConfigF:
				vm.VCPUSet = topology.FormatCPUList([]int{primary})
				vm.VMMSet = topology.FormatCPUList(pl.vmmPool)
				allUsed = append(allUsed, primary)
				offsets[pi]++

			case ConfigE:
				// Alternate between the two threads of a core, advancing only after
				// both are used.
				assigned := primary
				if slot[pi] == 1 {
					sib, ok := top.SiblingOf(primary)
					if !ok {
						return nil, fmt.Errorf("%w: cpu %d has no sibling", ErrSMTRequired, primary)
					}
					assigned = sib
				}
				vm.VCPUSet = topology.FormatCPUList([]int{assigned})
				allUsed = append(allUsed, assigned)
				slot[pi]++
				if slot[pi] >= 2 {
					slot[pi] = 0
					offsets[pi]++
				}
			}

			p.VMs = append(p.VMs, vm)
			idx++
			placed = true
		}
		if !placed {
			break // every node exhausted
		}
	}

	allUsed = append(allUsed, vmmAll...)
	p.ParentCPUSet = topology.FormatCPUList(dedupe(allUsed))
	return p, nil
}

// Validate checks a plan for the invariants the experiment depends on. A plan that
// violates these would produce data that looks plausible but measures the wrong thing.
func (p *Plan) Validate(top *topology.Topology) error {
	if p.Config == ConfigA {
		return nil // unpinned by design
	}

	seen := map[int]int{} // cpu -> count of vCPU assignments

	for _, vm := range p.VMs {
		cpus, err := topology.ParseCPUList(vm.VCPUSet)
		if err != nil {
			return fmt.Errorf("vm %d: parse vcpu set: %w", vm.Index, err)
		}
		if len(cpus) == 0 {
			return fmt.Errorf("vm %d: empty vcpu set under pinned config %s", vm.Index, p.Config)
		}
		for _, c := range cpus {
			seen[c]++
			// A microVM must not straddle NUMA nodes.
			if vm.Node >= 0 {
				found := false
				for _, id := range top.CPUsInNode(vm.Node) {
					if id == c {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("vm %d: cpu %d is not in its assigned node %d", vm.Index, c, vm.Node)
				}
			}
		}
	}

	// Only the oversubscribed config may share a CPU between microVMs.
	if p.Config != ConfigE {
		for cpu, n := range seen {
			if n > 1 {
				return fmt.Errorf("cpu %d assigned to %d microVMs under config %s, which does not oversubscribe",
					cpu, n, p.Config)
			}
		}
	}

	// The host reserve must never be handed to a microVM.
	reserve, err := topology.ParseCPUList(p.HostReserve)
	if err != nil {
		return fmt.Errorf("parse host reserve: %w", err)
	}
	for _, r := range reserve {
		if seen[r] > 0 {
			return fmt.Errorf("host reserve cpu %d was assigned to a microVM", r)
		}
	}

	return nil
}

func divCeil(a, b int) int {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

func dedupe(in []int) []int {
	set := map[int]struct{}{}
	var out []int
	for _, v := range in {
		if _, ok := set[v]; ok {
			continue
		}
		set[v] = struct{}{}
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}
