// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package orchestrator turns a placement plan into running, pinned Firecracker microVMs.
//
// It realises the second and third Host Agent duties from the architecture diagram:
// create per-microVM cgroup hierarchies, and orchestrate Firecracker jailer processes with
// per-microVM CPU pinning and NUMA-aware memory placement.
//
// The design deliberately mirrors lab/sweep's proven boot sequence rather than inventing a
// new one, and it imports the lab packages (topology, placement, launcher, firecracker)
// directly. The one constraint that shapes everything is that cpuset.cpus.partition is
// owned by the parent cgroup and is not delegatable: the agent must create the parent
// hierarchy, declare its exclusive CPUs, and set the partition mode itself, then let the
// jailer move each Firecracker process into a pre-created child cgroup via --parent-cgroup
// with no --cgroup.
package orchestrator

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/firecracker"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/launcher"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/vsock"
)

// agentPort is the vsock port the guest agent listens on. It matches the lab and the
// guest-agent's own protocol.
const agentPort = 5000

// Pinned artifact versions for this reference implementation.
const (
	// GuestKernelVersion is the guest kernel the AMI bakes and this agent boots.
	GuestKernelVersion = "6.18"
	// FirecrackerVersion is the VMM version the API client targets.
	FirecrackerVersion = "1.16.1"
)

// Config holds the orchestrator's static configuration. Paths default to the same
// locations the lab and provisioning scripts use.
type Config struct {
	// PlacementConfig selects the placement strategy (A-F). See lab/placement.
	PlacementConfig placement.Config
	// PlacementOptions tunes the VMM pool and host reserve. DefaultOptions matches the
	// lab: 8 VMM pool cores, 4 host reserve cores.
	PlacementOptions placement.Options

	CgroupName     string // parent cgroup directory name under /sys/fs/cgroup
	JailerBin      string // path to the jailer binary
	FirecrackerBin string // path to the firecracker binary
	KernelPath     string // host path to the guest kernel (staged into each jail)
	RootfsPath     string // host path to the read-only rootfs (staged into each jail)
	ChrootBase     string // jail root, e.g. /srv/jailer

	BaseUID int // per-microVM UID = BaseUID + index, per jailer guidance
	BaseGID int

	VCPUCount  int // guest vCPU count per microVM (1 in the density model)
	MemSizeMiB int // guest memory per microVM; NUMA-aware via the child cgroup's mems

	BootTimeout    time.Duration
	RequestTimeout time.Duration
}

// DefaultConfig returns configuration matching the lab defaults and this repo's scripts.
func DefaultConfig() Config {
	return Config{
		PlacementConfig:  placement.ConfigD,
		PlacementOptions: placement.DefaultOptions(),
		CgroupName:       "hostagent",
		JailerBin:        "/usr/local/bin/jailer",
		FirecrackerBin:   "/usr/local/bin/firecracker",
		KernelPath:       "/opt/firecracker/vmlinux",
		RootfsPath:       "/opt/firecracker/rootfs.ext4",
		ChrootBase:       "/srv/jailer",
		BaseUID:          100000,
		BaseGID:          100000,
		VCPUCount:        1,
		MemSizeMiB:       2048,
		BootTimeout:      30 * time.Second,
		RequestTimeout:   5 * time.Second,
	}
}

// MicroVM is one running, pinned microVM the orchestrator manages.
type MicroVM struct {
	Index    int
	ID       string
	Node     int    // NUMA node it is pinned to, -1 if unpinned
	CPUSet   string // cpuset.cpus applied
	Proc     *launcher.Process
	Client   *firecracker.Client
	VsockUDS string // host path to the vsock Unix socket inside the jail
	LaunchMS time.Duration
}

// Orchestrator owns the parent cgroup hierarchy and launches microVMs into it. It is safe
// for concurrent use.
type Orchestrator struct {
	cfg Config
	top *topology.Topology
	log *slog.Logger

	mu        sync.Mutex
	hierarchy *launcher.Hierarchy
	plan      *placement.Plan
	nextIndex int // monotonically increasing so cgroup/jail names never collide
}

// New constructs an orchestrator. It does not touch the host until SetupHierarchy runs.
func New(cfg Config, top *topology.Topology, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{cfg: cfg, top: top, log: log}
}

// Plan returns the placement plan built by SetupHierarchy, or nil if not yet built.
func (o *Orchestrator) Plan() *placement.Plan {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.plan
}

// SetupHierarchy builds a placement plan for the configured Config at the given density
// and establishes the parent cgroup hierarchy the plan requires.
//
// The ordering is dictated by the kernel and matches lab/sweep: verify cgroup v2, create
// the parent with the plan's union CPU set and the host's NUMA nodes, declare the CPUs
// exclusive, then make the parent an isolated partition root. Only after this can child
// partitions be created, because the partition flag is not delegatable to the jailer.
func (o *Orchestrator) SetupHierarchy(density int) (*placement.Plan, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	plan, err := placement.Build(o.top, o.cfg.PlacementConfig, density, o.cfg.PlacementOptions)
	if err != nil {
		return nil, fmt.Errorf("build placement plan: %w", err)
	}
	if err := plan.Validate(o.top); err != nil {
		return nil, fmt.Errorf("placement plan failed validation: %w", err)
	}
	for _, note := range plan.Notes {
		o.log.Info("placement note", "config", string(plan.Config), "note", note)
	}

	if err := launcher.EnsureV2(); err != nil {
		return nil, fmt.Errorf("cgroup v2 prerequisite: %w", err)
	}

	mems := nodeList(o.top)
	h, err := launcher.CreateHierarchy(o.cfg.CgroupName, plan.ParentCPUSet, mems)
	if err != nil {
		return nil, fmt.Errorf("create parent cgroup hierarchy: %w", err)
	}

	if plan.ParentCPUSet != "" {
		if err := h.SetExclusive(plan.ParentCPUSet); err != nil {
			// Not fatal: plain cpuset pinning still works without exclusive CPUs, but the
			// isolated partition that removes the scheduler will not. Record and continue,
			// matching the lab's tolerance.
			o.log.Warn("cpuset.cpus.exclusive rejected; child partitions unavailable", "err", err)
		} else if err := h.SetPartition(launcher.PartitionRoot); err != nil {
			o.log.Warn("parent partition root rejected; continuing with plain pinning", "err", err)
		}
	}

	o.hierarchy = h
	o.plan = plan
	o.log.Info("cgroup hierarchy established",
		"name", o.cfg.CgroupName,
		"parent_cpuset", plan.ParentCPUSet,
		"mems", mems,
		"vmm_pool", plan.VMMPool,
		"host_reserve", plan.HostReserve,
		"planned_vms", len(plan.VMs),
	)
	return plan, nil
}

// Launch boots one microVM using the placement slot at planIndex, creating its child
// cgroup from the parent side and letting the jailer move Firecracker into it.
//
// It reproduces lab/sweep's boot sequence exactly: create child cgroup, prepare jail with
// staged kernel+rootfs, launch jailer, wait for the API socket, configure machine (with
// NUMA-aware memory), boot source (kernel 6.18), read-only rootfs drive, vsock, start, then
// wait for the guest agent to answer PING.
func (o *Orchestrator) Launch(ctx context.Context, planIndex int) (*MicroVM, error) {
	o.mu.Lock()
	if o.hierarchy == nil || o.plan == nil {
		o.mu.Unlock()
		return nil, fmt.Errorf("hierarchy not set up; call SetupHierarchy first")
	}
	if planIndex < 0 || planIndex >= len(o.plan.VMs) {
		o.mu.Unlock()
		return nil, fmt.Errorf("plan index %d out of range [0,%d)", planIndex, len(o.plan.VMs))
	}
	pvm := o.plan.VMs[planIndex]
	h := o.hierarchy
	runIndex := o.nextIndex
	o.nextIndex++
	o.mu.Unlock()

	// A unique id per launch so re-launching a slot never collides with a lingering
	// cgroup or jail directory.
	id := fmt.Sprintf("vm%d", runIndex)
	uid := o.cfg.BaseUID + runIndex
	gid := o.cfg.BaseGID + runIndex

	// NUMA-aware memory: pin cpuset.mems to the microVM's node so guest memory allocates
	// locally. An unpinned baseline (node -1) gets the full node list.
	mems := nodeList(o.top)
	if pvm.Node >= 0 {
		mems = strconv.Itoa(pvm.Node)
	}

	if _, err := h.CreateChild(id, pvm.VCPUSet, mems); err != nil {
		return nil, fmt.Errorf("create child cgroup: %w", err)
	}

	// Cap the cgroup at guest RAM + VMM overhead so an untrusted guest that inflates its
	// host-side footprint OOMs alone instead of taking co-tenants with it. Unlike the
	// partition below, this is a security control: failure aborts the launch.
	if err := h.SetChildMemoryMax(id, launcher.ChildMemoryMaxBytes(o.cfg.MemSizeMiB)); err != nil {
		return nil, fmt.Errorf("cap child cgroup memory: %w", err)
	}

	// If the config carves a VMM pool or SMT sibling for VMM threads, and the parent is a
	// valid partition root, promote the child to an isolated partition so the scheduler
	// is removed from the vCPU thread. Best-effort: plain pinning is still meaningful.
	if pvm.VCPUSet != "" {
		if err := h.SetChildPartition(id, launcher.PartitionIsolated); err != nil {
			o.log.Debug("child isolated partition not applied; plain pinning in effect",
				"id", id, "err", err)
		}
	}

	jc := launcher.JailerConfig{
		ID:         id,
		JailerBin:  o.cfg.JailerBin,
		ExecFile:   o.cfg.FirecrackerBin,
		ChrootBase: o.cfg.ChrootBase,
		UID:        uid,
		GID:        gid,
		// --parent-cgroup with no --cgroup: the jailer moves Firecracker into the child
		// cgroup the agent already created, so the parent keeps ownership of partition
		// state. This is the non-delegatable partition rule in practice.
		ParentCgroup:   filepath.Join(o.cfg.CgroupName, id),
		Daemonize:      true,
		NewPIDNS:       true,
		ResourceLimits: []string{"no-file=4096"},
	}

	artifacts := map[string]string{
		"vmlinux":     o.cfg.KernelPath,
		"rootfs.ext4": o.cfg.RootfsPath,
	}
	if err := launcher.PrepareJail(jc, artifacts); err != nil {
		return nil, fmt.Errorf("prepare jail: %w", err)
	}

	started := time.Now()
	proc, err := launcher.Launch(ctx, jc)
	if err != nil {
		return nil, fmt.Errorf("launch jailer: %w", err)
	}

	vm := &MicroVM{
		Index:    runIndex,
		ID:       id,
		Node:     pvm.Node,
		CPUSet:   pvm.VCPUSet,
		Proc:     proc,
		Client:   firecracker.New(proc.APISocket),
		VsockUDS: filepath.Join(proc.ChrootDir, "v.sock"),
	}

	bootCtx, cancel := context.WithTimeout(ctx, o.cfg.BootTimeout)
	defer cancel()

	if err := vm.Client.WaitReady(bootCtx); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("api socket never became ready: %w", err)
	}
	if err := vm.Client.PutMachineConfig(bootCtx, firecracker.MachineConfig{
		VCPUCount:  o.cfg.VCPUCount,
		MemSizeMiB: o.cfg.MemSizeMiB,
	}); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("put machine-config: %w", err)
	}
	if err := vm.Client.PutBootSource(bootCtx, firecracker.BootSource{
		KernelImagePath: "vmlinux",
		// Same boot args the lab validated: no serial device (unbounded host memory at
		// density otherwise), panic=1 so a panicking guest releases its pinned cores.
		BootArgs: "ro console= 8250.nr_uarts=0 reboot=k panic=1 pci=off init=/init",
	}); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("put boot-source: %w", err)
	}
	if err := vm.Client.PutDrive(bootCtx, firecracker.Drive{
		DriveID:      "rootfs",
		PathOnHost:   "rootfs.ext4",
		IsRootDevice: true,
		// Read-only so one page-cache copy serves the whole fleet; this is what makes an
		// idle warm pool cheap.
		IsReadOnly: true,
	}); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("put drive: %w", err)
	}
	if err := vm.Client.PutVsock(bootCtx, firecracker.Vsock{
		GuestCID: 3,
		UDSPath:  "v.sock",
	}); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("put vsock: %w", err)
	}
	if err := vm.Client.Start(bootCtx); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("start microVM: %w", err)
	}
	if err := o.waitForAgent(bootCtx, vm); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("guest agent never answered: %w", err)
	}

	vm.LaunchMS = time.Since(started)
	o.log.Debug("microVM ready", "id", id, "node", pvm.Node, "cpuset", pvm.VCPUSet,
		"launch_ms", vm.LaunchMS.Milliseconds())
	return vm, nil
}

// Stop terminates one microVM and removes its child cgroup.
func (o *Orchestrator) Stop(vm *MicroVM) error {
	if vm == nil {
		return nil
	}
	var firstErr error
	if vm.Proc != nil {
		if err := vm.Proc.Stop(); err != nil {
			firstErr = err
		}
	}
	// Remove the child cgroup so leftover cgroups do not accumulate and eventually make
	// CPUs unavailable. Destroy on the whole hierarchy handles this at shutdown, but a
	// single-VM stop should clean up its own child immediately.
	o.mu.Lock()
	h := o.hierarchy
	o.mu.Unlock()
	if h != nil {
		_ = h.SetChildPartition(vm.ID, launcher.PartitionMember)
	}
	return firstErr
}

// Teardown tears down the entire hierarchy, including any remaining child cgroups. Callers
// must stop every microVM first; Destroy resets partition state before rmdir.
func (o *Orchestrator) Teardown() error {
	o.mu.Lock()
	h := o.hierarchy
	o.hierarchy = nil
	o.mu.Unlock()
	if h == nil {
		return nil
	}
	o.log.Info("tearing down cgroup hierarchy", "name", o.cfg.CgroupName)
	return h.Destroy()
}

// PlannedDensity is the number of microVM slots the current plan holds.
func (o *Orchestrator) PlannedDensity() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.plan == nil {
		return 0
	}
	return len(o.plan.VMs)
}

// waitForAgent polls the guest agent until it responds to PING, matching lab/sweep.
func (o *Orchestrator) waitForAgent(ctx context.Context, vm *MicroVM) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w (last: %v)", ctx.Err(), last)
			}
			return ctx.Err()
		case <-ticker.C:
			reply, err := o.AgentRequest(vm, "PING")
			if err == nil && strings.HasPrefix(reply, "OK") {
				return nil
			}
			last = err
		}
	}
}

// AgentRequest sends one line to a microVM's guest agent and returns its reply.
//
// Firecracker bridges host AF_UNIX to guest AF_VSOCK: connect to the UDS, send
// "CONNECT <port>\n", read the "OK <hostport>" ack, then exchange application data on the
// same connection. Verbs and replies follow the guest-agent protocol (PING/RUN/RUN_TASK/
// RUN_EPISODE -> OK/OKT/OKE/ERR).
func (o *Orchestrator) AgentRequest(vm *MicroVM, request string) (string, error) {
	conn, err := net.DialTimeout("unix", vm.VsockUDS, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial vsock uds: %w", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(o.cfg.RequestTimeout)); err != nil {
		return "", err
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentPort); err != nil {
		return "", fmt.Errorf("send CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	// Bounded reads throughout: the guest is untrusted, and an unbounded ReadString would
	// let a compromised agent stream newline-free bytes into host memory.
	if _, err := vsock.ReadLine(br, vsock.MaxReplyBytes); err != nil {
		return "", fmt.Errorf("read CONNECT ack: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", request); err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	reply, err := vsock.ReadLine(br, vsock.MaxReplyBytes)
	if err != nil {
		return "", fmt.Errorf("read reply: %w", err)
	}
	return strings.TrimRight(reply, "\r"), nil
}

// nodeList renders the host's NUMA node ids for cpuset.mems, defaulting to "0" when the
// host exposes no NUMA information. Mirrors lab/sweep's nodeListFor.
func nodeList(t *topology.Topology) string {
	if t == nil || len(t.Nodes) == 0 {
		return "0"
	}
	return topology.FormatCPUList(t.Nodes)
}
