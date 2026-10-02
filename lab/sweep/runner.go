// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package sweep

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/firecracker"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/launcher"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/measure"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/vsock"
)

// Config is everything the runner needs to execute a sweep.
type Config struct {
	// Artifacts
	FirecrackerBin string
	JailerBin      string
	KernelPath     string
	RootfsPath     string
	ChrootBase     string

	// Matrix
	Configs   []placement.Config
	Densities []int
	Workload  WorkloadSpec

	// Repeats is how many times the whole matrix runs. One repeat yields no variance
	// information, so any difference between configurations is indistinguishable from
	// run-to-run noise.
	Repeats int

	// ShuffleCells randomises cell order within each repeat.
	//
	// Cells run back to back share thermal state and page cache with whatever ran before them,
	// so a fixed order can bake an ordering artifact into the results: B@90 always following
	// B@45 means any effect of that adjacency is inseparable from the configuration. Shuffling
	// spreads the artifact across cells instead of concentrating it.
	ShuffleCells bool

	// ShuffleSeed makes the shuffle reproducible. Recorded in results so an ordering can be
	// replayed. Zero picks a seed from the clock and records what it picked.
	ShuffleSeed int64

	// CgroupName is the parent cgroup created for each cell.
	CgroupName string
	// Placement tunes the VMM pool and host reserve.
	Placement placement.Options

	// BaseUID is the first unprivileged uid handed to a jailed microVM. Upstream recommends
	// a distinct uid per microVM so a jailbreak cannot reach a sibling's files, so the
	// runner allocates BaseUID+index.
	BaseUID int
	BaseGID int

	// LaunchConcurrency bounds how many microVMs are started at once.
	//
	// Unbounded launching would measure the launch storm rather than steady-state density,
	// and jail creation slows down superlinearly with concurrency (upstream reports roughly
	// 10x with 10 parallel jails on a host with many mount points).
	LaunchConcurrency int

	// Timeouts
	BootTimeout time.Duration

	// RequestTimeout bounds one workload request, so it must exceed the longest a single task can
	// legitimately take. The default suits the synthetic workloads, whose episodes are seconds.
	// A real task carries its own limit -- a Terminal-Bench task.yaml declares
	// max_test_timeout_sec, typically 180 -- and a request cut off below that is recorded as a
	// failed task rather than a slow one, which silently turns a latency result into a wrong
	// pass rate. Set it from the task's own limit when running real tasks.
	RequestTimeout time.Duration

	// SettleTime is how long to wait after the fleet is up before measuring memory, so
	// figures reflect steady state rather than the tail of boot activity.
	SettleTime time.Duration

	// RequestedThreadsPerCore is the EC2 CpuOptions ThreadsPerCore the instance was launched
	// with. Recorded in results so an SMT-on and an SMT-off dataset are distinguishable, and
	// so a mismatch against the observed value is detectable. Zero means unstated.
	RequestedThreadsPerCore int

	// RunLabel names this host within a multi-host experiment, for example "smt-on".
	RunLabel string

	// Log receives progress lines. A sweep runs for tens of minutes on paid hardware, so
	// silence is indistinguishable from a hang.
	Log func(format string, args ...any)
}

// DefaultConfig returns sensible defaults for the lab plan.
func DefaultConfig() Config {
	return Config{
		FirecrackerBin:    "/usr/local/bin/firecracker",
		JailerBin:         "/usr/local/bin/jailer",
		KernelPath:        "/opt/firecracker/vmlinux",
		RootfsPath:        "/opt/firecracker/rootfs.ext4",
		ChrootBase:        "/srv/jailer",
		CgroupName:        "fclab",
		Placement:         placement.DefaultOptions(),
		BaseUID:           20000,
		BaseGID:           20000,
		LaunchConcurrency: 16,
		BootTimeout:       10 * time.Second,
		RequestTimeout:    120 * time.Second,
		SettleTime:        3 * time.Second,
		// Five repeats gives a median-of-five p99 and a range to report, which is the point
		// where a difference between configurations can be stated rather than suggested.
		// The full matrix runs in a couple of minutes, so this is minutes of hardware time.
		Repeats:      5,
		ShuffleCells: true,
		Workload: WorkloadSpec{
			Iterations: 5_000_000,
			Rounds:     5,
			// 450 samples puts the nearest-rank p99 at rank 446 of 450, comfortably clear of
			// the maximum, and costs almost nothing at low density where rounds are cheap.
			TargetSamples: 450,
			VCPUCount:     1,
			MemSizeMiB:    128,
		},
		Log: func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
}

// Validate checks the configuration before any hardware time is spent.
func (c Config) Validate() error {
	// Logical checks first, then filesystem checks. A contradictory flag combination is a
	// mistake the caller can fix anywhere; a missing artifact is only fixable on a provisioned
	// host. Reporting the artifact first would hide a flag conflict from anyone iterating on a
	// workstation, which is exactly where the conflict gets introduced.
	if len(c.Configs) == 0 {
		return errors.New("no placement configurations selected")
	}
	if len(c.Densities) == 0 {
		return errors.New("no density levels selected")
	}
	if c.Workload.Rounds < 1 {
		return errors.New("workload rounds must be at least 1")
	}
	if c.Workload.TargetSamples < 0 {
		return errors.New("workload target samples cannot be negative")
	}
	if e := c.Workload.Episode; e != nil {
		if e.InstanceID == "" {
			return errors.New("episode instance ID is empty; a dataset must state which trajectory produced it")
		}
		// Without a turn count there is nothing to check a replay against, and an episode that ran
		// three of fourteen turns would be recorded as a fast episode. The count is free -- it comes
		// from the trajectory -- so requiring it costs nothing and closes the trap.
		if e.Turns <= 0 {
			return errors.New("episode turn count must be stated (-episode-turns); without it an " +
				"episode that stopped partway looks like a cheap one rather than a broken one")
		}
		if e.ExpectedTurnsOK > e.Turns {
			return fmt.Errorf("episode expects %d successful turns out of %d", e.ExpectedTurnsOK, e.Turns)
		}
		// An episode is tens of turns plus a verifier, so it is far longer than a task run and the
		// default request timeout is far more likely to cut it off. A cut-off request counts as a
		// failed episode, which corrupts the pass rate rather than merely slowing the sweep.
		if e.TimeoutSec > 0 {
			limit := time.Duration(e.TimeoutSec * float64(time.Second))
			if c.RequestTimeout < limit {
				return fmt.Errorf(
					"request timeout %v is below episode %s's own limit of %v; an episode running "+
						"longer would be recorded as failed rather than slow. Raise -request-timeout",
					c.RequestTimeout, e.InstanceID, limit)
			}
		}
		// An episode ends in a verifier run, so setting both is a caller who wants the whole episode
		// and would silently get only its last step measured under the wrong label.
		if c.Workload.Task != nil {
			return errors.New("both -episode-instance and -task-instance set; an episode already " +
				"ends in a verifier run, so pick one")
		}
	}
	if t := c.Workload.Task; t != nil {
		if t.InstanceID == "" {
			return errors.New("task instance ID is empty; a dataset must state which task produced it")
		}
		// Task overrides every other workload, so setting both is a mistake worth naming rather
		// than silently resolving. Someone who passed both probably expected the sandbox.
		if c.Workload.Sandbox != nil {
			return errors.New("both -task-instance and -resident-kib set; a real task overrides " +
				"the sandbox workload, so pick one")
		}
		if c.Workload.WorkingSetKiB > 0 {
			return errors.New("both -task-instance and -working-set-kib set; a real task overrides " +
				"the pointer chase, so pick one")
		}
		// Refuse a request timeout below the task's own limit before spending hardware time. The
		// guest has no timeout by design -- bounding a hung suite is the host's job, since only the
		// host knows how long a cell may take -- so a low value here cuts the request off and the
		// task is counted as failed. That produces a wrong pass rate, and a pass rate is the one
		// number that decides whether a latency figure may be quoted at all.
		if t.TimeoutSec > 0 {
			taskLimit := time.Duration(t.TimeoutSec * float64(time.Second))
			if c.RequestTimeout < taskLimit {
				return fmt.Errorf(
					"request timeout %v is below task %s's own limit of %v; a task that runs longer "+
						"would be recorded as failed rather than slow. Raise -request-timeout",
					c.RequestTimeout, t.InstanceID, taskLimit)
			}
		}
	}
	if s := c.Workload.Sandbox; s != nil {
		if s.ResidentKiB < 0 || s.Steps < 0 || s.WriteKiB < 0 {
			return errors.New("sandbox resident, steps, and write sizes cannot be negative")
		}
		// Refuse an undersized guest before spending hardware time. An OOM kill mid-episode
		// presents as a microVM that stopped answering, and at density it would look like a
		// spurious density ceiling rather than a configuration error.
		if floor := s.GuestMemoryFloorMiB(); c.Workload.MemSizeMiB < floor {
			return fmt.Errorf(
				"guest memory %d MiB is too small for a sandbox holding %d KiB resident plus "+
					"%d KiB of writes; needs at least %d MiB. Raise -mem or lower -resident-kib",
				c.Workload.MemSizeMiB, s.ResidentKiB, s.WriteKiB, floor)
		}
	}
	if c.Repeats < 1 {
		return errors.New("repeats must be at least 1")
	}
	if c.BaseUID <= 0 || c.BaseGID <= 0 {
		return errors.New("BaseUID and BaseGID must be non-root positive values")
	}

	for _, f := range []struct{ name, path string }{
		{"firecracker binary", c.FirecrackerBin},
		{"jailer binary", c.JailerBin},
		{"guest kernel", c.KernelPath},
		{"rootfs image", c.RootfsPath},
	} {
		if f.path == "" {
			return fmt.Errorf("%s path is empty", f.name)
		}
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("%s not found at %s: %w", f.name, f.path, err)
		}
	}
	return nil
}

// Runner executes a sweep.
type Runner struct {
	cfg Config
	top *topology.Topology
}

// NewRunner discovers topology and prepares a runner.
func NewRunner(cfg Config) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := launcher.EnsureV2(); err != nil {
		return nil, fmt.Errorf("cgroup v2 required: %w", err)
	}
	top, err := topology.Discover()
	if err != nil {
		return nil, fmt.Errorf("discover topology: %w", err)
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &Runner{cfg: cfg, top: top}, nil
}

// Run executes the full matrix.
//
// A failing cell does not abort the sweep: hardware time is expensive, and a configuration
// that fails at high density is itself a result. The failure is recorded in the cell and the
// run continues.
func (r *Runner) Run(ctx context.Context) (*Results, error) {
	results := &Results{
		Host:      r.hostInfo(),
		Workload:  r.cfg.Workload,
		Repeats:   r.cfg.Repeats,
		StartedAt: time.Now(),
	}

	cells := r.buildMatrix(results)
	if len(cells) == 0 {
		results.FinishedAt = time.Now()
		results.Duration = results.FinishedAt.Sub(results.StartedAt)
		return results, errors.New("no runnable cells: every configuration was skipped")
	}

	seed := r.cfg.ShuffleSeed
	if r.cfg.ShuffleCells && seed == 0 {
		seed = time.Now().UnixNano()
	}
	results.ShuffleSeed = seed

	for repeat := 0; repeat < r.cfg.Repeats; repeat++ {
		order := cells
		if r.cfg.ShuffleCells {
			// Derive a per-repeat seed so each repeat gets a different order while the whole
			// run stays reproducible from the single recorded seed.
			order = shuffled(cells, rand.New(rand.NewSource(seed+int64(repeat))))
		}

		r.cfg.Log("### repeat %d of %d (%d cells) ###", repeat+1, r.cfg.Repeats, len(order))

		for _, mc := range order {
			select {
			case <-ctx.Done():
				results.FinishedAt = time.Now()
				results.Duration = results.FinishedAt.Sub(results.StartedAt)
				return results, ctx.Err()
			default:
			}

			rounds := r.cfg.Workload.RoundsFor(mc.density)
			r.cfg.Log("=== repeat %d: config %s @ density %d (%d rounds) ===",
				repeat+1, mc.config, mc.density, rounds)

			cell, raw := r.runCell(ctx, mc.config, mc.density, repeat, rounds)
			results.Add(cell)
			results.AddSamples(raw)

			r.cfg.Log("    %d/%d responded, %d samples, mean %.1f us, p99 %.1f us, trustworthy=%t",
				cell.Responded, mc.density, cell.SampleCount,
				cell.MeanLatencyUS, cell.P99LatencyUS, cell.Trustworthy())
			for _, e := range cell.Errors {
				r.cfg.Log("    error: %s", e)
			}
		}
	}

	results.FinishedAt = time.Now()
	results.Duration = results.FinishedAt.Sub(results.StartedAt)
	return results, nil
}

// matrixCell is one config-and-density pair to run.
type matrixCell struct {
	config  placement.Config
	density int
}

// buildMatrix expands configs x densities, recording skips once rather than per repeat.
func (r *Runner) buildMatrix(results *Results) []matrixCell {
	var cells []matrixCell
	for _, cfg := range r.cfg.Configs {
		// Skip configurations the host cannot express, and say why. An absent
		// configuration would otherwise read as an oversight rather than a property of
		// the hardware.
		if cfg.RequiresSMT() && !r.top.HasSMT() {
			reason := fmt.Sprintf("requires SMT; host reports %d thread(s) per core",
				r.top.ThreadsPerCore())
			results.Skip(cfg, reason)
			r.cfg.Log("skip config %s: %s", cfg, reason)
			continue
		}
		if cfg == placement.ConfigF && r.top.SMTState == "forceoff" {
			reason := "SMT is forceoff and cannot be toggled at runtime"
			results.Skip(cfg, reason)
			r.cfg.Log("skip config %s: %s", cfg, reason)
			continue
		}
		for _, density := range r.cfg.Densities {
			cells = append(cells, matrixCell{config: cfg, density: density})
		}
	}
	return cells
}

// shuffled returns a shuffled copy, leaving the caller's slice intact so later repeats shuffle
// from the same base order rather than compounding earlier permutations.
func shuffled(cells []matrixCell, rng *rand.Rand) []matrixCell {
	out := make([]matrixCell, len(cells))
	copy(out, cells)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// vm tracks one microVM through a cell.
type vm struct {
	index    int
	id       string
	node     int    // NUMA node, retained so a slow sample can be traced to hardware
	cpuSet   string // cpuset.cpus this microVM was pinned to
	proc     *launcher.Process
	client   *firecracker.Client
	vsockUDS string
	launchMS time.Duration
	ready    bool
}

// runCell executes one configuration at one density for one repeat.
//
// rounds is passed in rather than read from the workload spec because it varies by density:
// low-density cells run more rounds so their sample count is adequate for a tail estimate.
func (r *Runner) runCell(ctx context.Context, cfg placement.Config, density, repeat, rounds int) (CellResult, []Sample) {
	cell := CellResult{
		Config:    cfg,
		Density:   density,
		Repeat:    repeat,
		RoundsRun: rounds,
		StartedAt: time.Now(),
	}
	defer func() { cell.Duration = time.Since(cell.StartedAt) }()

	addErr := func(format string, args ...any) {
		cell.Errors = append(cell.Errors, fmt.Sprintf(format, args...))
	}

	plan, err := placement.Build(r.top, cfg, density, r.cfg.Placement)
	if err != nil {
		addErr("build placement plan: %v", err)
		return cell, nil
	}
	if err := plan.Validate(r.top); err != nil {
		// An invalid plan would produce numbers that look plausible while measuring
		// something other than the intended configuration.
		addErr("placement plan failed validation: %v", err)
		return cell, nil
	}
	cell.PlanNotes = plan.Notes

	hierarchy, err := r.setupCgroups(plan)
	if err != nil {
		addErr("cgroup setup: %v", err)
		return cell, nil
	}
	// Cleanup always runs: leftover cgroups accumulate across cells and eventually make
	// CPUs unavailable to later configurations.
	defer func() {
		if err := hierarchy.Destroy(); err != nil {
			r.cfg.Log("    warning: cgroup cleanup: %v", err)
		}
	}()

	vms := r.launchFleet(ctx, plan, hierarchy, &cell)
	defer r.stopFleet(vms)

	cell.Launched = 0
	for _, v := range vms {
		if v.ready {
			cell.Launched++
		}
	}
	if cell.Launched == 0 {
		addErr("no microVMs became ready")
		return cell, nil
	}

	// Let the fleet settle before measuring, so memory figures describe steady state
	// rather than the tail of boot activity.
	select {
	case <-ctx.Done():
		addErr("cancelled before measurement")
		return cell, nil
	case <-time.After(r.cfg.SettleTime):
	}

	r.measureMemory(vms, &cell, memoryPhaseBoot)

	before, _ := measure.ReadHostCounters()
	raw := r.runWorkload(ctx, vms, &cell, rounds)
	if after, err := measure.ReadHostCounters(); err == nil && before != nil {
		cell.ContextSwitchesPerSec = after.Sub(before).ContextSwitchesPerSecond()
	}

	// Measure again now the workload has run. This is the figure a fleet is sized on: the boot
	// measurement above describes an idle microVM, and for any workload that creates its own
	// footprint the two differ by however much work it did.
	//
	// After the workload rather than sampled during it, for two reasons. Reading smaps_rollup for
	// every VMM on a timer would add work inside the timed region the sweep exists to measure, and
	// it is unnecessary: with no balloon device and nothing calling MADV_DONTNEED, guest pages stay
	// resident in the VMM's mapping once faulted, so this single read is the cell's high-water mark
	// rather than merely its final state. measureMemory checks that premise and complains if the
	// figure went down.
	//
	// Before teardown, which is the only window where the processes still exist.
	r.measureMemory(vms, &cell, memoryPhaseWorking)

	return cell, raw
}

// setupCgroups builds the parent hierarchy the plan requires.
//
// The order matters and is dictated by the kernel: assign CPUs, delegate controllers,
// declare which CPUs may be handed to child partitions, then make the parent a partition
// root. cpuset.cpus.partition is owned by the parent and is not delegatable, so none of this
// can be left to the jailer.
func (r *Runner) setupCgroups(plan *placement.Plan) (*launcher.Hierarchy, error) {
	mems := nodeListFor(r.top)

	h, err := launcher.CreateHierarchy(r.cfg.CgroupName, plan.ParentCPUSet, mems)
	if err != nil {
		return nil, err
	}
	if plan.ParentCPUSet != "" {
		if err := h.SetExclusive(plan.ParentCPUSet); err != nil {
			// Not fatal: without exclusive CPUs, child partitions cannot be created, but
			// plain cpuset pinning still works and is worth measuring. Record it rather
			// than losing the cell.
			r.cfg.Log("    warning: cpuset.cpus.exclusive: %v", err)
		}
	}
	return h, nil
}

// launchFleet starts microVMs up to the plan's size, bounded by LaunchConcurrency.
func (r *Runner) launchFleet(ctx context.Context, plan *placement.Plan, h *launcher.Hierarchy, cell *CellResult) []*vm {
	vms := make([]*vm, 0, len(plan.VMs))
	var mu sync.Mutex
	var launchSamples measure.LatencySamples

	sem := make(chan struct{}, max(1, r.cfg.LaunchConcurrency))
	var wg sync.WaitGroup

	for _, pvm := range plan.VMs {
		select {
		case <-ctx.Done():
			break
		default:
		}

		wg.Add(1)
		go func(pvm placement.VM) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			started := time.Now()
			v, err := r.launchOne(ctx, pvm, h)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// Record only the first few failures. At high density a systemic problem
				// would otherwise produce 180 near-identical lines and bury the signal.
				if len(cell.Errors) < 5 {
					cell.Errors = append(cell.Errors,
						fmt.Sprintf("launch vm%d: %v", pvm.Index, err))
				}
				if v != nil {
					vms = append(vms, v)
				}
				return
			}
			v.launchMS = time.Since(started)
			launchSamples.Add(v.launchMS)
			vms = append(vms, v)
		}(pvm)
	}
	wg.Wait()

	fillLaunch(cell, &launchSamples)
	return vms
}

// launchOne prepares a jail, starts a jailed Firecracker, and configures and boots the guest.
func (r *Runner) launchOne(ctx context.Context, pvm placement.VM, h *launcher.Hierarchy) (*vm, error) {
	id := fmt.Sprintf("vm%d", pvm.Index)
	uid := r.cfg.BaseUID + pvm.Index
	gid := r.cfg.BaseGID + pvm.Index

	// Create the per-microVM cgroup from the parent side, then let the jailer move the
	// process into it. Passing --cgroup instead would make the jailer create its own,
	// taking the microVM out from under the parent that owns partition state.
	mems := strconv.Itoa(pvm.Node)
	if pvm.Node < 0 {
		mems = nodeListFor(r.top)
	}
	if _, err := h.CreateChild(id, pvm.VCPUSet, mems); err != nil {
		return nil, fmt.Errorf("create child cgroup: %w", err)
	}

	// Cap the cgroup at guest RAM + VMM overhead so one guest cannot OOM the host and
	// take the rest of the cell's fleet with it. A failure here aborts the launch: memory
	// confinement is a security control for untrusted workloads, not a tuning knob.
	if err := h.SetChildMemoryMax(id, launcher.ChildMemoryMaxBytes(r.cfg.Workload.MemSizeMiB)); err != nil {
		return nil, fmt.Errorf("cap child cgroup memory: %w", err)
	}

	jc := launcher.JailerConfig{
		ID:             id,
		JailerBin:      r.cfg.JailerBin,
		ExecFile:       r.cfg.FirecrackerBin,
		ChrootBase:     r.cfg.ChrootBase,
		UID:            uid,
		GID:            gid,
		ParentCgroup:   filepath.Join(r.cfg.CgroupName, id),
		Daemonize:      true,
		NewPIDNS:       true,
		ResourceLimits: []string{"no-file=4096"},
	}

	// Stage the kernel and rootfs into the jail. Firecracker is chrooted, so any path it is
	// given over the API must resolve inside the jail.
	artifacts := map[string]string{
		"vmlinux":     r.cfg.KernelPath,
		"rootfs.ext4": r.cfg.RootfsPath,
	}
	if err := launcher.PrepareJail(jc, artifacts); err != nil {
		return nil, fmt.Errorf("prepare jail: %w", err)
	}

	proc, err := launcher.Launch(ctx, jc)
	if err != nil {
		return nil, err
	}

	v := &vm{index: pvm.Index, id: id, node: pvm.Node, cpuSet: pvm.VCPUSet, proc: proc}

	client := firecracker.New(proc.APISocket)
	v.client = client

	bootCtx, cancel := context.WithTimeout(ctx, r.cfg.BootTimeout)
	defer cancel()
	if err := client.WaitReady(bootCtx); err != nil {
		return v, fmt.Errorf("api socket never became ready: %w", err)
	}

	// vsock UDS lives inside the jail, so the host path includes the chroot.
	v.vsockUDS = filepath.Join(proc.ChrootDir, "v.sock")

	if err := client.PutMachineConfig(bootCtx, firecracker.MachineConfig{
		VCPUCount:  r.cfg.Workload.VCPUCount,
		MemSizeMiB: r.cfg.Workload.MemSizeMiB,
	}); err != nil {
		return v, err
	}
	if err := client.PutBootSource(bootCtx, firecracker.BootSource{
		KernelImagePath: "vmlinux",
		// 8250.nr_uarts=0 disables the serial device, which is tied to VMM stdout and can
		// otherwise consume unbounded host memory at density. panic=1 makes a panicking
		// guest exit rather than hang holding its pinned cores.
		BootArgs: "ro console= 8250.nr_uarts=0 reboot=k panic=1 pci=off init=/init",
	}); err != nil {
		return v, err
	}
	if err := client.PutDrive(bootCtx, firecracker.Drive{
		DriveID:      "rootfs",
		PathOnHost:   "rootfs.ext4",
		IsRootDevice: true,
		// Read-only so one page-cache copy serves the whole fleet, which is what makes
		// an idle warm pool cheap.
		IsReadOnly: true,
	}); err != nil {
		return v, err
	}
	if err := client.PutVsock(bootCtx, firecracker.Vsock{
		GuestCID: 3,
		UDSPath:  "v.sock",
	}); err != nil {
		return v, err
	}
	if err := client.Start(bootCtx); err != nil {
		return v, err
	}

	// Boot is not complete when Start returns. Poll the agent until it answers.
	if err := r.waitForAgent(bootCtx, v); err != nil {
		return v, fmt.Errorf("guest agent never answered: %w", err)
	}
	v.ready = true
	return v, nil
}

// waitForAgent polls the guest agent until it responds to PING.
func (r *Runner) waitForAgent(ctx context.Context, v *vm) error {
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
			reply, err := r.agentRequest(v, "PING")
			if err == nil && strings.HasPrefix(reply, "OK") {
				return nil
			}
			last = err
		}
	}
}

// agentRequest sends one line to the guest agent and returns its reply.
//
// Firecracker bridges host AF_UNIX to guest AF_VSOCK: connect to the UDS, send
// "CONNECT <port>\n", read "OK <hostport>\n", then exchange application data on the same
// connection.
func (r *Runner) agentRequest(v *vm, request string) (string, error) {
	conn, err := net.DialTimeout("unix", v.vsockUDS, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial vsock uds: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(r.cfg.RequestTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentPort); err != nil {
		return "", fmt.Errorf("send CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	// Bounded reads throughout: the guest is untrusted, and an unbounded ReadString would
	// let a compromised agent stream newline-free bytes into host memory.
	ack, err := vsock.ReadLine(br, vsock.MaxReplyBytes)
	if err != nil {
		return "", fmt.Errorf("read CONNECT ack: %w", err)
	}
	if !strings.HasPrefix(ack, "OK") {
		// Firecracker closes the connection when nothing is listening on the port, so this
		// usually means the guest agent has not started yet.
		return "", fmt.Errorf("vsock connect refused: %q", strings.TrimSpace(ack))
	}

	if _, err := fmt.Fprintf(conn, "%s\n", request); err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	reply, err := vsock.ReadLine(br, vsock.MaxReplyBytes)
	if err != nil {
		return "", fmt.Errorf("read reply: %w", err)
	}
	return strings.TrimSpace(reply), nil
}

// agentPort must match the port the guest agent listens on.
const agentPort = 5000

// runWorkload drives the workload in every ready microVM concurrently and collects latencies.
//
// Concurrency is the point: the experiment is about what happens when the whole fleet
// computes at once. Running guests sequentially would measure an idle host.
func (r *Runner) runWorkload(ctx context.Context, vms []*vm, cell *CellResult, rounds int) []Sample {
	var mu sync.Mutex
	var samples measure.LatencySamples
	var raw []Sample
	responded := map[int]bool{}

	// Stamped on every sample so a dataset states which task produced it. Empty for synthetic
	// workloads, where the column is omitted from JSON.
	instanceID := ""
	if r.cfg.Workload.Task != nil {
		instanceID = r.cfg.Workload.Task.InstanceID
	}

	var wg sync.WaitGroup
	for _, v := range vms {
		if !v.ready {
			continue
		}
		wg.Add(1)
		go func(v *vm) {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				select {
				case <-ctx.Done():
					return
				default:
				}
				reply, err := r.agentRequest(v, r.cfg.Workload.Request())
				if err != nil {
					mu.Lock()
					if len(cell.Errors) < 5 {
						cell.Errors = append(cell.Errors,
							fmt.Sprintf("workload vm%d round %d: %v", v.index, round, err))
					}
					mu.Unlock()
					return
				}
				rr, ok := parseRunReply(reply)
				if !ok {
					mu.Lock()
					if len(cell.Errors) < 5 {
						cell.Errors = append(cell.Errors,
							fmt.Sprintf("workload vm%d: unparsable reply %q", v.index, reply))
					}
					mu.Unlock()
					return
				}
				// An agent too old to know a verb answers with an ERR that fails to parse,
				// but one could answer the wrong workload with a well-formed reply. That
				// would record, say, compute-loop timings under a sandbox label and produce
				// a null result for entirely the wrong reason. Compare the reply tag against
				// what was asked and refuse the cell on a mismatch.
				if want := r.cfg.Workload.Kind(); want != rr.Kind {
					mu.Lock()
					if len(cell.Errors) < 5 {
						cell.Errors = append(cell.Errors, fmt.Sprintf(
							"workload vm%d: requested %q but the guest answered %q (%q); "+
								"the agent does not match the requested workload",
							v.index, want, rr.Kind, reply))
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				// Count task outcomes before recording latency, so a cell whose tasks all failed is
				// marked untrustworthy even though its (short) durations parsed fine.
				if rr.HasExitCode {
					cell.TaskRuns++
					want := 0
					switch {
					case r.cfg.Workload.Episode != nil:
						want = r.cfg.Workload.Episode.ExpectedExit
					case r.cfg.Workload.Task != nil:
						want = r.cfg.Workload.Task.ExpectedExit
					}
					if rr.ExitCode == want {
						cell.TaskPassed++
					} else if len(cell.Errors) < 5 {
						cell.Errors = append(cell.Errors, fmt.Sprintf(
							"task vm%d round %d: exit %d, want %d",
							v.index, round, rr.ExitCode, want))
					}
				}
				// An episode that ran fewer turns than the trajectory has stopped executing partway,
				// which makes it *fast*. Counted as a failed run so the cell's pass rate reflects it:
				// without this an episode that did a fraction of the work would be quoted as a cheap
				// episode rather than a broken one.
				if e := r.cfg.Workload.Episode; e != nil && e.Turns > 0 && rr.Turns != e.Turns {
					cell.EpisodeTurnShortfalls++
					if len(cell.Errors) < 5 {
						cell.Errors = append(cell.Errors, fmt.Sprintf(
							"episode vm%d round %d: ran %d turns, trajectory has %d",
							v.index, round, rr.Turns, e.Turns))
					}
				}
				samples.Add(rr.Duration())
				raw = append(raw, Sample{
					Config:        cell.Config,
					Density:       cell.Density,
					Repeat:        cell.Repeat,
					VMIndex:       v.index,
					Node:          v.node,
					Round:         round,
					DurationUS:    float64(rr.TotalUS),
					GuestMeanNS:   rr.MeanNS,
					Checksum:      rr.Checksum,
					CPUSet:        v.cpuSet,
					WorkingSetKiB: rr.WorkingSetKiB,
					SetupUS:       float64(rr.SetupUS),
					SpawnUS:       float64(rr.SpawnUS),
					WriteUS:       float64(rr.WriteUS),
					SpawnsOK:      rr.SpawnsOK,
					ExitCode:      rr.ExitCode,
					PeakRSSKiB:    rr.PeakRSSKiB,
					Turns:         rr.Turns,
					TurnsOK:       rr.TurnsOK,
					TurnsUS:       float64(rr.TurnsUS),
					VerifyUS:      float64(rr.VerifyUS),
					InstanceID:    instanceID,
				})
				responded[v.index] = true
				mu.Unlock()
			}
		}(v)
	}
	wg.Wait()

	cell.Responded = len(responded)
	fillLatency(cell, &samples)
	return raw
}

// Request renders the agent request line for this workload.
func (w WorkloadSpec) Request() string {
	// Checked most-specific first. Episode subsumes Task -- an episode ends in a verifier run, so a
	// caller who set both wants the whole thing, not the last step of it. Task in turn subsumes
	// every synthetic workload, and Sandbox subsumes the two synthetic loops.
	if w.Episode != nil {
		return "RUN_EPISODE"
	}
	if w.Task != nil {
		return "RUN_TASK"
	}
	if w.Sandbox != nil {
		return fmt.Sprintf("RUN_SANDBOX %d %d %d",
			w.Sandbox.ResidentKiB, w.Sandbox.Steps, w.Sandbox.WriteKiB)
	}
	if w.WorkingSetKiB > 0 {
		return fmt.Sprintf("RUN_CACHE %d %d", w.WorkingSetKiB, w.Iterations)
	}
	return fmt.Sprintf("RUN %d", w.Iterations)
}

// Kind names the workload, for reports and for the reply-shape guard.
func (w WorkloadSpec) Kind() string {
	switch {
	case w.Episode != nil:
		return "episode"
	case w.Task != nil:
		return "task"
	case w.Sandbox != nil:
		return "sandbox"
	case w.WorkingSetKiB > 0:
		return "cache"
	default:
		return "compute"
	}
}

// parseRunReply parses either workload's reply:
//
//	OK  <iterations> <total_us> <mean_ns> <checksum>
//	OKC <ws_kib> <iterations> <chase_us> <mean_ns> <setup_us> <checksum>
//
// The checksum is required, not optional: without it a reply could come from a guest whose
// loop was optimised away, which would present as an implausibly fast host rather than as a
// bug. It is also retained in the raw sample so the proof travels with the data.
//
// The two tags are distinct so a cache reply can never be read as a compute reply. Field
// positions differ, so a shared tag would let the host record a working-set size as an
// iteration count and compare measurements that are not comparable.
func parseRunReply(reply string) (runReply, bool) {
	fields := strings.Fields(reply)
	if len(fields) == 0 {
		return runReply{}, false
	}

	switch fields[0] {
	case "OK":
		if len(fields) < 5 {
			return runReply{}, false
		}
		iters, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		totalUS, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		meanNS, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		checksum, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		return runReply{
			Iterations: iters, TotalUS: totalUS, MeanNS: meanNS, Checksum: checksum,
			Kind: "compute",
		}, true

	case "OKT":
		// OKT <exit_code> <total_us> <setup_us> <peak_rss_kib>
		//
		// exit_code is signed and may legitimately be non-zero, so it is parsed separately from
		// the unsigned fields rather than folded into the loop below.
		if len(fields) < 5 {
			return runReply{}, false
		}
		exitCode, err := strconv.ParseInt(fields[1], 10, 32)
		if err != nil {
			return runReply{}, false
		}
		totalUS, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		setupUS, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		peakRSS, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		return runReply{
			TotalUS:    totalUS,
			Kind:       "task",
			ExitCode:   int(exitCode),
			SetupUS:    setupUS,
			PeakRSSKiB: peakRSS,
			// No checksum: the exit code is the proof the work ran, and it carries more
			// information than a checksum would.
			HasExitCode: true,
		}, true

	case "OKE":
		// OKE <turns> <turns_ok> <total_us> <turns_us> <verify_us> <exit_code> <peak_rss_kib>
		//
		// A distinct tag from OKT even though both carry an exit code, because the field layout is
		// completely different: reading one as the other would report a turn count as a latency.
		if len(fields) < 8 {
			return runReply{}, false
		}
		turns, err := strconv.ParseInt(fields[1], 10, 32)
		if err != nil {
			return runReply{}, false
		}
		turnsOK, err := strconv.ParseInt(fields[2], 10, 32)
		if err != nil {
			return runReply{}, false
		}
		totalUS, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		turnsUS, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		verifyUS, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		// Signed, and legitimately non-zero: a rollout that failed its instance is a real result.
		exitCode, err := strconv.ParseInt(fields[6], 10, 32)
		if err != nil {
			return runReply{}, false
		}
		peakRSS, err := strconv.ParseUint(fields[7], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		return runReply{
			TotalUS:     totalUS,
			Kind:        "episode",
			Turns:       int(turns),
			TurnsOK:     int(turnsOK),
			TurnsUS:     turnsUS,
			VerifyUS:    verifyUS,
			ExitCode:    int(exitCode),
			PeakRSSKiB:  peakRSS,
			HasExitCode: true,
		}, true

	case "OKS":
		// OKS <resident_kib> <steps> <write_kib> <total_us> <spawn_us> <write_us> <spawns_ok>
		//     <checksum> <peak_rss_kib>
		//
		// peak_rss_kib was added 2026-09-18. The check is exact rather than a minimum precisely
		// because of that: an agent predating it answers with nine fields, which a `< 9` check
		// would accept while silently reporting a zero peak. A stale staged agent has cost this
		// project three sessions already, so it has to fail as a parse error instead.
		if len(fields) != 10 {
			return runReply{}, false
		}
		nums := make([]uint64, 0, 9)
		for _, f := range fields[1:10] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return runReply{}, false
			}
			nums = append(nums, v)
		}
		return runReply{
			// TotalUS is the whole episode, which is the comparable latency figure. Spawn and
			// write are components of it, kept separately so the fork and writeback costs can
			// be read off rather than inferred.
			TotalUS:     int64(nums[3]),
			Checksum:    nums[7],
			Kind:        "sandbox",
			ResidentKiB: nums[0],
			Steps:       nums[1],
			WriteKiB:    nums[2],
			SpawnUS:     int64(nums[4]),
			WriteUS:     int64(nums[5]),
			SpawnsOK:    nums[6],
			// The agent's own VmHWM, not a child's rusage. The sandbox holds its resident buffer
			// in the agent process and spawns only trivial children, so this is the one verb
			// where the agent's own footprint is the figure that matters.
			PeakRSSKiB: nums[8],
		}, true

	case "OKC":
		if len(fields) < 7 {
			return runReply{}, false
		}
		wsKiB, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		iters, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		// The chase duration is the measurement. Setup is parsed and kept separately so it can
		// be confirmed to be outside the timed region rather than assumed to be.
		chaseUS, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		meanNS, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		setupUS, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		checksum, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil {
			return runReply{}, false
		}
		return runReply{
			Iterations:    iters,
			TotalUS:       chaseUS,
			MeanNS:        meanNS,
			Checksum:      checksum,
			WorkingSetKiB: wsKiB,
			SetupUS:       setupUS,
			Kind:          "cache",
		}, true
	}
	return runReply{}, false
}

// Duration is the guest-reported wall clock for the workload.
func (r runReply) Duration() time.Duration {
	return time.Duration(r.TotalUS) * time.Microsecond
}

// memoryPhase selects which set of CellResult memory fields a measurement lands in.
//
// Two phases rather than one because boot cost and working-set cost answer different questions:
// what an idle warm-pool microVM costs, and what one doing work costs. Before 2026-09-18 only the
// first was recorded, under unqualified names, so a workload that created its own footprint was
// reported at its idle size.
type memoryPhase string

const (
	// memoryPhaseBoot is after the settle, with every microVM up and idle.
	memoryPhaseBoot memoryPhase = "boot"
	// memoryPhaseWorking is after the workload finishes, before teardown.
	memoryPhaseWorking memoryPhase = "working"
)

// measureMemory records fleet memory into the fields for the given phase.
func (r *Runner) measureMemory(vms []*vm, cell *CellResult, phase memoryPhase) {
	var pids []int
	for _, v := range vms {
		if !v.ready || v.proc == nil {
			continue
		}
		if pid, err := v.proc.PID(); err == nil {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return
	}

	fm, err := measure.ReadFleetMemory(pids)
	if err != nil {
		cell.Errors = append(cell.Errors, fmt.Sprintf("read fleet memory (%s): %v", phase, err))
		return
	}

	switch phase {
	case memoryPhaseBoot:
		cell.FleetRSSKiBBoot = fm.TotalRSSKiB
		cell.FleetPSSKiBBoot = fm.TotalPSSKiB
		cell.PerVMPrivateDirtyKiBBoot = fm.PerVMPrivateDirtyKiB
		cell.SharingRatioBoot = fm.SharingRatio()
	case memoryPhaseWorking:
		cell.FleetRSSKiBWorking = fm.TotalRSSKiB
		cell.FleetPSSKiBWorking = fm.TotalPSSKiB
		cell.PerVMPrivateDirtyKiBWorking = fm.PerVMPrivateDirtyKiB
		cell.SharingRatioWorking = fm.SharingRatio()

		cell.CheckWorkingSetMonotonic(fm.TotalPSSKiB)
	}

	if hm, err := measure.ReadHostMemory(); err == nil {
		cell.HostCachedKiB = hm.CachedKiB
		if hm.SwapInUse() {
			// Swap in use means guest memory can reach host storage, which invalidates the
			// security posture the provisioning scripts establish.
			cell.Errors = append(cell.Errors, "swap is in use; guest memory may reach host storage")
		}
	}
}

// stopFleet terminates every microVM.
//
// SIGKILL is the only reliable stop: a guest without an ACPI handler ignores Ctrl+Alt+Del,
// and a VMM wedged by the documented signal-handler deadlock will not respond to anything
// softer.
func (r *Runner) stopFleet(vms []*vm) {
	var wg sync.WaitGroup
	for _, v := range vms {
		if v.proc == nil {
			continue
		}
		wg.Add(1)
		go func(v *vm) {
			defer wg.Done()
			_ = v.proc.Stop()
			// Remove the jail so successive cells do not accumulate chroot directories,
			// each holding a hard link to the kernel and rootfs.
			_ = os.RemoveAll(filepath.Dir(v.proc.ChrootDir))
		}(v)
	}
	wg.Wait()
}

func (r *Runner) hostInfo() HostInfo {
	h := HostInfo{
		LogicalCPUs:    len(r.top.CPUs),
		PhysicalCores:  r.top.PhysicalCores(),
		ThreadsPerCore: r.top.ThreadsPerCore(),
		NUMANodes:      len(r.top.Nodes),
		L3Domains:      len(r.top.L3Domains),
		CacheMode:      string(r.top.CacheMode()),
		SMTState:       r.top.SMTState,
		CPUModel:       readCPUModel(),
		HostKernel:     readKernelRelease(),
		GuestKernel:    filepath.Base(r.cfg.KernelPath),

		RequestedThreadsPerCore: r.cfg.RequestedThreadsPerCore,
		RunLabel:                r.cfg.RunLabel,

		// These two were declared but never populated, so earlier summaries printed
		// "Density sweep: " and "firecracker ," with the values missing.
		InstanceType:   readInstanceType(),
		FirecrackerVer: readFirecrackerVersion(r.cfg.FirecrackerBin),
	}
	if hm, err := measure.ReadHostMemory(); err == nil {
		h.TotalMemoryKiB = hm.TotalKiB
	}
	return h
}

func readCPUModel() string {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if name, val, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(val)
		}
	}
	return "unknown"
}

// readInstanceType queries IMDSv2. Token-based because IMDSv1 may be disabled, and the
// provision scripts install a firewall rule that blocks guests (not the host) from reaching it.
func readInstanceType() string {
	client := &http.Client{Timeout: 2 * time.Second}

	tokReq, err := http.NewRequest(http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	if err != nil {
		return "unknown"
	}
	tokReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokResp, err := client.Do(tokReq)
	if err != nil {
		return "unknown"
	}
	defer tokResp.Body.Close()
	token, err := io.ReadAll(tokResp.Body)
	if err != nil || len(token) == 0 {
		return "unknown"
	}

	req, err := http.NewRequest(http.MethodGet,
		"http://169.254.169.254/latest/meta-data/instance-type", nil)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("X-aws-ec2-metadata-token", string(token))
	resp, err := client.Do(req)
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(body))
}

// readFirecrackerVersion shells out to the binary, since the version is not otherwise
// discoverable and it must match for snapshots to be portable.
func readFirecrackerVersion(bin string) string {
	// bin is cfg.FirecrackerBin: an operator-set path (defaults to
	// /usr/local/bin/firecracker), never guest- or network-controlled, and
	// preflight-checked to exist. The only argument is the literal "--version", and
	// exec.Command runs execve directly (no shell), so nothing is shell-interpreted.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return strings.TrimSpace(strings.TrimPrefix(line, "Firecracker "))
}

func readKernelRelease() string {
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(raw))
}

// nodeListFor renders the host's NUMA nodes as a cpuset.mems value.
func nodeListFor(t *topology.Topology) string {
	if len(t.Nodes) == 0 {
		return "0"
	}
	return topology.FormatCPUList(t.Nodes)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
