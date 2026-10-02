// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCgroupFS builds a directory tree shaped like cgroup v2. Writes to control files
// behave like ordinary file writes, which is enough to verify ordering and content; the
// kernel's own validation is exercised only on real hardware.
func fakeCgroupFS(t *testing.T, controllers string) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "cgroup.controllers"), controllers)
	// Amazon Linux 2023 ships a root subtree_control without cpuset, which is what makes
	// enableRootCpuset necessary. The fixture reproduces that starting state.
	write(t, filepath.Join(root, "cgroup.subtree_control"), "cpu io memory pids")
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })
	return root
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

func TestEnsureV2AcceptsRequiredControllers(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu io memory hugetlb pids misc")
	if err := EnsureV2(); err != nil {
		t.Errorf("EnsureV2 on a complete host: %v", err)
	}
}

func TestEnsureV2RejectsMissingCpuset(t *testing.T) {
	fakeCgroupFS(t, "cpu io memory pids")
	err := EnsureV2()
	if err == nil {
		t.Fatal("EnsureV2 accepted a host without the cpuset controller")
	}
	if !strings.Contains(err.Error(), "cpuset") {
		t.Errorf("error %q should name the missing controller", err)
	}
}

func TestEnsureV2RejectsCgroupV1(t *testing.T) {
	// A v1 host has no cgroup.controllers file at the root.
	root := t.TempDir()
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })

	if err := EnsureV2(); err == nil {
		t.Error("EnsureV2 accepted a host with no cgroup.controllers (cgroup v1)")
	}
}

func TestCreateHierarchyWritesCPUsBeforeDelegating(t *testing.T) {
	root := fakeCgroupFS(t, "cpuset cpu memory")

	h, err := CreateHierarchy("fclab", "2-31,34-63", "0-1")
	if err != nil {
		t.Fatalf("CreateHierarchy: %v", err)
	}
	if h.Path != filepath.Join(root, "fclab") {
		t.Errorf("Path = %q, want %q", h.Path, filepath.Join(root, "fclab"))
	}
	if got := read(t, filepath.Join(h.Path, "cpuset.cpus")); got != "2-31,34-63" {
		t.Errorf("cpuset.cpus = %q, want 2-31,34-63", got)
	}
	if got := read(t, filepath.Join(h.Path, "cpuset.mems")); got != "0-1" {
		t.Errorf("cpuset.mems = %q, want 0-1", got)
	}
	// Children cannot set their own cpuset.cpus unless cpuset is delegated.
	sub := read(t, filepath.Join(h.Path, "cgroup.subtree_control"))
	if !strings.Contains(sub, "+cpuset") {
		t.Errorf("subtree_control = %q, want it to enable +cpuset", sub)
	}
}

func TestCreateHierarchyRejectsBadNames(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	for _, name := range []string{"", "a/b", "../escape", "."} {
		if _, err := CreateHierarchy(name, "0-1", "0"); err == nil {
			t.Errorf("CreateHierarchy(%q) = nil error, want rejection", name)
		}
	}
}

func TestCreateHierarchyRequiresCPUs(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	if _, err := CreateHierarchy("fclab", "", "0"); err == nil {
		t.Error("CreateHierarchy with an empty cpuset returned nil error")
	}
}

func TestCreateChildSetsOwnCPUSet(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	h, err := CreateHierarchy("fclab", "2-31", "0")
	if err != nil {
		t.Fatal(err)
	}
	path, err := h.CreateChild("vm0", "5", "0")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if got := read(t, filepath.Join(path, "cpuset.cpus")); got != "5" {
		t.Errorf("child cpuset.cpus = %q, want 5", got)
	}
	if got := read(t, filepath.Join(path, "cpuset.mems")); got != "0" {
		t.Errorf("child cpuset.mems = %q, want 0", got)
	}
}

func TestCreateChildRejectsPathTraversal(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	h, _ := CreateHierarchy("fclab", "2-31", "0")
	for _, id := range []string{"../sibling", "a/b", "."} {
		if _, err := h.CreateChild(id, "5", "0"); err == nil {
			t.Errorf("CreateChild(%q) = nil error, want rejection", id)
		}
	}
}

func TestSetExclusiveAndPartition(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	h, err := CreateHierarchy("fclab", "2-31", "0")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetExclusive("2-31"); err != nil {
		t.Fatalf("SetExclusive: %v", err)
	}
	if got := read(t, filepath.Join(h.Path, "cpuset.cpus.exclusive")); got != "2-31" {
		t.Errorf("cpuset.cpus.exclusive = %q, want 2-31", got)
	}
	if err := h.SetPartition(PartitionIsolated); err != nil {
		t.Fatalf("SetPartition: %v", err)
	}
	if got := read(t, filepath.Join(h.Path, "cpuset.cpus.partition")); got != "isolated" {
		t.Errorf("partition = %q, want isolated", got)
	}
}

// The kernel can accept the write and still refuse the transition, reporting an invalid
// state on read-back. Treating that as success would mean measuring an experiment that
// never had the isolation it claims.
func TestSetPartitionDetectsKernelRejection(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	h, err := CreateHierarchy("fclab", "2-31", "0")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the kernel reporting a rejected transition. A real kernel writes back
	// something like "isolated invalid (Invalid cpuset.cpus)".
	partPath := filepath.Join(h.Path, "cpuset.cpus.partition")
	write(t, partPath, "isolated invalid (Invalid cpuset.cpus)")

	err = h.verifyPartition(PartitionIsolated)
	if err == nil {
		t.Fatal("verifyPartition accepted an invalid partition state")
	}
	if !strings.Contains(err.Error(), "rejected by kernel") {
		t.Errorf("error %q should say the kernel rejected the transition", err)
	}
}

func TestSetPartitionDetectsSilentDowngrade(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	h, _ := CreateHierarchy("fclab", "2-31", "0")

	// A downgrade to member with no "invalid" marker is still a failure to isolate.
	write(t, filepath.Join(h.Path, "cpuset.cpus.partition"), "member")

	if err := h.verifyPartition(PartitionIsolated); err == nil {
		t.Error("verifyPartition accepted member when isolated was requested")
	}
}

func TestDestroyRemovesChildrenAndParent(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	h, err := CreateHierarchy("fclab", "2-31", "0")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm0", "vm1", "vm2"} {
		if _, err := h.CreateChild(id, "5", "0"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(h.Path); !os.IsNotExist(err) {
		t.Errorf("parent cgroup still present after Destroy (stat err = %v)", err)
	}
}

func TestDestroyIsIdempotent(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu memory")
	h, _ := CreateHierarchy("fclab", "2-31", "0")
	if err := h.Destroy(); err != nil {
		t.Fatal(err)
	}
	// Destroying an already-removed hierarchy should not error: cleanup runs on paths
	// where a previous cleanup may have partially succeeded.
	if err := h.Destroy(); err != nil {
		t.Errorf("second Destroy returned %v, want nil", err)
	}
}

// A root cgroup without cpuset in subtree_control means a new child has no cpuset.cpus file
// at all, and the write fails with a permission error that looks like a privilege problem.
// This is the Amazon Linux 2023 default, so CreateHierarchy must fix it rather than fail.
func TestCreateHierarchyEnablesRootCpuset(t *testing.T) {
	root := fakeCgroupFS(t, "cpuset cpu io memory hugetlb pids misc")

	if got := read(t, filepath.Join(root, "cgroup.subtree_control")); strings.Contains(got, "cpuset") {
		t.Fatalf("fixture should start without cpuset, got %q", got)
	}

	if _, err := CreateHierarchy("fclab", "2-31", "0"); err != nil {
		t.Fatalf("CreateHierarchy: %v", err)
	}

	if got := read(t, filepath.Join(root, "cgroup.subtree_control")); !strings.Contains(got, "cpuset") {
		t.Errorf("root subtree_control = %q, want cpuset enabled", got)
	}
}

func TestEnableRootCpusetIsIdempotent(t *testing.T) {
	root := fakeCgroupFS(t, "cpuset cpu memory")
	write(t, filepath.Join(root, "cgroup.subtree_control"), "cpuset cpu memory")

	// Already enabled: must not attempt a write, since some hosts refuse redundant writes.
	if err := enableRootCpuset(); err != nil {
		t.Errorf("enableRootCpuset on an already-enabled root: %v", err)
	}
}

func TestCreateHierarchyDelegatesMemoryController(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu io memory pids")
	h, err := CreateHierarchy("fclab", "0-3", "0")
	if err != nil {
		t.Fatalf("CreateHierarchy: %v", err)
	}
	got := read(t, filepath.Join(h.Path, "cgroup.subtree_control"))
	if !strings.Contains(got, "+memory") {
		t.Errorf("subtree_control %q must delegate the memory controller so children can set memory.max", got)
	}
}

func TestSetChildMemoryMaxWritesCapAndForbidsSwap(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu io memory pids")
	h, err := CreateHierarchy("fclab", "0-3", "0")
	if err != nil {
		t.Fatalf("CreateHierarchy: %v", err)
	}
	if _, err := h.CreateChild("vm0", "2", "0"); err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	// The fake fs behaves like ordinary files, so pre-create memory.swap.max to model a
	// kernel with swap support compiled in.
	write(t, filepath.Join(h.Path, "vm0", "memory.swap.max"), "max")

	maxBytes := ChildMemoryMaxBytes(2048)
	if err := h.SetChildMemoryMax("vm0", maxBytes); err != nil {
		t.Fatalf("SetChildMemoryMax: %v", err)
	}
	if got, want := read(t, filepath.Join(h.Path, "vm0", "memory.max")), "2281701376"; got != want {
		t.Errorf("memory.max = %q, want %q (2048 MiB guest + %d MiB VMM overhead)", got, want, VMMOverheadMiB)
	}
	if got := read(t, filepath.Join(h.Path, "vm0", "memory.swap.max")); got != "0" {
		t.Errorf("memory.swap.max = %q, want 0: an untrusted guest must not page to host storage", got)
	}
}

func TestSetChildMemoryMaxRejectsBogusSizes(t *testing.T) {
	fakeCgroupFS(t, "cpuset cpu io memory pids")
	h, err := CreateHierarchy("fclab", "0-3", "0")
	if err != nil {
		t.Fatalf("CreateHierarchy: %v", err)
	}
	if _, err := h.CreateChild("vm0", "2", "0"); err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	// (The ENOENT tolerance for kernels without swap cannot be modelled on a plain
	// filesystem, where os.WriteFile creates the missing file; that branch is exercised
	// only on real hardware.)
	if err := h.SetChildMemoryMax("vm0", ChildMemoryMaxBytes(128)); err != nil {
		t.Fatalf("SetChildMemoryMax: %v", err)
	}
	if err := h.SetChildMemoryMax("vm0", 0); err == nil {
		t.Error("SetChildMemoryMax accepted a zero cap; an unbounded child defeats the control")
	}
	if err := h.SetChildMemoryMax("vm0", -5); err == nil {
		t.Error("SetChildMemoryMax accepted a negative cap")
	}
}

func TestChildMemoryMaxBytesArithmetic(t *testing.T) {
	// 2048 MiB guest + 128 MiB overhead = 2176 MiB.
	if got, want := ChildMemoryMaxBytes(2048), int64(2176)<<20; got != want {
		t.Errorf("ChildMemoryMaxBytes(2048) = %d, want %d", got, want)
	}
}
