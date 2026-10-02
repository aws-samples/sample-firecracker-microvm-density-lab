// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package topology discovers CPU, cache, and NUMA layout from sysfs.
//
// Real topology cannot be baked into an image or assumed from an instance type: the
// number of NUMA nodes on a single socket depends on firmware settings (AMD NPS mode,
// Intel Sub-NUMA Clustering), and SMT sibling numbering is not portable. On one
// m8i.metal-48xl, for example, Linux reported three NUMA nodes on a single socket with
// SMT siblings offset by 96. Anything that pins a vCPU has to read the machine.
//
// This package is deliberately dependency-free and read-only so the production placement
// agent can adopt it unchanged.
package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sysfsRoot is a variable rather than a constant so tests can point it at a fixture
// directory containing a captured topology.
var sysfsRoot = "/sys/devices/system/cpu"

// CPU is one logical processor, which is one hardware thread.
type CPU struct {
	ID       int   // logical CPU id, as used by cpuset
	Core     int   // physical core id, shared by SMT siblings
	Socket   int   // physical package id
	Node     int   // NUMA node, -1 if the system exposes none
	Siblings []int // all logical CPUs sharing this physical core, including ID
	L3       int   // index into Topology.L3Domains, -1 if unknown
}

// IsPrimarySibling reports whether this CPU is the lowest-numbered thread on its core.
// Pinning one microVM per primary sibling is how a "one vCPU per physical core"
// configuration is expressed on an SMT-enabled host.
func (c CPU) IsPrimarySibling() bool {
	for _, s := range c.Siblings {
		if s < c.ID {
			return false
		}
	}
	return true
}

// L3Domain is a set of logical CPUs sharing one last-level cache.
type L3Domain struct {
	Index int
	CPUs  []int
	Node  int // NUMA node this domain belongs to, -1 if unknown
}

// Topology is the discovered layout of one host.
type Topology struct {
	CPUs      []CPU
	L3Domains []L3Domain
	Nodes     []int // sorted NUMA node ids
	// Distances is the inter-node distance matrix indexed by node id, nil when the
	// host exposes no NUMA information. Values are relative, with 10 meaning local.
	Distances map[int]map[int]int
	SMTState  string // on, off, forceoff, notsupported, or unavailable
}

// Discover reads the current host's topology.
func Discover() (*Topology, error) {
	ids, err := parseCPUList(filepath.Join(sysfsRoot, "present"))
	if err != nil {
		return nil, fmt.Errorf("read present CPUs: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no CPUs found under %s", sysfsRoot)
	}

	t := &Topology{SMTState: readSMTState()}

	for _, id := range ids {
		base := filepath.Join(sysfsRoot, fmt.Sprintf("cpu%d", id))
		c := CPU{ID: id, Node: -1, L3: -1}

		// Offline CPUs have no topology directory. Skip rather than fail: a host may
		// legitimately have CPUs offlined, for instance after disabling SMT at runtime.
		if _, err := os.Stat(filepath.Join(base, "topology")); os.IsNotExist(err) {
			continue
		}

		c.Core = readIntOr(filepath.Join(base, "topology/core_id"), -1)
		c.Socket = readIntOr(filepath.Join(base, "topology/physical_package_id"), -1)

		if sibs, err := parseCPUList(filepath.Join(base, "topology/thread_siblings_list")); err == nil {
			c.Siblings = sibs
		} else {
			c.Siblings = []int{id}
		}

		c.Node = nodeOf(id)
		t.CPUs = append(t.CPUs, c)
	}

	if err := t.discoverL3(); err != nil {
		return nil, err
	}
	t.discoverNodes()
	t.Distances = readDistances(t.Nodes)

	return t, nil
}

// discoverL3 groups CPUs by last-level cache. Cache index numbering is not fixed
// across platforms, so find the cache whose level is 3 rather than assuming index3.
func (t *Topology) discoverL3() error {
	seen := map[string]int{} // shared_cpu_list -> domain index

	for i := range t.CPUs {
		id := t.CPUs[i].ID
		cacheDir := filepath.Join(sysfsRoot, fmt.Sprintf("cpu%d/cache", id))
		entries, err := os.ReadDir(cacheDir)
		if err != nil {
			continue // no cache info exposed; leave L3 as -1
		}

		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "index") {
				continue
			}
			lvl := readIntOr(filepath.Join(cacheDir, e.Name(), "level"), -1)
			if lvl != 3 {
				continue
			}
			listPath := filepath.Join(cacheDir, e.Name(), "shared_cpu_list")
			raw, err := os.ReadFile(listPath)
			if err != nil {
				continue
			}
			key := strings.TrimSpace(string(raw))
			idx, ok := seen[key]
			if !ok {
				cpus, err := parseCPUList(listPath)
				if err != nil {
					continue
				}
				idx = len(t.L3Domains)
				seen[key] = idx
				t.L3Domains = append(t.L3Domains, L3Domain{
					Index: idx,
					CPUs:  cpus,
					Node:  t.CPUs[i].Node,
				})
			}
			t.CPUs[i].L3 = idx
			break
		}
	}
	return nil
}

func (t *Topology) discoverNodes() {
	set := map[int]struct{}{}
	for _, c := range t.CPUs {
		if c.Node >= 0 {
			set[c.Node] = struct{}{}
		}
	}
	for n := range set {
		t.Nodes = append(t.Nodes, n)
	}
	sort.Ints(t.Nodes)
}

// PhysicalCores returns the number of distinct physical cores.
//
// Counting entries in a per-logical-CPU listing overcounts by the SMT factor: on a host
// with 96 cores and 2 threads each, that yields 192. Deduplicate by (socket, core).
func (t *Topology) PhysicalCores() int {
	set := map[[2]int]struct{}{}
	for _, c := range t.CPUs {
		set[[2]int{c.Socket, c.Core}] = struct{}{}
	}
	return len(set)
}

// ThreadsPerCore returns the SMT factor, or 1 when SMT is absent or disabled.
func (t *Topology) ThreadsPerCore() int {
	cores := t.PhysicalCores()
	if cores == 0 {
		return 0
	}
	return len(t.CPUs) / cores
}

// HasSMT reports whether more than one thread per core is currently online.
func (t *Topology) HasSMT() bool { return t.ThreadsPerCore() > 1 }

// CPUsInNode returns the online logical CPUs belonging to a NUMA node, sorted.
func (t *Topology) CPUsInNode(node int) []int {
	var out []int
	for _, c := range t.CPUs {
		if c.Node == node {
			out = append(out, c.ID)
		}
	}
	sort.Ints(out)
	return out
}

// PrimarySiblingsInNode returns one logical CPU per physical core in a node: the
// lowest-numbered thread of each core. This is the allocation unit for configurations
// that give each microVM a full physical core.
func (t *Topology) PrimarySiblingsInNode(node int) []int {
	var out []int
	for _, c := range t.CPUs {
		if c.Node == node && c.IsPrimarySibling() {
			out = append(out, c.ID)
		}
	}
	sort.Ints(out)
	return out
}

// SiblingOf returns the other thread on the same physical core, and false when the CPU
// has no sibling (SMT absent or disabled).
func (t *Topology) SiblingOf(cpu int) (int, bool) {
	for _, c := range t.CPUs {
		if c.ID != cpu {
			continue
		}
		for _, s := range c.Siblings {
			if s != cpu {
				return s, true
			}
		}
		return 0, false
	}
	return 0, false
}

// L3PerNode counts distinct last-level cache domains that appear within each NUMA node.
//
// Read this together with L3SpansNodes. A count of 1 here is ambiguous on its own: it
// means either that the node owns exactly one cache domain, or that the whole machine has
// one cache domain which every node happens to see. Those are opposite architectures.
func (t *Topology) L3PerNode() map[int]int {
	out := map[int]int{}
	seen := map[[2]int]struct{}{}
	for _, c := range t.CPUs {
		if c.Node < 0 || c.L3 < 0 {
			continue
		}
		k := [2]int{c.Node, c.L3}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out[c.Node]++
	}
	return out
}

// CacheMode describes how last-level cache domains relate to NUMA nodes.
type CacheMode string

const (
	// CacheUnified means one last-level cache domain spans every NUMA node, so the
	// nodes reflect memory-side partitioning rather than cache boundaries. Intel
	// Sub-NUMA Clustering produces this: memory controllers are split into domains
	// while the last-level cache stays shared.
	CacheUnified CacheMode = "unified-across-nodes"
	// CachePerNode means each NUMA node contains its own last-level cache domain, and
	// no domain crosses a node. This is what a chiplet or tile mapped to a node looks
	// like, for example an AMD CCD group under an NPS mode.
	CachePerNode CacheMode = "per-node"
	// CacheSubdividedPerNode means each node contains several distinct cache domains,
	// with none crossing a node boundary.
	CacheSubdividedPerNode CacheMode = "multiple-per-node"
	// CacheModeUnknown means the host exposed no usable cache or NUMA information.
	CacheModeUnknown CacheMode = "unknown"
)

// L3SpansNodes reports whether any single last-level cache domain covers CPUs in more
// than one NUMA node.
//
// This is the measurement that actually discriminates NUMA mechanisms, and it is the one
// that matters for placement. If a cache domain spans nodes, then last-level cache is a
// host-wide shared resource and confining a microVM to a node buys memory locality but
// not cache isolation.
func (t *Topology) L3SpansNodes() bool {
	nodesOf := map[int]map[int]struct{}{} // L3 index -> set of NUMA nodes
	for _, c := range t.CPUs {
		if c.L3 < 0 || c.Node < 0 {
			continue
		}
		if nodesOf[c.L3] == nil {
			nodesOf[c.L3] = map[int]struct{}{}
		}
		nodesOf[c.L3][c.Node] = struct{}{}
	}
	for _, nodes := range nodesOf {
		if len(nodes) > 1 {
			return true
		}
	}
	return false
}

// CacheMode classifies the relationship between cache domains and NUMA nodes.
func (t *Topology) CacheMode() CacheMode {
	if len(t.L3Domains) == 0 || len(t.Nodes) == 0 {
		return CacheModeUnknown
	}
	if t.L3SpansNodes() {
		return CacheUnified
	}
	per := t.L3PerNode()
	for _, n := range t.Nodes {
		if per[n] > 1 {
			return CacheSubdividedPerNode
		}
	}
	return CachePerNode
}

// ---- sysfs helpers ----

func readSMTState() string {
	raw, err := os.ReadFile(filepath.Join(sysfsRoot, "smt/control"))
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(raw))
}

func nodeOf(cpu int) int {
	// Node membership is exposed as a nodeN symlink inside the CPU directory.
	entries, err := os.ReadDir(filepath.Join(sysfsRoot, fmt.Sprintf("cpu%d", cpu)))
	if err != nil {
		return -1
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "node") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "node")); err == nil {
			return n
		}
	}
	return -1
}

func readDistances(nodes []int) map[int]map[int]int {
	if len(nodes) == 0 {
		return nil
	}
	out := map[int]map[int]int{}
	for _, n := range nodes {
		path := fmt.Sprintf("/sys/devices/system/node/node%d/distance", n)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		row := map[int]int{}
		for i, f := range strings.Fields(string(raw)) {
			v, err := strconv.Atoi(f)
			if err != nil || i >= len(nodes) {
				continue
			}
			row[nodes[i]] = v
		}
		out[n] = row
	}
	return out
}

func readIntOr(path string, def int) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return def
	}
	return v
}

// parseCPUList parses the kernel cpulist format: comma-separated ids and inclusive
// ranges, for example "0-31,96-127".
func parseCPUList(path string) ([]int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCPUList(strings.TrimSpace(string(raw)))
}

// ParseCPUList parses a kernel cpulist string such as "0-3,8,12-13".
func ParseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		if !isRange {
			v, err := strconv.Atoi(lo)
			if err != nil {
				return nil, fmt.Errorf("parse cpu %q: %w", lo, err)
			}
			out = append(out, v)
			continue
		}
		start, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("parse range start %q: %w", lo, err)
		}
		end, err := strconv.Atoi(hi)
		if err != nil {
			return nil, fmt.Errorf("parse range end %q: %w", hi, err)
		}
		if end < start {
			return nil, fmt.Errorf("invalid range %q", part)
		}
		for v := start; v <= end; v++ {
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out, nil
}

// FormatCPUList renders ids in kernel cpulist form, collapsing runs into ranges. This
// is the format cpuset.cpus expects.
func FormatCPUList(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	s := append([]int(nil), ids...)
	sort.Ints(s)

	var parts []string
	start, prev := s[0], s[0]
	flush := func() {
		switch {
		case start == prev:
			parts = append(parts, strconv.Itoa(start))
		case prev == start+1:
			parts = append(parts, strconv.Itoa(start), strconv.Itoa(prev))
		default:
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, v := range s[1:] {
		if v == prev+1 {
			prev = v
			continue
		}
		if v == prev { // tolerate duplicates
			continue
		}
		flush()
		start, prev = v, v
	}
	flush()
	return strings.Join(parts, ",")
}
