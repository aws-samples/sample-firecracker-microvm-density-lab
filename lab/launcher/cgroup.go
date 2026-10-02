// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package launcher creates cgroups and starts jailed Firecracker processes.
//
// The cgroup work is separated from process launch because it has a constraint that shapes
// the whole design: cpuset.cpus.partition is owned by the parent cgroup and is not
// delegatable. The jailer can write cpuset.cpus for a microVM's own cgroup via --cgroup,
// but it cannot make that cgroup a partition root. So the parent hierarchy has to be
// established, with its exclusive CPU set and partition state, before any microVM starts.
package launcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// cgroupRoot is a variable so tests can point it at a temporary directory.
var cgroupRoot = "/sys/fs/cgroup"

// PartitionMode is the value written to cpuset.cpus.partition.
type PartitionMode string

const (
	// PartitionMember is an ordinary cgroup, not a partition root.
	PartitionMember PartitionMode = "member"
	// PartitionRoot gives the cgroup exclusive ownership of its CPUs.
	PartitionRoot PartitionMode = "root"
	// PartitionIsolated is a partition root with scheduler load balancing disabled and
	// the CPUs excluded from unbound workqueues. This is the mode that matters for
	// pinning: it is what removes the scheduler from the picture rather than merely
	// constraining where it may place a thread.
	PartitionIsolated PartitionMode = "isolated"
)

// Hierarchy is a parent cgroup that owns a set of CPUs, under which per-microVM cgroups
// are created.
type Hierarchy struct {
	// Name is the directory under the cgroup root, for example "fclab".
	Name string
	// Path is the absolute path to the parent cgroup directory.
	Path string
}

// EnsureV2 verifies the host is running cgroup v2 with the controllers the lab needs.
//
// cgroup v1 is not merely different here: Firecracker documents high snapshot restore
// latency under v1, and the cpuset partition interface does not exist there at all.
func EnsureV2() error {
	controllersPath := filepath.Join(cgroupRoot, "cgroup.controllers")
	raw, err := os.ReadFile(controllersPath)
	if err != nil {
		return fmt.Errorf("cgroup v2 not active: cannot read %s: %w", controllersPath, err)
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(raw)) {
		have[c] = true
	}
	for _, need := range []string{"cpuset", "cpu", "memory"} {
		if !have[need] {
			return fmt.Errorf("cgroup v2 controller %q not available (have: %s)",
				need, strings.TrimSpace(string(raw)))
		}
	}
	return nil
}

// CreateHierarchy creates the parent cgroup, gives it the CPU set the plan needs, and
// enables the controllers that child cgroups will require.
//
// cpus is the plan's ParentCPUSet: the union of every CPU any microVM will use, plus the
// shared VMM pool. mems is the set of NUMA nodes those CPUs belong to.
func CreateHierarchy(name, cpus, mems string) (*Hierarchy, error) {
	if name == "" {
		return nil, errors.New("hierarchy name must not be empty")
	}
	if strings.ContainsAny(name, "/.") {
		// A name containing a separator would escape the intended location.
		return nil, fmt.Errorf("hierarchy name %q must be a single path segment", name)
	}
	if cpus == "" {
		return nil, errors.New("parent cpuset must not be empty")
	}

	// A cgroup's cpuset.* files only exist if its *parent* lists cpuset in
	// cgroup.subtree_control. Amazon Linux 2023 ships a root cgroup with
	// "cpu io memory pids" and no cpuset, so a freshly created child has no cpuset.cpus at
	// all and the write fails with EACCES. Enable it at the root first.
	if err := enableRootCpuset(); err != nil {
		return nil, err
	}

	path := filepath.Join(cgroupRoot, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create parent cgroup %s: %w", path, err)
	}

	h := &Hierarchy{Name: name, Path: path}

	// Assign CPUs and memory nodes before enabling subtree control: a child cannot be
	// given CPUs the parent does not hold.
	if err := writeCgroupFile(path, "cpuset.cpus", cpus); err != nil {
		return nil, err
	}
	if mems != "" {
		if err := writeCgroupFile(path, "cpuset.mems", mems); err != nil {
			return nil, err
		}
	}

	// Delegate cpuset, cpu, and memory to children so per-microVM cgroups can set their
	// own cpuset.cpus and memory.max. Without cpuset delegation, writes to a child's
	// cpuset.cpus fail; without memory delegation, the per-microVM memory.max that stops
	// one guest from OOMing the whole host cannot be set.
	if err := writeCgroupFile(path, "cgroup.subtree_control", "+cpuset +cpu +memory"); err != nil {
		return nil, fmt.Errorf("enable subtree controllers: %w", err)
	}

	return h, nil
}

// SetExclusive declares which of the parent's CPUs may be handed to child partitions.
//
// This is required before a child can become a partition root. Only CPUs listed in the
// parent's cpuset.cpus.exclusive.effective are eligible, so skipping this step makes every
// child partition request fail as invalid.
func (h *Hierarchy) SetExclusive(cpus string) error {
	return writeCgroupFile(h.Path, "cpuset.cpus.exclusive", cpus)
}

// SetPartition sets the partition mode on the parent itself.
func (h *Hierarchy) SetPartition(mode PartitionMode) error {
	if err := writeCgroupFile(h.Path, "cpuset.cpus.partition", string(mode)); err != nil {
		return err
	}
	return h.verifyPartition(mode)
}

// CreateChild creates a per-microVM cgroup under the parent.
//
// The jailer will move the Firecracker process into this cgroup, but the caller creates it
// here so partition state can be set from the parent side, which the jailer cannot do.
func (h *Hierarchy) CreateChild(id, cpus, mems string) (string, error) {
	if strings.ContainsAny(id, "/.") {
		return "", fmt.Errorf("child id %q must be a single path segment", id)
	}
	path := filepath.Join(h.Path, id)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("create child cgroup %s: %w", path, err)
	}
	if cpus != "" {
		if err := writeCgroupFile(path, "cpuset.cpus", cpus); err != nil {
			return "", err
		}
	}
	if mems != "" {
		if err := writeCgroupFile(path, "cpuset.mems", mems); err != nil {
			return "", err
		}
	}
	return path, nil
}

// SetChildPartition sets partition mode on a child cgroup.
//
// This exists as a method on Hierarchy rather than a free function to make the ownership
// explicit: the write targets the child's file, but the capability belongs to the parent,
// which must already be a valid partition root with the CPU declared exclusive.
func (h *Hierarchy) SetChildPartition(id string, mode PartitionMode) error {
	path := filepath.Join(h.Path, id)
	if err := writeCgroupFile(path, "cpuset.cpus.partition", string(mode)); err != nil {
		return err
	}
	return verifyPartitionAt(path, mode)
}

// VMMOverheadMiB is the memory budget a microVM's cgroup gets on top of its guest RAM.
//
// The guest can touch at most its configured memory size; everything above that is the
// VMM's own footprint (Firecracker text/heap, page tables for the guest mapping, virtio
// queues, the API server). Upstream measures the VMM overhead in single-digit MiB; 128 MiB
// leaves generous room without meaningfully denting density arithmetic (at 180 microVMs it
// reserves 22.5 GiB on a 768 GiB host).
const VMMOverheadMiB = 128

// ChildMemoryMaxBytes is the memory.max value for a microVM with the given guest memory.
func ChildMemoryMaxBytes(guestMemMiB int) int64 {
	return int64(guestMemMiB+VMMOverheadMiB) * 1024 * 1024
}

// SetChildMemoryMax caps a child cgroup's memory and forbids it swap.
//
// cpuset confinement alone does not bound memory: without memory.max, one guest that finds
// a way to balloon its host-side footprint (or one misconfigured launch) competes for all
// host RAM and the kernel's OOM killer starts shooting co-tenants. This cap turns that
// blast radius into a single microVM kill.
//
// Unlike partition state, which degrades to plain pinning, this is a security control for
// untrusted workloads, so failures are returned rather than logged away. memory.swap.max
// is best-effort only in one narrow case: the file does not exist on kernels built without
// swap support, where there is nothing to forbid.
func (h *Hierarchy) SetChildMemoryMax(id string, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("memory.max for %q must be positive, got %d", id, maxBytes)
	}
	path := filepath.Join(h.Path, id)
	if err := writeCgroupFile(path, "memory.max", fmt.Sprintf("%d", maxBytes)); err != nil {
		return err
	}
	// Swap is disabled host-wide by provisioning, and the sweep fails cells that see swap
	// in use; this write is the per-cgroup belt to that host-level suspenders.
	if err := writeCgroupFile(path, "memory.swap.max", "0"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // kernel built without swap: nothing to forbid
		}
		return err
	}
	return nil
}

// Destroy removes the hierarchy and all children.
//
// A cgroup cannot be removed while it holds processes, so callers must stop every VMM
// first. Leftover cgroups accumulate across sweep configurations and eventually make CPUs
// unavailable, so cleanup is not optional.
//
// Removal uses rmdir semantics. On a real cgroup filesystem a directory is removable even
// though it appears to contain control files, because those files are kernel-synthesised
// rather than real entries. removeCgroupDir accounts for the difference so this logic can
// also be exercised against an ordinary directory tree in tests.
func (h *Hierarchy) Destroy() error {
	entries, err := os.ReadDir(h.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(h.Path, e.Name())
		// Reset partition state before removal: an isolated partition holds its CPUs
		// exclusively, and removing it without releasing them can leave the parent's
		// effective set wrong.
		_ = writeCgroupFile(child, "cpuset.cpus.partition", string(PartitionMember))
		if err := removeCgroupDir(child); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove child cgroup %s: %w", child, err)
		}
	}
	_ = writeCgroupFile(h.Path, "cpuset.cpus.partition", string(PartitionMember))
	if err := removeCgroupDir(h.Path); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("remove parent cgroup %s: %w", h.Path, err)
	}
	return firstErr
}

// removeCgroupDir removes a cgroup directory.
//
// The plain rmdir is tried first, which is the only operation the kernel permits on a real
// cgroup directory. If that reports the directory is not empty, the path is not a cgroup
// filesystem, so fall back to removing the contained files. Doing this in the opposite
// order would be wrong: on a real cgroupfs, unlinking a control file fails and would mask
// an otherwise successful removal.
func removeCgroupDir(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	entries, readErr := os.ReadDir(path)
	if readErr != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // subdirectories are handled by the caller
		}
		if rmErr := os.Remove(filepath.Join(path, e.Name())); rmErr != nil {
			return err
		}
	}
	return os.Remove(path)
}

// verifyPartition reads back the partition state.
//
// Writing the file can succeed while the kernel refuses the transition, in which case the
// file reads back as "member" or reports an invalid state in parentheses. Silently
// accepting that would mean running the experiment without the isolation it claims to test.
func (h *Hierarchy) verifyPartition(want PartitionMode) error {
	return verifyPartitionAt(h.Path, want)
}

func verifyPartitionAt(path string, want PartitionMode) error {
	raw, err := os.ReadFile(filepath.Join(path, "cpuset.cpus.partition"))
	if err != nil {
		return fmt.Errorf("read back partition state: %w", err)
	}
	got := strings.TrimSpace(string(raw))
	if got == string(want) {
		return nil
	}
	// The kernel reports failures as, for example, "isolated invalid (Invalid cpuset.cpus)".
	if strings.Contains(got, "invalid") {
		return fmt.Errorf("partition %s rejected by kernel: %s", want, got)
	}
	return fmt.Errorf("partition state is %q after requesting %q", got, want)
}

func writeCgroupFile(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s = %q: %w", path, value, err)
	}
	return nil
}

// enableRootCpuset adds cpuset to the root cgroup's subtree_control if it is missing.
//
// Without this, a newly created child cgroup has no cpuset.cpus file, and writing to it fails
// with a permission error that looks like a privilege problem rather than a delegation one.
// Amazon Linux 2023 is affected: its root subtree_control is "cpu io memory pids".
//
// Writing an already-enabled controller is harmless, so the read-first check is only to avoid
// a confusing error on hosts where the write is refused for an unrelated reason.
func enableRootCpuset() error {
	path := filepath.Join(cgroupRoot, "cgroup.subtree_control")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, c := range strings.Fields(string(raw)) {
		if c == "cpuset" {
			return nil
		}
	}
	if err := os.WriteFile(path, []byte("+cpuset"), 0o644); err != nil {
		return fmt.Errorf("enable cpuset in root subtree_control (%s): %w", path, err)
	}
	return nil
}
