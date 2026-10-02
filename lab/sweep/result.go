// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package sweep drives the density experiment and records results.
//
// A sweep is a matrix: each placement configuration crossed with each density level. For
// every cell it launches microVMs, drives a workload in each, and records latency and host
// metrics. The output is the parameter-tradeoff table the blog is built around.
//
// Two design commitments shape this package.
//
// Results carry their provenance. Every cell records the host it ran on, the software
// versions, and whether anything went wrong. A number without that context cannot be
// defended in review, and a reader cannot tell whether it applies to their hardware.
//
// Partial failure is recorded, not hidden. If 90 microVMs were requested and 87 launched,
// the result says so. Reporting latency for 87 as though it were 90 would understate
// contention and overstate achievable density, which is precisely the error the experiment
// exists to avoid.
package sweep

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/measure"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
)

// HostInfo describes where a sweep ran. Without this a result is not interpretable: the
// same configuration behaves differently on a host with a different NUMA layout, cache
// arrangement, or SMT state.
type HostInfo struct {
	InstanceType   string `json:"instance_type"`
	CPUModel       string `json:"cpu_model"`
	LogicalCPUs    int    `json:"logical_cpus"`
	PhysicalCores  int    `json:"physical_cores"`
	ThreadsPerCore int    `json:"threads_per_core"`
	NUMANodes      int    `json:"numa_nodes"`
	L3Domains      int    `json:"l3_domains"`
	// CacheMode records whether a cache domain spans NUMA nodes. On a host where it does,
	// node confinement gives memory locality but not cache isolation, so cache-related
	// counters must be read as whole-host signals.
	CacheMode string `json:"cache_mode"`
	SMTState  string `json:"smt_state"`

	// RequestedThreadsPerCore is the EC2 CpuOptions ThreadsPerCore value the instance was
	// launched with, as reported by the caller. CPU Options are fixed at launch and cannot
	// be changed on a running instance, so SMT state is a property of the host rather than
	// something a sweep can toggle.
	//
	// Recorded separately from the observed ThreadsPerCore because two datasets that differ
	// only by launch parameter would otherwise be indistinguishable, and because a mismatch
	// between requested and observed means the option did not take effect and the run is
	// measuring something other than what was intended. Zero means the caller did not say.
	RequestedThreadsPerCore int `json:"requested_threads_per_core,omitempty"`

	// RunLabel distinguishes datasets from separate hosts in the same experiment, for
	// example "smt-on" and "smt-off". Comparisons across labels cross physical machines,
	// so a difference may be host variance rather than the parameter under test.
	RunLabel string `json:"run_label,omitempty"`

	HostKernel     string `json:"host_kernel"`
	FirecrackerVer string `json:"firecracker_version"`
	GuestKernel    string `json:"guest_kernel"`
	TotalMemoryKiB int    `json:"total_memory_kib"`
}

// CheckWorkingSetMonotonic records an error if the working-set PSS came in below the boot PSS.
//
// The working figure is quoted as a cell's high-water mark, which is only true because no balloon
// device is configured and nothing calls MADV_DONTNEED: a page the guest faults stays resident in
// the VMM's anonymous mapping, so the total cannot fall. A decrease means that premise is false --
// a balloon appeared, a VMM restarted mid-cell, or these are not the same processes -- and then
// every working-set figure in the run describes something other than a peak.
//
// Recorded as an error rather than a Trustworthy() failure: a large *increase* is the finding this
// measurement exists to capture, and only a *decrease* contradicts the model. Equal is normal, and
// expected of a workload that touches nothing the boot did not.
//
// Extracted from measureMemory so it can be tested without live processes, which /proc cannot be
// made to fake.
func (c *CellResult) CheckWorkingSetMonotonic(workingPSSKiB int) {
	if c.FleetPSSKiBBoot <= 0 || workingPSSKiB >= c.FleetPSSKiBBoot {
		return
	}
	c.WorkingSetRegression = true
	c.Errors = append(c.Errors, fmt.Sprintf(
		"fleet PSS fell from %d KiB at boot to %d KiB after the workload; guest memory is expected "+
			"to be monotonic without a balloon device, so the working-set figures for this cell do "+
			"not describe a high-water mark",
		c.FleetPSSKiBBoot, workingPSSKiB))
}

// SMTConsistent reports whether the observed thread count matches what was requested at
// launch.
//
// A false result means the CpuOptions setting did not take effect, so the run is measuring a
// different SMT configuration than intended. That invalidates the comparison the sweep exists
// to make, which is why it is surfaced rather than left for a reader to notice.
func (h HostInfo) SMTConsistent() bool {
	if h.RequestedThreadsPerCore == 0 {
		return true // caller did not state an expectation
	}
	return h.RequestedThreadsPerCore == h.ThreadsPerCore
}

// WorkloadSpec is what each guest was asked to do. Recorded so a result can be reproduced
// and so latencies from different workload shapes are never compared directly.
type WorkloadSpec struct {
	// Iterations is the compute-loop count sent to each guest.
	Iterations uint64 `json:"iterations"`
	// Rounds is the minimum number of times each guest runs the workload. It is a floor
	// rather than the actual count, because TargetSamples can raise it at low density.
	Rounds int `json:"rounds"`
	// WorkingSetKiB selects the cache-sensitive pointer-chase workload instead of the
	// arithmetic compute loop, with a buffer of this size per microVM. Zero means use the
	// compute loop.
	//
	// The compute loop cannot evaluate cache-affinity placement: its working set is about
	// sixteen bytes, so it never reaches last-level cache. Measured on m8i, taking a host from
	// 10 to 90 microVMs sharing one L3 moved the mean 0.4%, so a cache-affinity config
	// measured against it would report no effect regardless of whether the placement worked.
	WorkingSetKiB int `json:"working_set_kib,omitempty"`

	// Sandbox selects the agentic-sandbox-shaped workload: fork/exec churn per step, a resident
	// buffer held for the whole episode, and file writes to a tmpfs. Non-nil overrides both the
	// compute loop and the pointer chase.
	//
	// Neither of the other workloads forks a process, holds memory, or writes a byte. A real
	// RLVR rollout does all three repeatedly, and those are different kernel paths that take
	// kernel locks and touch shared structures, so they are the paths most likely to contend at
	// density. This workload also carries the resident-set size, which is what determines
	// whether memory or cores is the binding density constraint.
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`

	// Task selects a real task baked into the guest image: the guest runs it and reports its
	// exit status. Non-nil overrides every other workload.
	//
	// This is the only workload whose result is not a latency number. A SWE-bench instance
	// carries fail-to-pass tests, so the runner's exit status is a machine-checkable reward and
	// the measurement is "how many real tasks per host, and did they pass" rather than "how fast
	// is a synthetic loop". That is the figure a customer can hold against their own harness.
	Task *TaskSpec `json:"task,omitempty"`

	// Episode replays a recorded agent trajectory as a full RL episode. Non-nil overrides every
	// other workload, including Task.
	//
	// This is the workload a customer is actually sizing for. Task runs a verifier once, which is
	// one component of an episode: a real GRPO rollout is tens of turns of short process spawns and
	// file reads, and only then does the verifier decide the reward. The two have different shapes,
	// so a fleet sized from verifier runs understates an episode by however many turns it has.
	Episode *EpisodeSpec `json:"episode,omitempty"`

	// TargetSamples is the sample count each cell aims for, achieved by scaling rounds
	// inversely with density.
	//
	// A fixed round count makes low-density cells structurally sample-starved: samples are
	// density x rounds, so at 5 rounds a 10-microVM cell yields 50 samples and a 180-microVM
	// cell yields 900. At 50 samples the nearest-rank p99 is rank ceil(50 x 0.99) = 50, which
	// is the maximum, so the reported p99 is a single worst observation rather than a tail
	// estimate. Low-density cells are also the cheapest to run, so raising their round count
	// costs very little wall clock. Zero disables scaling and uses Rounds everywhere.
	//
	// The default of 450 was tuned for the 7.7 ms compute loop, where 45 rounds at density 10
	// costs seconds. **It does not transfer to slow workloads.** A 22-second sandbox episode at
	// 45 rounds is 16 minutes for a single low-density cell, and low-density cells are the least
	// interesting ones. For episode-scale workloads use 100, which puts the nearest-rank p99 at
	// rank 99 of 100 -- the smallest count at which p99 is not simply the maximum. Going below
	// 100 reintroduces the p99 == max defect documented in analysis/PINNING-RESULTS.md.
	TargetSamples int `json:"target_samples,omitempty"`
	// VCPUCount and MemSizeMiB are the per-microVM machine configuration.
	//
	// MemSizeMiB must be large enough to hold the sandbox workload's resident set plus its
	// tmpfs writes plus the guest kernel. Validate checks that rather than leaving an
	// undersized guest to be OOM-killed, which presents to the host as a microVM that
	// silently stopped answering.
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

// SandboxSpec parameterises the agentic-sandbox workload.
//
// The three fields map onto the parts of an episode that cost host resources: memory held,
// processes created, and bytes written. They are separate rather than derived from one another
// because a customer's sandbox can be large-and-quiet (a loaded repo, few tool calls) or
// small-and-busy (a tight edit-test loop), and those stress different limits.
type SandboxSpec struct {
	// ResidentKiB is memory held for the whole episode, faulted in and re-touched each step.
	// Stands in for an interpreter heap plus a checked-out repository.
	//
	// This is the parameter that answers the memory-versus-cores question. Every measurement
	// before this one used 128 MiB, which no real sandbox uses: a SWE-bench-style environment
	// needs 1 to 4 GiB, and at 2 GiB a 768 GiB host caps out near 380 microVMs regardless of
	// how many cores it has.
	ResidentKiB int `json:"resident_kib"`
	// Steps is tool calls per episode. Each spawns and reaps a child process.
	Steps int `json:"steps"`
	// WriteKiB is bytes written per step to a tmpfs, modelling file edits and test output.
	//
	// Note that tmpfs pages are *guest* memory, so writes here consume the same budget as
	// ResidentKiB. That is a real property of a read-only-rootfs sandbox rather than an
	// artifact of the harness, and it is why the two are separate parameters.
	WriteKiB int `json:"write_kib"`
}

// Guest memory overhead, measured on m8i and m8a metal 2026-08-02.
//
// A flat 64 MiB was tried first and was wrong: a 2 GiB resident set in a 2176 MiB guest passed
// validation and then died with "read reply: EOF" on every microVM, because the guest kernel
// OOM-killed the agent. The host had 768 GiB free, so the kill was inside the guest.
//
// Overhead is not constant. The guest kernel's page tables scale with the memory it maps
// (roughly 0.2% for 4 KiB pages), the tmpfs needs room for a step's writes, and the kernel
// image, slab, and agent are fixed. So the floor is a fixed part plus a proportional part.
const (
	// guestBaseOverheadMiB covers the guest kernel image, slab, the static agent, and enough
	// slack that a page-cache spike does not tip the guest over.
	guestBaseOverheadMiB = 192
	// guestOverheadPercent covers page tables and per-page kernel bookkeeping, which grow with
	// the size of the resident set rather than staying fixed.
	guestOverheadPercent = 25
)

// TaskSpec identifies the real task a guest image runs.
//
// The command itself lives in the image (/etc/fc-task.env, written at conversion), so this
// carries only what the host needs for provenance and for judging the result.
type TaskSpec struct {
	// InstanceID is the upstream identifier, for example "django__django-11099". Recorded per
	// sample so a dataset states which task produced it; two instances from the same repository
	// can differ by orders of magnitude in runtime.
	InstanceID string `json:"instance_id"`
	// ExpectedExit is the exit code that means the task succeeded, normally 0 for a
	// fail-to-pass test run.
	//
	// Compared against what the guest reports so a cell where tasks failed is not quoted as a
	// latency result. **A task that crashes early looks fast**, which is the same trap as the
	// undersized guest whose OOM read as a density ceiling.
	ExpectedExit int `json:"expected_exit"`
	// TimeoutSec is how long the task's own benchmark allows it, from the upstream task
	// definition -- Terminal-Bench's task.yaml declares max_test_timeout_sec, typically 180.
	//
	// Recorded so the host's per-request timeout can be checked against it before a run. A
	// request cut off below the task's own limit reports a *failed* task rather than a slow one,
	// so the cell's pass rate would be wrong for a reason that has nothing to do with the task.
	// Zero means unstated, and no check is made.
	TimeoutSec float64 `json:"timeout_sec,omitempty"`
}

// EpisodeSpec identifies the recorded trajectory a guest image replays.
//
// The turns and the verifier command live in the image, baked in by traj-to-episode.py, so this
// carries only what the host needs for provenance and for judging the result.
type EpisodeSpec struct {
	// InstanceID is the upstream instance the trajectory solved, for example
	// "marshmallow-code__marshmallow-1867".
	InstanceID string `json:"instance_id"`
	// TrajectorySource records where the recording came from, since a replay is only interpretable
	// against the run it replays. Different agents produce very different episode lengths.
	TrajectorySource string `json:"trajectory_source,omitempty"`
	// Turns is how many turns the episode should run, from the trajectory.
	//
	// Checked against what the guest reports. An episode whose turns stopped executing partway is
	// **fast**, and without this it would look like a cheap episode rather than a broken one. Same
	// class of trap as the exit-127 task that read as a 2 ms success.
	Turns int `json:"turns"`
	// ExpectedTurnsOK is how many of those turns exited zero in the recording.
	//
	// Below Turns is normal: a real rollout contains mistakes, and the trajectory replayed here has
	// an edit rejected for bad indentation before the retry succeeds. Recorded so a *change* in the
	// failure count is detectable, which would mean the replay diverged from the recording.
	ExpectedTurnsOK int `json:"expected_turns_ok"`
	// ExpectedExit is the verifier's exit code that means the episode earned its reward.
	ExpectedExit int `json:"expected_exit"`
	// TimeoutSec is how long one episode may take. An episode is far longer than a single verifier
	// run -- tens of turns plus the verifier -- so the host's per-request timeout matters more here
	// than for Task, and a request cut off early is recorded as a failed episode rather than a slow
	// one. Zero means unstated.
	TimeoutSec float64 `json:"timeout_sec,omitempty"`
}

// GuestMemoryFloorMiB is the smallest guest that can host this sandbox spec.
//
// A guest sized below this gets OOM-killed mid-episode, and the host sees a microVM that
// stopped answering rather than an error. At density that reads as a density ceiling, so an
// undersized guest would be published as a memory limit that is really a configuration bug.
// Refusing up front is much cheaper than the wrong conclusion.
//
// Deliberately generous. Over-provisioning a guest costs host memory, which is measurable and
// recoverable; under-provisioning costs a paid session and risks a wrong number.
func (s SandboxSpec) GuestMemoryFloorMiB() int {
	payloadMiB := (s.ResidentKiB + s.WriteKiB) / 1024
	return payloadMiB + payloadMiB*guestOverheadPercent/100 + guestBaseOverheadMiB
}

// MaxRoundsPerCell bounds the rounds TargetSamples can request.
//
// It exists only to stop a very low density combined with a very high target from producing a
// session far longer than intended. It is generous enough never to bind in the lab plan, and
// when it does bind the cell records a note rather than quietly running fewer rounds.
const MaxRoundsPerCell = 500

// RoundsFor returns how many workload rounds a cell at this density should run.
//
// Rounds is treated as a floor, so enabling TargetSamples can only add samples to a cell and
// never remove them. That keeps high-density cells, which already have plenty of samples,
// exactly as they were.
func (w WorkloadSpec) RoundsFor(density int) int {
	rounds := w.Rounds
	if rounds < 1 {
		rounds = 1
	}
	if w.TargetSamples <= 0 || density <= 0 {
		return rounds
	}
	// Ceiling division: land at or above the target rather than just below it.
	needed := (w.TargetSamples + density - 1) / density
	if needed > rounds {
		rounds = needed
	}
	if rounds > MaxRoundsPerCell {
		rounds = MaxRoundsPerCell
	}
	return rounds
}

// Sample is one workload measurement from one microVM.
//
// These are retained individually rather than only in aggregate. Percentiles cannot be
// recomputed from percentiles, so discarding the raw samples would make it impossible to
// ask a different question of the data later: whether a large jitter figure came from one
// outlier or a wide distribution, whether specific microVMs were consistently slow (which
// would point at particular cores or NUMA nodes), or what p95 and p999 were. At a few tens
// of thousands of rows the storage cost is trivial against losing measurements that require
// paid hardware to reproduce.
type Sample struct {
	Config  placement.Config `json:"config"`
	Density int              `json:"density"`
	VMIndex int              `json:"vm_index"`
	Node    int              `json:"numa_node"`
	Round   int              `json:"round"`
	// DurationUS is the guest-reported wall clock for the workload, in microseconds.
	DurationUS float64 `json:"duration_us"`
	// GuestMeanNS is the guest's own per-iteration mean, which is a cross-check on
	// DurationUS rather than an independent measurement.
	GuestMeanNS uint64 `json:"guest_mean_ns"`
	// Checksum proves the compute loop actually ran. A guest whose loop was optimised
	// away would otherwise look like an implausibly fast host.
	Checksum uint64 `json:"checksum"`
	// CPUSet is the cpuset the microVM was pinned to, so a slow sample can be traced back
	// to specific CPUs.
	CPUSet string `json:"cpuset"`
	// WorkingSetKiB is the pointer-chase buffer size, zero for the compute loop. Recorded per
	// sample so a working-set sweep is reanalysable from the raw file alone.
	WorkingSetKiB uint64 `json:"working_set_kib,omitempty"`
	// SetupUS is guest-side buffer allocation, excluded from DurationUS. Retained so the
	// exclusion is verifiable: if setup were leaking into the timed region, these two would
	// move together across working-set sizes.
	SetupUS float64 `json:"setup_us,omitempty"`
	// SpawnUS and WriteUS are the fork/exec and file-write components of DurationUS for the
	// sandbox workload. Retained per sample so a density effect can be attributed to process
	// creation or to writeback rather than only observed in the total, which is the whole
	// reason for running a workload that exercises those paths.
	SpawnUS float64 `json:"spawn_us,omitempty"`
	WriteUS float64 `json:"write_us,omitempty"`
	// SpawnsOK is children successfully reaped. Below the requested step count means fork
	// failed, which at density usually means a PID or memory limit was reached. That is a
	// density finding and must not be silently averaged away.
	SpawnsOK uint64 `json:"spawns_ok,omitempty"`
	// ExitCode is the real task's exit status, which for a SWE-bench instance is the
	// fail-to-pass reward. Retained per sample so pass rate can be recomputed per microVM and a
	// systematically failing slot traced to its cpuset.
	ExitCode int `json:"exit_code,omitempty"`
	// PeakRSSKiB is the guest's measured high-water resident set, which is the per-instance
	// memory figure warm-pool sizing needs and cannot be derived from the configured guest size.
	PeakRSSKiB uint64 `json:"peak_rss_kib,omitempty"`
	// InstanceID names the task, since two instances from one repository can differ by orders of
	// magnitude in runtime and must never be pooled into one latency figure.
	InstanceID string `json:"instance_id,omitempty"`
	// Turns and TurnsOK are the episode's turn count and how many exited zero. TurnsOK below Turns
	// is normal -- a recorded rollout contains mistakes -- but a Turns below what the trajectory
	// says means the episode stopped executing partway, which is fast rather than cheap.
	Turns   int `json:"turns,omitempty"`
	TurnsOK int `json:"turns_ok,omitempty"`
	// TurnsUS and VerifyUS split DurationUS into the agent's turns and the verifier run.
	//
	// Retained per sample because the ratio is the finding: it is what distinguishes an episode
	// from the single verifier run that every earlier measurement here reported as one. Recomputing
	// it from aggregates afterwards is not possible.
	TurnsUS  float64 `json:"turns_us,omitempty"`
	VerifyUS float64 `json:"verify_us,omitempty"`
	// Repeat is the zero-based repeat index this sample belongs to. Without it, samples from
	// separate repeats of the same cell are indistinguishable in the raw file, which would
	// make across-repeat variance uncomputable from the very data retained to compute it.
	Repeat int `json:"repeat"`
}

// CellResult is one configuration at one density level, from one repeat.
type CellResult struct {
	Config  placement.Config `json:"config"`
	Density int              `json:"density_requested"`

	// Repeat is the zero-based repeat index. Repeats of the same cell are kept as separate
	// results rather than pooled, because pooling would hide the run-to-run spread that is
	// the reason for repeating at all.
	Repeat int `json:"repeat"`

	// RoundsRun is how many workload rounds each guest actually ran. It can exceed the
	// configured Rounds when TargetSamples raises it at low density, so the effective value
	// travels with the cell rather than only the requested one.
	RoundsRun int `json:"rounds_run"`

	// Launched is how many microVMs reached a state where they answered the agent. This
	// can be lower than Density, and the gap is itself a finding.
	Launched int `json:"launched"`
	// Responded is how many returned at least one workload result. A microVM that launched
	// but never answered is a different failure from one that never launched.
	Responded int `json:"responded"`

	// TaskRuns and TaskPassed count real-task rounds and how many exited as expected. Both zero
	// for a synthetic workload, which is what PassRate and Trustworthy key off.
	TaskRuns   int `json:"task_runs,omitempty"`
	TaskPassed int `json:"task_passed,omitempty"`

	// EpisodeTurnShortfalls counts episode runs that executed a different number of turns than the
	// trajectory has. Non-zero means the replay diverged: an episode that stopped partway is fast,
	// so without this it would be quoted as a cheap episode rather than a broken one.
	EpisodeTurnShortfalls int `json:"episode_turn_shortfalls,omitempty"`

	// PlanNotes carries any note from the placement plan, for example that the requested
	// density exceeded the configuration's ceiling.
	PlanNotes []string `json:"plan_notes,omitempty"`

	// Latency statistics across all workload rounds from all guests, in microseconds.
	// Microseconds rather than Duration so the JSON is directly usable in a spreadsheet.
	MeanLatencyUS float64 `json:"mean_latency_us"`
	P50LatencyUS  float64 `json:"p50_latency_us"`
	P99LatencyUS  float64 `json:"p99_latency_us"`
	MaxLatencyUS  float64 `json:"max_latency_us"`
	JitterUS      float64 `json:"jitter_us"`
	SampleCount   int     `json:"sample_count"`

	// LaunchLatency is time from process spawn to the agent answering, which is the
	// figure a warm pool refill rate depends on.
	MeanLaunchMS float64 `json:"mean_launch_ms"`
	P99LaunchMS  float64 `json:"p99_launch_ms"`

	// Memory is recorded twice, because boot cost and working-set cost answer different
	// questions and only the second is what a fleet is sized on.
	//
	// The *Boot fields are measured after the settle, with every microVM up and idle. Until
	// 2026-09-18 these were the only memory figures recorded and they carried the unqualified
	// names, so every dataset before then reports boot cost while reading as though it were the
	// whole story. For a workload whose footprint the workload itself creates -- a sandbox's
	// resident set, a test suite's heap -- they describe an idle guest.
	//
	// The *Working fields are measured after the workload finishes, before teardown. That is the
	// cell's high-water mark rather than merely its final state: no balloon device is configured
	// and nothing calls MADV_DONTNEED, so a page the guest faults stays resident in the VMM's
	// anonymous mapping and these figures only grow over a cell's life. Measured once at the end
	// rather than sampled during, because reading smaps_rollup for 240 processes on a timer would
	// perturb the very latency region the sweep exists to measure.
	FleetRSSKiBBoot          int     `json:"fleet_rss_kib_boot"`
	FleetPSSKiBBoot          int     `json:"fleet_pss_kib_boot"`
	PerVMPrivateDirtyKiBBoot int     `json:"per_vm_private_dirty_kib_boot"`
	SharingRatioBoot         float64 `json:"sharing_ratio_boot"`

	FleetRSSKiBWorking          int     `json:"fleet_rss_kib_working"`
	FleetPSSKiBWorking          int     `json:"fleet_pss_kib_working"`
	PerVMPrivateDirtyKiBWorking int     `json:"per_vm_private_dirty_kib_working"`
	SharingRatioWorking         float64 `json:"sharing_ratio_working"`

	HostCachedKiB int `json:"host_cached_kib"`

	// WorkingSetRegression is set when the working-set PSS came in below the boot PSS. See
	// CheckWorkingSetMonotonic.
	WorkingSetRegression bool `json:"working_set_regression,omitempty"`

	// Host counters observed across the measurement window.
	ContextSwitchesPerSec float64 `json:"context_switches_per_sec"`

	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration_ns"`

	// Errors records anything that went wrong without aborting the cell. A cell with
	// errors is still reportable, but the errors must travel with the numbers.
	Errors []string `json:"errors,omitempty"`
}

// ShortfallPercent reports how far the cell fell below its requested density. A non-zero
// value means the latency figures describe a smaller fleet than asked for.
func (c *CellResult) ShortfallPercent() float64 {
	if c.Density == 0 {
		return 0
	}
	return 100 * float64(c.Density-c.Responded) / float64(c.Density)
}

// PassRate is the fraction of task runs that exited as expected.
//
// Returns 1.0 when no task results were recorded, so a synthetic-workload cell is not penalised
// for a measurement it never made.
func (c *CellResult) PassRate() float64 {
	if c.TaskRuns == 0 {
		return 1.0
	}
	return float64(c.TaskPassed) / float64(c.TaskRuns)
}

// Trustworthy reports whether a cell's numbers can be quoted without qualification.
//
// The 5% threshold is a judgement, not a law: a couple of microVMs missing out of 90 does not
// change the picture, but losing a tenth of the fleet means the remaining ones saw less
// contention than the configuration nominally creates.
//
// For a real-task workload the pass rate matters as much as the shortfall, and for the same
// reason: **a task that crashes early looks fast**. A cell where the test suite failed to run
// would otherwise report an attractively low latency and be quoted as a density result. The
// threshold is 100% rather than a fraction because a fail-to-pass test either passes or the
// instance did not work in this environment, and either way the latency describes something other
// than the task.
func (c *CellResult) Trustworthy() bool {
	if len(c.Errors) != 0 || c.ShortfallPercent() > 5.0 || c.SampleCount == 0 {
		return false
	}
	// Checked explicitly rather than relying on the error it also records: Errors is capped at five
	// entries per cell, so a cell that hit the cap on something else could still carry a divergent
	// replay with nothing to show for it.
	if c.EpisodeTurnShortfalls != 0 {
		return false
	}
	return c.TaskRuns == 0 || c.TaskPassed == c.TaskRuns
}

// Results is a complete sweep.
type Results struct {
	Host     HostInfo     `json:"host"`
	Workload WorkloadSpec `json:"workload"`
	Cells    []CellResult `json:"cells"`

	// Repeats is how many times the matrix was run. One repeat gives no variance
	// information at all, so a single-repeat dataset can only be reported as indicative.
	Repeats int `json:"repeats"`

	// ShuffleSeed is the seed used to randomise cell order, recorded so an ordering can be
	// replayed if a result looks order-dependent. Zero means cells ran in matrix order.
	ShuffleSeed int64 `json:"shuffle_seed,omitempty"`

	// Samples holds every individual measurement. Written to a separate file rather than
	// inlined in the JSON summary, since it is orders of magnitude larger.
	Samples []Sample `json:"-"`

	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration_ns"`

	// SkippedConfigs records configurations that were not run and why, for example an
	// SMT-dependent configuration on a host with one thread per core. An absent
	// configuration would otherwise look like an oversight.
	SkippedConfigs map[string]string `json:"skipped_configs,omitempty"`
}

// Add appends a cell result.
func (r *Results) Add(c CellResult) { r.Cells = append(r.Cells, c) }

// Skip records a configuration that was not run, with the reason.
func (r *Results) Skip(cfg placement.Config, reason string) {
	if r.SkippedConfigs == nil {
		r.SkippedConfigs = map[string]string{}
	}
	r.SkippedConfigs[string(cfg)] = reason
}

// AddSamples appends raw measurements.
func (r *Results) AddSamples(s []Sample) { r.Samples = append(r.Samples, s...) }

// WriteSamplesCSV emits one row per measurement.
//
// This is the file that makes the dataset reanalysable: percentiles, distributions, and
// per-microVM outlier analysis can all be derived from it, and none of them can be derived
// from the aggregate summary.
func (r *Results) WriteSamplesCSV(w io.Writer) error {
	if _, err := io.WriteString(w,
		"config,density,repeat,vm_index,numa_node,round,duration_us,guest_mean_ns,checksum,"+
			"working_set_kib,setup_us,spawn_us,write_us,spawns_ok,"+
			"exit_code,peak_rss_kib,turns,turns_ok,turns_us,verify_us,instance_id,cpuset\n"); err != nil {
		return err
	}
	// cpuset stays last: it is the only free-text field, so keeping it at the end means adding
	// a column never shifts it and existing parsers that split on comma keep working.
	for _, s := range r.Samples {
		if _, err := fmt.Fprintf(w,
			"%s,%d,%d,%d,%d,%d,%.1f,%d,%d,%d,%.1f,%.1f,%.1f,%d,%d,%d,%d,%d,%.1f,%.1f,%s,%s\n",
			s.Config, s.Density, s.Repeat, s.VMIndex, s.Node, s.Round,
			s.DurationUS, s.GuestMeanNS, s.Checksum,
			s.WorkingSetKiB, s.SetupUS,
			s.SpawnUS, s.WriteUS, s.SpawnsOK,
			s.ExitCode, s.PeakRSSKiB,
			s.Turns, s.TurnsOK, s.TurnsUS, s.VerifyUS,
			s.InstanceID, s.CPUSet); err != nil {
			return err
		}
	}
	return nil
}

// WriteJSON emits the full results.
func (r *Results) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteCSV emits one row per cell, for direct use in a spreadsheet or plot.
//
// A trustworthy column is included rather than filtering untrustworthy rows out. Dropping
// them would hide the density ceiling, which is one of the results worth reporting.
func (r *Results) WriteCSV(w io.Writer) error {
	header := "config,density_requested,repeat,rounds_run,launched,responded,shortfall_pct,trustworthy," +
		"task_runs,task_passed,pass_rate," +
		"mean_us,p50_us,p99_us,max_us,jitter_us,samples," +
		"mean_launch_ms,p99_launch_ms," +
		"fleet_rss_kib_boot,fleet_pss_kib_boot,per_vm_private_dirty_kib_boot,sharing_ratio_boot," +
		"ctx_switches_per_sec,errors," +
		// Appended rather than placed beside their _boot counterparts so the leading column
		// order matches every dataset committed before 2026-09-18. Anything reading these
		// positionally keeps working; anything reading by name gets both.
		"fleet_rss_kib_working,fleet_pss_kib_working," +
		"per_vm_private_dirty_kib_working,sharing_ratio_working\n"
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	for _, c := range r.Cells {
		_, err := fmt.Fprintf(w,
			"%s,%d,%d,%d,%d,%d,%.1f,%t,%d,%d,%.3f,%.1f,%.1f,%.1f,%.1f,%.1f,%d,%.1f,%.1f,%d,%d,%d,%.2f,%.0f,%d,"+
				"%d,%d,%d,%.2f\n",
			c.Config, c.Density, c.Repeat, c.RoundsRun,
			c.Launched, c.Responded, c.ShortfallPercent(), c.Trustworthy(),
			c.TaskRuns, c.TaskPassed, c.PassRate(),
			c.MeanLatencyUS, c.P50LatencyUS, c.P99LatencyUS, c.MaxLatencyUS, c.JitterUS, c.SampleCount,
			c.MeanLaunchMS, c.P99LaunchMS,
			c.FleetRSSKiBBoot, c.FleetPSSKiBBoot, c.PerVMPrivateDirtyKiBBoot, c.SharingRatioBoot,
			c.ContextSwitchesPerSec, len(c.Errors),
			c.FleetRSSKiBWorking, c.FleetPSSKiBWorking, c.PerVMPrivateDirtyKiBWorking, c.SharingRatioWorking)
		if err != nil {
			return err
		}
	}
	return nil
}

// CellVariance summarises one config-and-density across its repeats.
//
// This is the statistic a repeated sweep exists to produce. A single cell reports a p99; a set
// of repeats reports how much that p99 moves when nothing changes, which is what determines
// whether a difference between two configurations is a finding or noise.
type CellVariance struct {
	Config  placement.Config `json:"config"`
	Density int              `json:"density"`
	Repeats int              `json:"repeats"`

	MeanOfMeansUS float64 `json:"mean_of_means_us"`
	MinMeanUS     float64 `json:"min_mean_us"`
	MaxMeanUS     float64 `json:"max_mean_us"`

	// Three percentiles rather than one, because they answer different questions and a
	// recommendation usually needs at least two of them:
	//
	//   p50   the typical task, which is what a customer's own dashboard shows
	//   p99   the tail
	//   p100  the worst task
	//
	// p100 is the one most easily dismissed and often the most decision-relevant. Under a GRPO
	// group barrier no completion contributes to the gradient until all G in its group finish,
	// and max-of-G converges toward p100 as G grows, so at large G it is p100 rather than p99
	// that gates a training step.
	//
	// Carrying all three here rather than only in CellResult matters because this is the file
	// every published comparison reads from. Only mean and p99 used to reach it, so the
	// tail-versus-median distinction had to be recomputed from raw samples every time.
	//
	// Each is the median of the per-repeat values rather than the mean, because a percentile is
	// already a tail statistic and one pathological repeat should not drag the summary.
	MedianP50US float64 `json:"median_p50_us"`
	MinP50US    float64 `json:"min_p50_us"`
	MaxP50US    float64 `json:"max_p50_us"`

	MedianP99US float64 `json:"median_p99_us"`
	MinP99US    float64 `json:"min_p99_us"`
	MaxP99US    float64 `json:"max_p99_us"`

	MedianP100US float64 `json:"median_p100_us"`
	MinP100US    float64 `json:"min_p100_us"`
	MaxP100US    float64 `json:"max_p100_us"`

	MinJitterUS float64 `json:"min_jitter_us"`
	MaxJitterUS float64 `json:"max_jitter_us"`

	// AllTrustworthy is false when any repeat had errors or fell short of its density, in
	// which case the spread mixes measurement noise with partial failure.
	AllTrustworthy bool `json:"all_trustworthy"`
}

// SpreadPercent reports the p99 range as a percentage of the median p99. It is the headline
// number for "how much does this move between identical runs".
func (v CellVariance) SpreadPercent() float64 {
	if v.MedianP99US == 0 {
		return 0
	}
	return 100 * (v.MaxP99US - v.MinP99US) / v.MedianP99US
}

// TailRatio is median p99 over median p50.
//
// This bounds max-of-G group-completion amplification: a group's completion time cannot exceed
// the tail it samples from, so a cell whose p99 sits close to its p50 cannot produce a straggler
// effect however large G is. It has been computed by hand in every analysis so far
// (analysis/GRPO-GROUP-COMPLETION.md, analysis/SANDBOX-WORKLOAD.md); exposing it here means the
// oversubscription signature can be read straight off the variance file.
func (v CellVariance) TailRatio() float64 {
	if v.MedianP50US == 0 {
		return 0
	}
	return v.MedianP99US / v.MedianP50US
}

// WorstRatio is median p100 over median p50: how much slower the worst task is than the typical
// one. Diverging from TailRatio as density rises is what says the extreme is pulling away from
// the tail, which is the regime where large-G groups suffer most.
func (v CellVariance) WorstRatio() float64 {
	if v.MedianP50US == 0 {
		return 0
	}
	return v.MedianP100US / v.MedianP50US
}

// Variance groups cells by config and density and summarises across repeats.
//
// Cells with only one repeat are included with a zero spread rather than omitted, so a reader
// can see which cells have no variance information instead of having to infer it from absence.
func (r *Results) Variance() []CellVariance {
	type key struct {
		cfg     placement.Config
		density int
	}
	grouped := map[key][]*CellResult{}
	var order []key
	for i := range r.Cells {
		c := &r.Cells[i]
		k := key{c.Config, c.Density}
		if _, seen := grouped[k]; !seen {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], c)
	}

	out := make([]CellVariance, 0, len(order))
	for _, k := range order {
		cells := grouped[k]
		v := CellVariance{
			Config:         k.cfg,
			Density:        k.density,
			Repeats:        len(cells),
			AllTrustworthy: true,
		}
		means := make([]float64, 0, len(cells))
		p50s := make([]float64, 0, len(cells))
		p99s := make([]float64, 0, len(cells))
		p100s := make([]float64, 0, len(cells))
		jitters := make([]float64, 0, len(cells))
		for _, c := range cells {
			if !c.Trustworthy() {
				v.AllTrustworthy = false
			}
			if c.SampleCount == 0 {
				continue // nothing measured; including a zero would understate every statistic
			}
			means = append(means, c.MeanLatencyUS)
			p50s = append(p50s, c.P50LatencyUS)
			p99s = append(p99s, c.P99LatencyUS)
			// MaxLatencyUS is p100: measure.LatencySamples.Max() is Percentile(100).
			p100s = append(p100s, c.MaxLatencyUS)
			jitters = append(jitters, c.JitterUS)
		}
		if len(means) == 0 {
			out = append(out, v)
			continue
		}
		sort.Float64s(means)
		sort.Float64s(p50s)
		sort.Float64s(p99s)
		sort.Float64s(p100s)
		sort.Float64s(jitters)

		var total float64
		for _, m := range means {
			total += m
		}
		v.MeanOfMeansUS = total / float64(len(means))
		v.MinMeanUS, v.MaxMeanUS = means[0], means[len(means)-1]
		v.MedianP50US = median(p50s)
		v.MinP50US, v.MaxP50US = p50s[0], p50s[len(p50s)-1]
		v.MedianP99US = median(p99s)
		v.MinP99US, v.MaxP99US = p99s[0], p99s[len(p99s)-1]
		v.MedianP100US = median(p100s)
		v.MinP100US, v.MaxP100US = p100s[0], p100s[len(p100s)-1]
		v.MinJitterUS, v.MaxJitterUS = jitters[0], jitters[len(jitters)-1]
		out = append(out, v)
	}
	return out
}

// median expects a sorted slice. For an even count it averages the two middle values, which is
// the conventional definition and matters here because repeat counts are often even.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// WriteVarianceCSV emits one row per config-and-density with across-repeat spread.
//
// Carries p50, p99 and p100 with their across-repeat ranges, plus the two derived ratios, so a
// reader can see the typical task, the tail, and the worst task without going back to the raw
// samples. Column order groups each percentile's median/min/max together rather than interleaving,
// so a spreadsheet selection of three adjacent columns is one statistic.
func (r *Results) WriteVarianceCSV(w io.Writer) error {
	if _, err := io.WriteString(w,
		"config,density,repeats,mean_of_means_us,min_mean_us,max_mean_us,"+
			"median_p50_us,min_p50_us,max_p50_us,"+
			"median_p99_us,min_p99_us,max_p99_us,"+
			"median_p100_us,min_p100_us,max_p100_us,"+
			"p99_spread_pct,tail_ratio,worst_ratio,"+
			"min_jitter_us,max_jitter_us,all_trustworthy\n"); err != nil {
		return err
	}
	for _, v := range r.Variance() {
		if _, err := fmt.Fprintf(w,
			"%s,%d,%d,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.3f,%.3f,%.1f,%.1f,%t\n",
			v.Config, v.Density, v.Repeats,
			v.MeanOfMeansUS, v.MinMeanUS, v.MaxMeanUS,
			v.MedianP50US, v.MinP50US, v.MaxP50US,
			v.MedianP99US, v.MinP99US, v.MaxP99US,
			v.MedianP100US, v.MinP100US, v.MaxP100US,
			v.SpreadPercent(), v.TailRatio(), v.WorstRatio(),
			v.MinJitterUS, v.MaxJitterUS, v.AllTrustworthy); err != nil {
			return err
		}
	}
	return nil
}

// Summarize renders a human-readable report.
//
// The report leads with caveats rather than appending them. Someone skimming for a density
// number should encounter the reasons not to trust it before the number itself.
func (r *Results) Summarize(w io.Writer) error {
	p := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}

	if err := p("Density sweep: %s\n", r.Host.InstanceType); err != nil {
		return err
	}
	if err := p("  %s, %d physical cores x %d threads, %d NUMA nodes, SMT %s\n",
		r.Host.CPUModel, r.Host.PhysicalCores, r.Host.ThreadsPerCore,
		r.Host.NUMANodes, r.Host.SMTState); err != nil {
		return err
	}
	if err := p("  cache: %s (%d L3 domain(s) across %d nodes)\n",
		r.Host.CacheMode, r.Host.L3Domains, r.Host.NUMANodes); err != nil {
		return err
	}
	if r.Host.RunLabel != "" || r.Host.RequestedThreadsPerCore != 0 {
		label := r.Host.RunLabel
		if label == "" {
			label = "(unlabelled)"
		}
		if err := p("  run: %s, CpuOptions ThreadsPerCore requested %d, observed %d\n",
			label, r.Host.RequestedThreadsPerCore, r.Host.ThreadsPerCore); err != nil {
			return err
		}
	}
	if !r.Host.SMTConsistent() {
		// Leading, not buried: the run measured a different SMT configuration than intended,
		// so every number below describes something other than what was asked for.
		if err := p("\n  *** SMT MISMATCH: requested ThreadsPerCore=%d but the host reports %d.\n"+
			"      The CpuOptions setting did not take effect. These results do not describe\n"+
			"      the intended configuration and should not be used for the SMT comparison.\n",
			r.Host.RequestedThreadsPerCore, r.Host.ThreadsPerCore); err != nil {
			return err
		}
	}
	if err := p("  firecracker %s, guest kernel %s\n",
		r.Host.FirecrackerVer, r.Host.GuestKernel); err != nil {
		return err
	}
	roundsDesc := fmt.Sprintf("%d rounds", r.Workload.Rounds)
	if r.Workload.TargetSamples > 0 {
		roundsDesc = fmt.Sprintf("%d+ rounds scaled to ~%d samples per cell",
			r.Workload.Rounds, r.Workload.TargetSamples)
	}
	// Same precedence as Kind(), and a switch rather than a chain of ifs so a later branch
	// cannot silently overwrite an earlier one. An if-chain here did exactly that: Sandbox
	// clobbered Task and the summary named the wrong workload.
	var kind string
	switch {
	case r.Workload.Episode != nil:
		kind = fmt.Sprintf("replayed episode %s (%d turns, %d expected OK, expect exit %d)",
			r.Workload.Episode.InstanceID, r.Workload.Episode.Turns,
			r.Workload.Episode.ExpectedTurnsOK, r.Workload.Episode.ExpectedExit)
	case r.Workload.Task != nil:
		kind = fmt.Sprintf("real task %s (expect exit %d)",
			r.Workload.Task.InstanceID, r.Workload.Task.ExpectedExit)
	case r.Workload.Sandbox != nil:
		sb := r.Workload.Sandbox
		kind = fmt.Sprintf("agentic sandbox: %d KiB resident, %d steps, %d KiB written per step",
			sb.ResidentKiB, sb.Steps, sb.WriteKiB)
	case r.Workload.WorkingSetKiB > 0:
		kind = fmt.Sprintf("pointer chase over %d KiB", r.Workload.WorkingSetKiB)
	default:
		kind = "arithmetic compute loop"
	}
	if err := p("  workload: %s, %d iterations x %s, %d vCPU / %d MiB per microVM\n",
		kind, r.Workload.Iterations, roundsDesc,
		r.Workload.VCPUCount, r.Workload.MemSizeMiB); err != nil {
		return err
	}
	if r.Workload.WorkingSetKiB == 0 && r.Workload.Sandbox == nil && r.Workload.Task == nil &&
		r.Workload.Episode == nil {
		// Stated because it bounds what the dataset can answer. The compute loop's working set
		// never reaches last-level cache, so cache-related placement differences are invisible
		// to it and an absence of effect here is not evidence of an absence of effect.
		if err := p("  note: this workload is cache-insensitive; it cannot evaluate cache-affinity placement\n"); err != nil {
			return err
		}
	}
	repeats := r.Repeats
	if repeats < 1 {
		repeats = 1
	}
	if err := p("  repeats: %d\n\n", repeats); err != nil {
		return err
	}

	// Caveats first.
	if repeats == 1 {
		// Leading, because a single repeat means every number below is one observation and
		// none of the differences between configurations can be separated from run-to-run
		// noise. That is the most important thing to know before reading the table.
		if err := p("CAVEAT: one repeat, so run-to-run variance is unknown and no difference\n" +
			"  between configurations can be distinguished from noise. Use -repeats to bound it.\n\n"); err != nil {
			return err
		}
	}
	if starved := r.sampleStarvedCells(); len(starved) > 0 {
		if err := p("CAVEAT: %d cell(s) have too few samples for a meaningful p99.\n"+
			"  At N samples the nearest-rank p99 is rank ceil(0.99N), so below ~100 samples it\n"+
			"  is the maximum or near it: a single worst observation, not a tail estimate.\n",
			len(starved)); err != nil {
			return err
		}
		for _, c := range starved {
			if err := p("  %s @ %d (repeat %d): %d samples, p99 is rank %d of %d\n",
				c.Config, c.Density, c.Repeat, c.SampleCount,
				nearestRank(c.SampleCount, 99), c.SampleCount); err != nil {
				return err
			}
		}
		if err := p("\n"); err != nil {
			return err
		}
	}

	var untrustworthy []CellResult
	for _, c := range r.Cells {
		if !c.Trustworthy() {
			untrustworthy = append(untrustworthy, c)
		}
	}
	if len(untrustworthy) > 0 {
		if err := p("CAVEATS: %d of %d cells should not be quoted without qualification\n",
			len(untrustworthy), len(r.Cells)); err != nil {
			return err
		}
		for _, c := range untrustworthy {
			reason := "no samples"
			switch {
			case len(c.Errors) > 0:
				reason = fmt.Sprintf("%d error(s): %s", len(c.Errors), c.Errors[0])
			case c.ShortfallPercent() > 5:
				reason = fmt.Sprintf("only %d of %d microVMs responded (%.0f%% short)",
					c.Responded, c.Density, c.ShortfallPercent())
			}
			if err := p("  %s @ %d (repeat %d): %s\n",
				c.Config, c.Density, c.Repeat, reason); err != nil {
				return err
			}
		}
		if err := p("\n"); err != nil {
			return err
		}
	}

	if len(r.SkippedConfigs) > 0 {
		if err := p("SKIPPED CONFIGURATIONS\n"); err != nil {
			return err
		}
		var keys []string
		for k := range r.SkippedConfigs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := p("  %s: %s\n", k, r.SkippedConfigs[k]); err != nil {
				return err
			}
		}
		if err := p("\n"); err != nil {
			return err
		}
	}

	// Both PSS columns, because one number here invited the reading that boot cost was the whole
	// memory story. A large gap between them is the workload's own footprint.
	if err := p("%-4s %8s %5s %6s %8s %10s %10s %10s %9s %9s %8s\n",
		"cfg", "density", "rpt", "resp", "samples", "mean us", "p99 us", "jitter us",
		"PSS boot", "PSS work", "trust"); err != nil {
		return err
	}
	for _, c := range r.Cells {
		if err := p("%-4s %8d %5d %6d %8d %10.1f %10.1f %10.1f %9d %9d %8t\n",
			c.Config, c.Density, c.Repeat, c.Responded, c.SampleCount,
			c.MeanLatencyUS, c.P99LatencyUS, c.JitterUS,
			c.FleetPSSKiBBoot/1024, c.FleetPSSKiBWorking/1024, c.Trustworthy()); err != nil {
			return err
		}
	}

	if err := r.summarizeVariance(w); err != nil {
		return err
	}
	return r.summarizeFindings(w)
}

// nearestRank returns the 1-based rank Percentile uses for p on n samples. Kept here so the
// summary can state which observation a p99 actually is, which is the whole point of the
// sample-starvation warning.
func nearestRank(n int, p float64) int {
	if n <= 0 {
		return 0
	}
	rank := int(float64(n)*p/100 + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return rank
}

// minSamplesForP99 is the sample count below which a p99 is not meaningfully distinct from the
// maximum. At 100 samples the p99 is rank 99 of 100, which is the first rank that is not the
// maximum, so this is the smallest defensible threshold rather than a round number.
const minSamplesForP99 = 100

// sampleStarvedCells returns cells whose p99 is the maximum or adjacent to it.
func (r *Results) sampleStarvedCells() []CellResult {
	var out []CellResult
	for _, c := range r.Cells {
		if c.SampleCount > 0 && c.SampleCount < minSamplesForP99 {
			out = append(out, c)
		}
	}
	return out
}

// summarizeVariance reports how much each cell moved across its repeats.
//
// Nothing is printed for a single-repeat sweep: a spread of zero across one observation would
// read as stability when it is really an absence of measurement.
func (r *Results) summarizeVariance(w io.Writer) error {
	p := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}

	variance := r.Variance()
	multi := false
	for _, v := range variance {
		if v.Repeats > 1 {
			multi = true
			break
		}
	}
	if !multi {
		return nil
	}

	if err := p("\nACROSS-REPEAT VARIANCE\n"); err != nil {
		return err
	}
	// p50, p99 and p100 side by side, plus the two ratios. The ratios are the point of the
	// table: p99/p50 bounds max-of-G group-completion amplification, and p100/p50 diverging
	// from it says the extreme is pulling away from the tail.
	if err := p("%-4s %8s %5s %10s %10s %10s %7s %7s %10s %6s\n",
		"cfg", "density", "rpts", "med p50", "med p99", "med p100",
		"p99/p50", "p100/50", "spread %", "trust"); err != nil {
		return err
	}
	var worst CellVariance
	for _, v := range variance {
		if v.Repeats < 2 {
			continue
		}
		if err := p("%-4s %8d %5d %10.0f %10.0f %10.0f %7.2f %7.2f %10.1f %6t\n",
			v.Config, v.Density, v.Repeats,
			v.MedianP50US, v.MedianP99US, v.MedianP100US,
			v.TailRatio(), v.WorstRatio(),
			v.SpreadPercent(), v.AllTrustworthy); err != nil {
			return err
		}
		if v.SpreadPercent() > worst.SpreadPercent() {
			worst = v
		}
	}

	if worst.Repeats > 1 {
		// The largest spread is the effect size a finding must exceed to be reportable. Saying
		// so explicitly is the difference between a dataset that supports a claim and one that
		// merely looks like it does.
		if err := p("\n  Largest p99 spread: %s @ %d moved %.1f%% across %d identical repeats.\n",
			worst.Config, worst.Density, worst.SpreadPercent(), worst.Repeats); err != nil {
			return err
		}
		if err := p("  Treat differences smaller than that as noise rather than as findings.\n"); err != nil {
			return err
		}
	}
	return nil
}

// summarizeFindings highlights comparisons the experiment was designed to make, so a reader
// does not have to derive them from the table by hand.
func (r *Results) summarizeFindings(w io.Writer) error {
	p := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}

	if err := p("\nFINDINGS\n"); err != nil {
		return err
	}

	// Highest density that stayed trustworthy, per configuration.
	best := map[placement.Config]int{}
	for _, c := range r.Cells {
		if c.Trustworthy() && c.Density > best[c.Config] {
			best[c.Config] = c.Density
		}
	}
	if len(best) > 0 {
		var cfgs []placement.Config
		for k := range best {
			cfgs = append(cfgs, k)
		}
		sort.Slice(cfgs, func(i, j int) bool { return cfgs[i] < cfgs[j] })
		if err := p("  Highest trustworthy density per configuration:\n"); err != nil {
			return err
		}
		for _, cfg := range cfgs {
			if err := p("    %s: %d microVMs\n", cfg, best[cfg]); err != nil {
				return err
			}
		}
	}

	// The SMT comparison the sweep exists to make: config B (one physical core each)
	// against config E (two threads per core) at equal density.
	//
	// With repeats available the comparison uses each configuration's median p99 across
	// repeats and qualifies the delta against the observed spread. Comparing two single
	// observations, as this did before, reports a difference that may be smaller than the
	// noise floor without saying so.
	if b, e, density, ok := r.compareAtEqualDensity(placement.ConfigB, placement.ConfigE); ok {
		delta := 100 * (e.MedianP99US - b.MedianP99US) / b.MedianP99US
		if err := p("\n  At density %d, oversubscribing SMT siblings (E) versus one physical\n", density); err != nil {
			return err
		}
		if err := p("  core per microVM (B) changes median p99 latency by %+.1f%%\n", delta); err != nil {
			return err
		}
		if err := p("    B: mean %.1f us, p99 %.1f us (range %.1f-%.1f over %d repeat(s))\n",
			b.MeanOfMeansUS, b.MedianP99US, b.MinP99US, b.MaxP99US, b.Repeats); err != nil {
			return err
		}
		if err := p("    E: mean %.1f us, p99 %.1f us (range %.1f-%.1f over %d repeat(s))\n",
			e.MeanOfMeansUS, e.MedianP99US, e.MinP99US, e.MaxP99US, e.Repeats); err != nil {
			return err
		}
		switch {
		case b.Repeats < 2 || e.Repeats < 2:
			if err := p("    Single repeat: this delta cannot be separated from run-to-run noise.\n"); err != nil {
				return err
			}
		case b.MaxP99US >= e.MinP99US && e.MaxP99US >= b.MinP99US:
			// Overlapping ranges mean at least one B repeat was slower than at least one E
			// repeat, so the ordering is not established by this data.
			if err := p("    Ranges overlap, so this delta is not established: some B repeats\n" +
				"    were slower than some E repeats.\n"); err != nil {
				return err
			}
		default:
			if err := p("    Ranges do not overlap across repeats, so the direction holds.\n"); err != nil {
				return err
			}
		}
	}

	// Whether idle fleet memory is dominated by sharing, which is the warm pool question.
	for _, c := range r.Cells {
		if c.Trustworthy() && c.SharingRatioBoot > 0 {
			if err := p("\n  Memory sharing at %s density %d: RSS sum is %.1fx the PSS sum,\n",
				c.Config, c.Density, c.SharingRatioBoot); err != nil {
				return err
			}
			if err := p("  so an RSS-based estimate would overstate fleet memory by that factor.\n"); err != nil {
				return err
			}
			// Both, and the growth between them. Sizing a fleet on the boot figure is what the
			// unqualified single number used to invite: it is the cost of an idle microVM, not
			// of one doing work.
			if err := p("  Marginal cost per microVM (private dirty): %d KiB at boot",
				c.PerVMPrivateDirtyKiBBoot); err != nil {
				return err
			}
			if c.PerVMPrivateDirtyKiBWorking > 0 {
				growth := 0.0
				if c.PerVMPrivateDirtyKiBBoot > 0 {
					growth = float64(c.PerVMPrivateDirtyKiBWorking) / float64(c.PerVMPrivateDirtyKiBBoot)
				}
				if err := p(", %d KiB after the workload (%.1fx)\n",
					c.PerVMPrivateDirtyKiBWorking, growth); err != nil {
					return err
				}
				if err := p("  Size a fleet on the second figure: the first is an idle microVM.\n"); err != nil {
					return err
				}
			} else if err := p("\n"); err != nil {
				return err
			}
			break
		}
	}

	return nil
}

// compareAtEqualDensity finds the highest density at which both configurations produced
// trustworthy results, so a comparison is never made across different fleet sizes.
//
// It returns across-repeat summaries rather than individual cells, so a comparison carries the
// spread of each side and not just one observation from each.
func (r *Results) compareAtEqualDensity(a, b placement.Config) (va, vb CellVariance, density int, ok bool) {
	byDensity := map[int]map[placement.Config]CellVariance{}
	for _, v := range r.Variance() {
		if !v.AllTrustworthy || v.MedianP99US == 0 {
			continue
		}
		if byDensity[v.Density] == nil {
			byDensity[v.Density] = map[placement.Config]CellVariance{}
		}
		byDensity[v.Density][v.Config] = v
	}
	var densities []int
	for d := range byDensity {
		densities = append(densities, d)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(densities)))
	for _, d := range densities {
		if x, okA := byDensity[d][a]; okA {
			if y, okB := byDensity[d][b]; okB {
				return x, y, d, true
			}
		}
	}
	return CellVariance{}, CellVariance{}, 0, false
}

// runReply is a parsed guest response.
type runReply struct {
	Iterations uint64
	TotalUS    int64
	MeanNS     uint64
	Checksum   uint64

	// Kind is "compute", "cache", or "sandbox", taken from the reply tag rather than from what
	// the host asked for. Compared against the request so a guest answering the wrong workload
	// is caught rather than silently recorded under the wrong label.
	Kind string

	// Cache workload fields.
	WorkingSetKiB uint64
	// SetupUS is buffer allocation and shuffling, which the guest measures outside the timed
	// region. Recorded so that exclusion is verifiable rather than assumed.
	SetupUS int64

	// Task workload fields.
	//
	// HasExitCode distinguishes "exit code 0" from "no exit code reported". Without it a
	// compute-workload reply, whose ExitCode is zero by default, would be indistinguishable from
	// a task that passed, and the pass-rate check would silently pass every non-task cell.
	ExitCode    int
	HasExitCode bool
	PeakRSSKiB  uint64

	// Episode workload fields.
	//
	// TurnsUS and VerifyUS are components of TotalUS, and keeping them apart is the whole reason
	// this workload exists: every earlier figure in this project timed a verifier run and called it
	// an episode. Their ratio is what shows how much of an episode's sandbox cost the verifier
	// actually is.
	Turns    int
	TurnsOK  int
	TurnsUS  int64
	VerifyUS int64

	// Sandbox workload fields.
	ResidentKiB uint64
	Steps       uint64
	WriteKiB    uint64
	// SpawnUS and WriteUS are components of TotalUS: aggregated time inside fork/exec and
	// inside file writes. Separated because those are the paths the other workloads never
	// exercise, so being able to attribute a density effect to one of them is the point.
	SpawnUS int64
	WriteUS int64
	// SpawnsOK is children successfully reaped. Fewer than Steps means fork failed, which at
	// density usually means a PID or memory limit was hit and is a result rather than an error.
	SpawnsOK uint64
}

// fillLatency populates the latency fields of a cell from collected samples.
func fillLatency(c *CellResult, l *measure.LatencySamples) {
	if l.Len() == 0 {
		return
	}
	const us = float64(time.Microsecond)
	c.SampleCount = l.Len()
	c.MeanLatencyUS = float64(l.Mean()) / us
	c.P50LatencyUS = float64(l.Percentile(50)) / us
	c.P99LatencyUS = float64(l.Percentile(99)) / us
	c.MaxLatencyUS = float64(l.Max()) / us
	c.JitterUS = float64(l.Jitter()) / us
}

// fillLaunch populates launch latency fields.
func fillLaunch(c *CellResult, l *measure.LatencySamples) {
	if l.Len() == 0 {
		return
	}
	const msf = float64(time.Millisecond)
	c.MeanLaunchMS = float64(l.Mean()) / msf
	c.P99LaunchMS = float64(l.Percentile(99)) / msf
}
