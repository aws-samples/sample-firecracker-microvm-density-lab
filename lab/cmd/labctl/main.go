// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Command labctl inspects host topology and previews microVM placement plans.
//
// Two subcommands:
//
//	labctl topology            report CPU, cache, and NUMA layout as text or JSON
//	labctl plan -config B ...  preview the cpuset assignments for one sweep config
//
// Both are read-only. Nothing here launches a microVM or writes to a cgroup, so it is
// safe to run on a host that is doing real work, and it can be run before a lab session
// to confirm the host is the shape the experiment assumes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "topology":
		if err := cmdTopology(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "plan":
		if err := cmdPlan(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `labctl inspects host topology and previews microVM placement.

Usage:
  labctl topology [-json]
  labctl plan -config <A|B|C|D|E|F> -density <n> [-json] [-vmm-pool <n>] [-reserve <n>]

Both subcommands are read-only.
`)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

func cmdTopology(args []string) error {
	fs := flag.NewFlagSet("topology", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	top, err := topology.Discover()
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(top)
	}

	fmt.Printf("Logical CPUs:     %d\n", len(top.CPUs))
	fmt.Printf("Physical cores:   %d\n", top.PhysicalCores())
	fmt.Printf("Threads per core: %d\n", top.ThreadsPerCore())
	fmt.Printf("SMT state:        %s\n", top.SMTState)
	fmt.Printf("NUMA nodes:       %d\n", len(top.Nodes))
	fmt.Printf("L3 domains:       %d\n", len(top.L3Domains))

	fmt.Println("\nPer-node layout:")
	for _, n := range top.Nodes {
		cpus := top.CPUsInNode(n)
		cores := top.PrimarySiblingsInNode(n)
		fmt.Printf("  node%-3d %3d logical CPUs, %3d physical cores  %s\n",
			n, len(cpus), len(cores), topology.FormatCPUList(cpus))
	}

	// How cache domains relate to NUMA nodes is the measurement that distinguishes NUMA
	// mechanisms, and it is the reason this subcommand exists: the node count alone
	// leaves the cause ambiguous, and it also determines whether pinning to a node buys
	// cache isolation or only memory locality.
	if mode := top.CacheMode(); mode != topology.CacheModeUnknown {
		fmt.Printf("\nCache and NUMA relationship: %s\n", mode)
		fmt.Printf("  last-level cache domains: %d, NUMA nodes: %d, a domain spans nodes: %v\n",
			len(top.L3Domains), len(top.Nodes), top.L3SpansNodes())

		switch mode {
		case topology.CacheUnified:
			fmt.Println("\n  One last-level cache domain spans every NUMA node, so the nodes")
			fmt.Println("  partition memory rather than cache. This is the shape Intel Sub-NUMA")
			fmt.Println("  Clustering produces: memory controllers split into domains while the")
			fmt.Println("  last-level cache stays shared host-wide.")
			fmt.Println("\n  Placement consequence: confining a microVM to one node buys memory")
			fmt.Println("  locality but NOT cache isolation. Last-level cache remains a shared")
			fmt.Println("  resource across the whole host, so cache pressure from any microVM is")
			fmt.Println("  visible to all of them regardless of node assignment.")
		case topology.CachePerNode:
			fmt.Println("\n  Each NUMA node contains its own last-level cache domain and no domain")
			fmt.Println("  crosses a node. This is what a chiplet or tile mapped to a node looks")
			fmt.Println("  like, for example an AMD CCD group under an NPS mode.")
			fmt.Println("\n  Placement consequence: node confinement buys both memory locality and")
			fmt.Println("  cache isolation.")
		case topology.CacheSubdividedPerNode:
			fmt.Println("\n  Each NUMA node contains several distinct cache domains, none crossing")
			fmt.Println("  a node boundary. Pinning within an L3 domain is a finer-grained lever")
			fmt.Println("  than pinning to a node.")
		}
	}

	if top.Distances != nil {
		fmt.Println("\nNode distance matrix:")
		fmt.Print("        ")
		for _, n := range top.Nodes {
			fmt.Printf("node%-3d ", n)
		}
		fmt.Println()
		for _, from := range top.Nodes {
			fmt.Printf("  node%-3d", from)
			for _, to := range top.Nodes {
				fmt.Printf("%4d    ", top.Distances[from][to])
			}
			fmt.Println()
		}
	}

	// Report which sweep configs this host can actually run, so a session does not
	// discover mid-run that half the matrix is inapplicable.
	fmt.Println("\nRunnable sweep configs on this host:")
	for _, c := range placement.AllConfigs {
		ok := !c.RequiresSMT() || top.HasSMT()
		mark := "yes"
		reason := ""
		if !ok {
			mark = "no "
			reason = "  (requires SMT; this host reports one thread per core)"
		}
		if c == placement.ConfigF && top.SMTState == "forceoff" {
			mark = "no "
			reason = "  (SMT is forceoff and cannot be toggled at runtime)"
		}
		fmt.Printf("  %s  %s  %s%s\n", mark, c, c.Description(), reason)
	}

	return nil
}

func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	cfgName := fs.String("config", "", "sweep config: A, B, C, D, E, or F")
	density := fs.Int("density", 0, "target number of microVMs")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	vmmPool := fs.Int("vmm-pool", 0, "physical cores for the shared VMM pool (default 8)")
	reserve := fs.Int("reserve", 0, "physical cores reserved for the host (default 4)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgName == "" || *density == 0 {
		return fmt.Errorf("both -config and -density are required")
	}

	cfg := placement.Config(strings.ToUpper(*cfgName))
	known := false
	for _, c := range placement.AllConfigs {
		if c == cfg {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("unknown config %q; want one of A B C D E F", *cfgName)
	}

	top, err := topology.Discover()
	if err != nil {
		return err
	}

	opts := placement.DefaultOptions()
	if *vmmPool > 0 {
		opts.VMMPoolCores = *vmmPool
	}
	if *reserve > 0 {
		opts.HostReserveCores = *reserve
	}

	p, err := placement.Build(top, cfg, *density, opts)
	if err != nil {
		return err
	}
	// Validate before reporting: an invalid plan would produce data that looks fine and
	// measures the wrong thing.
	if err := p.Validate(top); err != nil {
		return fmt.Errorf("plan failed validation: %w", err)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}

	fmt.Printf("Config:        %s  (%s)\n", p.Config, p.Config.Description())
	fmt.Printf("Requested:     %d microVMs\n", p.Density)
	fmt.Printf("Planned:       %d microVMs\n", len(p.VMs))
	if p.VMMPool != "" {
		fmt.Printf("VMM pool:      %s\n", p.VMMPool)
	}
	if p.HostReserve != "" {
		fmt.Printf("Host reserve:  %s\n", p.HostReserve)
	}
	if p.ParentCPUSet != "" {
		fmt.Printf("Parent cpuset: %s\n", p.ParentCPUSet)
	}

	for _, n := range p.Notes {
		fmt.Printf("\nNote: %s\n", n)
	}

	byNode := map[int]int{}
	for _, vm := range p.VMs {
		byNode[vm.Node]++
	}
	fmt.Println("\nmicroVMs per NUMA node:")
	var nodes []int
	for n := range byNode {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	for _, n := range nodes {
		label := fmt.Sprintf("node%d", n)
		if n < 0 {
			label = "unpinned"
		}
		fmt.Printf("  %-9s %d\n", label, byNode[n])
	}

	// Print a sample rather than all 180 assignments.
	fmt.Println("\nFirst assignments:")
	for i, vm := range p.VMs {
		if i >= 8 {
			fmt.Printf("  ... %d more\n", len(p.VMs)-8)
			break
		}
		vmm := vm.VMMSet
		if vmm == "" {
			vmm = "(shares vcpu set)"
		}
		vcpu := vm.VCPUSet
		if vcpu == "" {
			vcpu = "(unconstrained)"
		}
		fmt.Printf("  vm%-4d node%-3d vcpu=%-10s vmm=%s\n", vm.Index, vm.Node, vcpu, vmm)
	}

	fmt.Println("\nReminder: cpuset.cpus.partition is owned by the parent cgroup and is not")
	fmt.Println("delegatable. Write ParentCPUSet to the parent and set its partition state")
	fmt.Println("before creating per-microVM cgroups. The jailer --cgroup flag cannot do this.")

	return nil
}
