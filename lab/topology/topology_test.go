// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCPUList(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"0", []int{0}},
		{"0-3", []int{0, 1, 2, 3}},
		{"0,2,4", []int{0, 2, 4}},
		{"0-2,8,10-11", []int{0, 1, 2, 8, 10, 11}},
		// The layout observed on m8i.metal-48xl NUMA node0.
		{"0-31,96-127", func() []int {
			var v []int
			for i := 0; i <= 31; i++ {
				v = append(v, i)
			}
			for i := 96; i <= 127; i++ {
				v = append(v, i)
			}
			return v
		}()},
	}
	for _, c := range cases {
		got, err := ParseCPUList(c.in)
		if err != nil {
			t.Fatalf("ParseCPUList(%q): %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("ParseCPUList(%q) len = %d, want %d", c.in, len(got), len(c.want))
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("ParseCPUList(%q)[%d] = %d, want %d", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseCPUListErrors(t *testing.T) {
	for _, in := range []string{"abc", "5-1", "3-x"} {
		if _, err := ParseCPUList(in); err == nil {
			t.Errorf("ParseCPUList(%q) = nil error, want error", in)
		}
	}
}

func TestFormatCPUList(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{5}, "5"},
		{[]int{0, 1, 2, 3}, "0-3"},
		{[]int{0, 2, 4}, "0,2,4"},
		// A two-element run is written as two ids, since "0-1" and "0,1" are
		// equivalent but the latter matches kernel output.
		{[]int{0, 1}, "0,1"},
		{[]int{3, 1, 2}, "1-3"},    // unsorted input
		{[]int{1, 1, 2, 3}, "1-3"}, // duplicates
		{[]int{0, 1, 2, 8, 10, 11}, "0-2,8,10,11"},
	}
	for _, c := range cases {
		if got := FormatCPUList(c.in); got != c.want {
			t.Errorf("FormatCPUList(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	orig := []int{0, 1, 2, 3, 17, 40, 41, 42, 96, 97}
	s := FormatCPUList(orig)
	back, err := ParseCPUList(s)
	if err != nil {
		t.Fatalf("ParseCPUList(%q): %v", s, err)
	}
	if len(back) != len(orig) {
		t.Fatalf("round trip via %q gave %d ids, want %d", s, len(back), len(orig))
	}
	for i := range orig {
		if back[i] != orig[i] {
			t.Fatalf("round trip via %q: [%d] = %d, want %d", s, i, back[i], orig[i])
		}
	}
}

// writeFixture builds a sysfs-shaped tree. Parameters mirror a real host: cores physical
// cores, threads per core, and nodes NUMA nodes. Sibling numbering follows the pattern
// observed on m8i.metal-48xl, where the second thread of core n is CPU n+cores.
func writeFixture(t *testing.T, cores, threads, nodes, l3PerNode int) string {
	t.Helper()
	root := t.TempDir()

	total := cores * threads
	var present string
	if total > 0 {
		present = fmt.Sprintf("0-%d", total-1)
	}
	mustWrite(t, filepath.Join(root, "present"), present)
	mustWrite(t, filepath.Join(root, "smt/control"), map[bool]string{true: "on", false: "notsupported"}[threads > 1])

	coresPerNode := cores / nodes
	coresPerL3 := coresPerNode / l3PerNode

	for cpu := 0; cpu < total; cpu++ {
		core := cpu % cores
		node := core / coresPerNode
		l3 := core / coresPerL3

		dir := filepath.Join(root, fmt.Sprintf("cpu%d", cpu))
		mustWrite(t, filepath.Join(dir, "topology/core_id"), fmt.Sprint(core))
		mustWrite(t, filepath.Join(dir, "topology/physical_package_id"), "0")

		// Siblings: core and, when SMT is present, core+cores.
		sibs := fmt.Sprint(core)
		if threads > 1 {
			sibs = fmt.Sprintf("%d,%d", core, core+cores)
		}
		mustWrite(t, filepath.Join(dir, "topology/thread_siblings_list"), sibs)

		// NUMA membership is a nodeN entry in the CPU directory.
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("node%d/.keep", node)), "")

		// L3 is exposed as index3 here; the code locates it by level, not index.
		lo := l3 * coresPerL3
		hi := lo + coresPerL3 - 1
		shared := fmt.Sprintf("%d-%d", lo, hi)
		if threads > 1 {
			shared = fmt.Sprintf("%d-%d,%d-%d", lo, hi, lo+cores, hi+cores)
		}
		mustWrite(t, filepath.Join(dir, "cache/index3/level"), "3")
		mustWrite(t, filepath.Join(dir, "cache/index3/shared_cpu_list"), shared)
		// An L2 entry, to confirm level-based lookup ignores it.
		mustWrite(t, filepath.Join(dir, "cache/index2/level"), "2")
		mustWrite(t, filepath.Join(dir, "cache/index2/shared_cpu_list"), fmt.Sprint(cpu))
	}
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withFixture(t *testing.T, root string) {
	t.Helper()
	old := sysfsRoot
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = old })
}

// The shape measured on a real m8i.metal-48xl: 96 physical cores, 2 threads each,
// 3 NUMA nodes on one socket. This is the case that previously produced a wrong core
// count, so it is the primary regression test.
func TestDiscoverM8iShape(t *testing.T) {
	withFixture(t, writeFixture(t, 96, 2, 3, 1))

	top, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if got := len(top.CPUs); got != 192 {
		t.Errorf("logical CPUs = %d, want 192", got)
	}
	// Regression: counting per-logical-CPU rows would report 192 here.
	if got := top.PhysicalCores(); got != 96 {
		t.Errorf("PhysicalCores() = %d, want 96", got)
	}
	if got := top.ThreadsPerCore(); got != 2 {
		t.Errorf("ThreadsPerCore() = %d, want 2", got)
	}
	if !top.HasSMT() {
		t.Error("HasSMT() = false, want true")
	}
	if got := len(top.Nodes); got != 3 {
		t.Errorf("NUMA nodes = %d, want 3", got)
	}

	// Each node holds 32 physical cores, so 64 logical CPUs.
	if got := len(top.CPUsInNode(0)); got != 64 {
		t.Errorf("CPUsInNode(0) = %d, want 64", got)
	}
	if got := len(top.PrimarySiblingsInNode(0)); got != 32 {
		t.Errorf("PrimarySiblingsInNode(0) = %d, want 32", got)
	}

	// Sibling of CPU 0 is 96 on this layout.
	if sib, ok := top.SiblingOf(0); !ok || sib != 96 {
		t.Errorf("SiblingOf(0) = (%d, %v), want (96, true)", sib, ok)
	}

	// One L3 domain per node in this fixture.
	for node, n := range top.L3PerNode() {
		if n != 1 {
			t.Errorf("L3PerNode()[%d] = %d, want 1", node, n)
		}
	}
}

// m8a.metal-48xl: 192 physical cores, one thread each, no SMT.
func TestDiscoverM8aShape(t *testing.T) {
	withFixture(t, writeFixture(t, 192, 1, 4, 1))

	top, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := len(top.CPUs); got != 192 {
		t.Errorf("logical CPUs = %d, want 192", got)
	}
	if got := top.PhysicalCores(); got != 192 {
		t.Errorf("PhysicalCores() = %d, want 192", got)
	}
	if got := top.ThreadsPerCore(); got != 1 {
		t.Errorf("ThreadsPerCore() = %d, want 1", got)
	}
	if top.HasSMT() {
		t.Error("HasSMT() = true, want false on a no-SMT host")
	}
	if top.SMTState != "notsupported" {
		t.Errorf("SMTState = %q, want notsupported", top.SMTState)
	}
	// Without SMT every CPU is its own core, so all are primary siblings.
	if got := len(top.PrimarySiblingsInNode(0)); got != 48 {
		t.Errorf("PrimarySiblingsInNode(0) = %d, want 48", got)
	}
	if _, ok := top.SiblingOf(0); ok {
		t.Error("SiblingOf(0) reported a sibling on a no-SMT host")
	}
}

// Multiple L3 domains per NUMA node is the signature of a partitioning scheme rather
// than one node per tile. The discriminator has to report this correctly.
func TestL3PerNodeDetectsSubdivision(t *testing.T) {
	withFixture(t, writeFixture(t, 96, 2, 3, 2))

	top, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := top.L3PerNode()
	if len(got) != 3 {
		t.Fatalf("L3PerNode() covered %d nodes, want 3", len(got))
	}
	for node, n := range got {
		if n != 2 {
			t.Errorf("L3PerNode()[%d] = %d, want 2", node, n)
		}
	}
}

func TestDiscoverNoCPUs(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "present"), "")
	withFixture(t, root)

	if _, err := Discover(); err == nil {
		t.Error("Discover() = nil error on an empty host, want error")
	}
}
