// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package sweep

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
)

// m8iTopology is the shape measured on a real m8i.metal-48xl: 96 physical cores, 2 threads
// each, 3 NUMA nodes on one socket, SMT siblings offset by 96.
func m8iTopology() *topology.Topology { return synth(96, 2, 3) }

// m8aTopology is the m8a.metal-48xl shape: 192 physical cores, one thread each, no SMT.
func m8aTopology() *topology.Topology { return synth(192, 1, 4) }

func synth(cores, threads, nodes int) *topology.Topology {
	t := &topology.Topology{}
	coresPerNode := cores / nodes
	for cpu := 0; cpu < cores*threads; cpu++ {
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

func testRunner(top *topology.Topology, cfg Config) *Runner {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &Runner{cfg: cfg, top: top}
}

func TestBuildMatrixExpandsConfigsAndDensities(t *testing.T) {
	r := testRunner(m8iTopology(), Config{
		Configs:   []placement.Config{placement.ConfigB, placement.ConfigE},
		Densities: []int{10, 90},
	})
	var results Results
	cells := r.buildMatrix(&results)

	if len(cells) != 4 {
		t.Fatalf("buildMatrix returned %d cells, want 4 (2 configs x 2 densities)", len(cells))
	}
	// Matrix order is config-major, which is the order a reader of the plan expects.
	want := []matrixCell{
		{placement.ConfigB, 10}, {placement.ConfigB, 90},
		{placement.ConfigE, 10}, {placement.ConfigE, 90},
	}
	for i, w := range want {
		if cells[i] != w {
			t.Errorf("cell %d = %v, want %v", i, cells[i], w)
		}
	}
	if len(results.SkippedConfigs) != 0 {
		t.Errorf("skipped %v on a host that supports both", results.SkippedConfigs)
	}
}

// Skips must be recorded once for the whole run, not once per repeat, or a 5-repeat sweep would
// report the same skip five times and imply five separate observations.
func TestBuildMatrixRecordsSkipsOnce(t *testing.T) {
	r := testRunner(m8aTopology(), Config{
		Configs:   []placement.Config{placement.ConfigB, placement.ConfigC, placement.ConfigE},
		Densities: []int{10, 90},
		Repeats:   5,
	})
	var results Results
	cells := r.buildMatrix(&results)

	// Only B survives on a no-SMT host; C and E require siblings.
	if len(cells) != 2 {
		t.Fatalf("buildMatrix returned %d cells, want 2 (config B at two densities)", len(cells))
	}
	for _, c := range cells {
		if c.config != placement.ConfigB {
			t.Errorf("cell %v should have been skipped on a no-SMT host", c)
		}
	}
	if len(results.SkippedConfigs) != 2 {
		t.Fatalf("recorded %d skips, want 2 (C and E)", len(results.SkippedConfigs))
	}
	for _, cfg := range []string{"C", "E"} {
		if reason := results.SkippedConfigs[cfg]; reason == "" {
			t.Errorf("config %s skipped without a reason", cfg)
		}
	}
}

// The seed is recorded so an ordering can be replayed if a result looks order-dependent. A
// shuffle that is not reproducible from the recorded seed makes that impossible.
func TestShuffleIsReproducibleFromSeed(t *testing.T) {
	cells := []matrixCell{
		{placement.ConfigA, 10}, {placement.ConfigB, 45}, {placement.ConfigC, 90},
		{placement.ConfigD, 135}, {placement.ConfigE, 180}, {placement.ConfigF, 81},
	}
	a := shuffled(cells, rand.New(rand.NewSource(42)))
	b := shuffled(cells, rand.New(rand.NewSource(42)))
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed produced different orders at %d: %v vs %v", i, a[i], b[i])
		}
	}
	// Different seeds should generally differ; with 6 cells a collision is 1 in 720.
	c := shuffled(cells, rand.New(rand.NewSource(43)))
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("seeds 42 and 43 produced identical orders; the seed is not being used")
	}
}

// Shuffling must not lose or duplicate cells: a dropped cell would silently shrink the matrix,
// and a duplicated one would double-count in the variance summary.
func TestShufflePreservesCellSet(t *testing.T) {
	cells := []matrixCell{
		{placement.ConfigA, 10}, {placement.ConfigB, 45}, {placement.ConfigC, 90},
		{placement.ConfigD, 135}, {placement.ConfigE, 180},
	}
	for seed := int64(0); seed < 20; seed++ {
		got := shuffled(cells, rand.New(rand.NewSource(seed)))
		if len(got) != len(cells) {
			t.Fatalf("seed %d: %d cells, want %d", seed, len(got), len(cells))
		}
		counts := map[matrixCell]int{}
		for _, c := range got {
			counts[c]++
		}
		for _, want := range cells {
			if counts[want] != 1 {
				t.Errorf("seed %d: cell %v appears %d times, want once", seed, want, counts[want])
			}
		}
	}
}

// shuffled must copy, not permute in place. Otherwise each repeat would shuffle the already
// shuffled order, so the recorded seed would not reproduce repeat N without replaying 0..N-1.
func TestShuffleDoesNotMutateInput(t *testing.T) {
	cells := []matrixCell{
		{placement.ConfigA, 10}, {placement.ConfigB, 45}, {placement.ConfigC, 90},
		{placement.ConfigD, 135}, {placement.ConfigE, 180},
	}
	original := make([]matrixCell, len(cells))
	copy(original, cells)

	for seed := int64(0); seed < 10; seed++ {
		shuffled(cells, rand.New(rand.NewSource(seed)))
	}
	for i := range original {
		if cells[i] != original[i] {
			t.Fatalf("input was mutated at %d: %v, want %v", i, cells[i], original[i])
		}
	}
}

func TestValidateRejectsBadRepeatsAndSamples(t *testing.T) {
	base := DefaultConfig()
	// Point artifacts at something that exists so only the new fields are under test.
	base.FirecrackerBin = "/bin/sh"
	base.JailerBin = "/bin/sh"
	base.KernelPath = "/bin/sh"
	base.RootfsPath = "/bin/sh"
	base.Configs = []placement.Config{placement.ConfigB}
	base.Densities = []int{10}

	if err := base.Validate(); err != nil {
		t.Fatalf("the default configuration should validate: %v", err)
	}

	zeroRepeats := base
	zeroRepeats.Repeats = 0
	if err := zeroRepeats.Validate(); err == nil {
		t.Error("Validate accepted 0 repeats, which would run no cells at all")
	}

	negTarget := base
	negTarget.Workload.TargetSamples = -1
	if err := negTarget.Validate(); err == nil {
		t.Error("Validate accepted a negative target sample count")
	}
}

// The episode workload takes precedence over every other, and reports its own request tag.
//
// An episode ends in a verifier run, so a caller who set both wants the whole episode. Silently
// running only the last step of it -- and labelling the result "task" -- is precisely the confusion
// this workload was added to correct.
func TestEpisodeTakesPrecedenceOverTask(t *testing.T) {
	w := WorkloadSpec{
		Iterations: 5_000_000,
		Task:       &TaskSpec{InstanceID: "some-task"},
		Episode:    &EpisodeSpec{InstanceID: "marshmallow-1867", Turns: 14},
	}
	if got := w.Request(); got != "RUN_EPISODE" {
		t.Errorf("Request() = %q, want RUN_EPISODE", got)
	}
	if got := w.Kind(); got != "episode" {
		t.Errorf("Kind() = %q, want episode", got)
	}
	// And the tag must match what the guest replies with, or the mismatch guard rejects every cell.
	rr, ok := parseRunReply("OKE 14 13 3500000 2000000 1500000 0 185000")
	if !ok || rr.Kind != w.Kind() {
		t.Errorf("guest reply kind %q does not match requested kind %q", rr.Kind, w.Kind())
	}
}

// A cell whose replay ran a different number of turns than the trajectory is not a result.
//
// An episode that stopped partway is *fast*, so without this it would be quoted as a cheap episode.
// Same trap as the exit-127 task that read as a 2 ms success.
func TestCellWithTurnShortfallIsUntrustworthy(t *testing.T) {
	c := &CellResult{
		SampleCount: 100,
		Launched:    10,
		Responded:   10,
		TaskRuns:    10,
		TaskPassed:  10,
	}
	if !c.Trustworthy() {
		t.Fatal("a cell with no errors and a full pass rate should be trustworthy")
	}
	c.EpisodeTurnShortfalls = 1
	if c.Trustworthy() {
		t.Error("a cell whose replay diverged from the trajectory must not be trustworthy")
	}
}

func TestValidateRejectsBadEpisodeSpec(t *testing.T) {
	base := DefaultConfig()
	base.FirecrackerBin = "/bin/sh"
	base.JailerBin = "/bin/sh"
	base.KernelPath = "/bin/sh"
	base.RootfsPath = "/bin/sh"
	base.Configs = []placement.Config{placement.ConfigB}
	base.Densities = []int{10}

	// No turn count: nothing to check a replay against, so a truncated episode reads as a cheap one.
	noTurns := base
	noTurns.Workload.Episode = &EpisodeSpec{InstanceID: "x"}
	if err := noTurns.Validate(); err == nil {
		t.Error("Validate accepted an episode with no turn count")
	}

	// An episode is far longer than a verifier run, so the timeout guard matters more here.
	tooShort := base
	tooShort.RequestTimeout = 60 * time.Second
	tooShort.Workload.Episode = &EpisodeSpec{InstanceID: "x", Turns: 14, TimeoutSec: 600}
	if err := tooShort.Validate(); err == nil {
		t.Error("Validate accepted a request timeout below the episode's own limit")
	}

	// Both workloads set: an episode already ends in a verifier run.
	both := base
	both.Workload.Episode = &EpisodeSpec{InstanceID: "x", Turns: 14}
	both.Workload.Task = &TaskSpec{InstanceID: "y"}
	if err := both.Validate(); err == nil {
		t.Error("Validate accepted both -episode-instance and -task-instance")
	}

	ok := base
	ok.RequestTimeout = 900 * time.Second
	ok.Workload.Episode = &EpisodeSpec{
		InstanceID: "x", Turns: 14, ExpectedTurnsOK: 13, TimeoutSec: 600,
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate rejected a well-formed episode spec: %v", err)
	}
}

// A request timeout below the task's own limit must be refused.
//
// The guest deliberately has no timeout, so the host's request deadline is what cuts a long task
// off -- and a cut-off request is counted as a *failed* task, not a slow one. That corrupts the
// cell's pass rate, which is the gate on whether any latency figure from that cell may be quoted.
// Terminal-Bench tasks declare max_test_timeout_sec of 180s against a 120s default, so this is the
// live case rather than a hypothetical.
func TestValidateRejectsRequestTimeoutBelowTaskTimeout(t *testing.T) {
	base := DefaultConfig()
	base.FirecrackerBin = "/bin/sh"
	base.JailerBin = "/bin/sh"
	base.KernelPath = "/bin/sh"
	base.RootfsPath = "/bin/sh"
	base.Configs = []placement.Config{placement.ConfigB}
	base.Densities = []int{10}
	base.Workload.Task = &TaskSpec{InstanceID: "hello-world", TimeoutSec: 180}

	tooShort := base
	tooShort.RequestTimeout = 120 * time.Second
	if err := tooShort.Validate(); err == nil {
		t.Error("Validate accepted a 120s request timeout for a task allowed 180s; tasks that ran " +
			"longer would be recorded as failures and the pass rate would be wrong")
	}

	enough := base
	enough.RequestTimeout = 200 * time.Second
	if err := enough.Validate(); err != nil {
		t.Errorf("Validate rejected a request timeout above the task's limit: %v", err)
	}

	// Unstated means no check: the synthetic workloads have no task-declared limit, and demanding
	// one would break every existing sweep.
	unstated := base
	unstated.RequestTimeout = 1 * time.Second
	unstated.Workload.Task = &TaskSpec{InstanceID: "hello-world"}
	if err := unstated.Validate(); err != nil {
		t.Errorf("Validate rejected an unstated task timeout: %v", err)
	}
}

// The defaults are what a session actually runs, so they must satisfy the sampling properties
// the flags exist to guarantee.
func TestDefaultsProduceAdequateSamples(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Repeats < 2 {
		t.Errorf("default Repeats = %d; a default of 1 yields no variance information", cfg.Repeats)
	}
	if !cfg.ShuffleCells {
		t.Error("default ShuffleCells = false; a fixed order bakes adjacency effects into results")
	}
	// Every density in the lab plan must clear the p99 threshold at the default settings.
	for _, density := range []int{10, 45, 90, 135, 180} {
		samples := cfg.Workload.RoundsFor(density) * density
		if samples < minSamplesForP99 {
			t.Errorf("default settings at density %d yield %d samples, under the %d needed",
				density, samples, minSamplesForP99)
		}
	}
}

// measureMemory writes into the fields for the phase it is given, and never the other's.
//
// One function serves both call sites, so a wrong switch arm would overwrite the boot figure with
// the working one and silently restore the defect this change exists to fix.
func TestMeasureMemoryWritesOnlyItsOwnPhase(t *testing.T) {
	cell := CellResult{
		FleetPSSKiBBoot:    900,
		FleetPSSKiBWorking: 8000,
	}
	// No live VMs, so measureMemory returns before touching anything. That is the point: it must
	// leave both phases alone rather than zeroing the one it was asked for.
	r := testRunner(synth(8, 1, 2), DefaultConfig())
	r.measureMemory(nil, &cell, memoryPhaseWorking)
	if cell.FleetPSSKiBBoot != 900 || cell.FleetPSSKiBWorking != 8000 {
		t.Errorf("a no-op measurement changed the cell: boot=%d working=%d",
			cell.FleetPSSKiBBoot, cell.FleetPSSKiBWorking)
	}
}

// The working figure is quoted as a high-water mark, which holds only because no balloon device is
// configured and nothing calls MADV_DONTNEED, so guest pages stay resident in the VMM's mapping once
// faulted. A decrease means that premise is false and every working-set figure in the run describes
// something other than a peak, so it must be recorded rather than silently published.
func TestWorkingSetBelowBootIsRecordedAsAnError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		boot, work int
		wantErr    bool
	}{
		{"growth is the finding, not a fault", 900, 8000, false},
		{"equal is normal for a workload that touches nothing new", 900, 900, false},
		{"a decrease contradicts the model", 900, 400, true},
		{"no boot figure means nothing to compare", 0, 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell := CellResult{FleetPSSKiBBoot: tc.boot}
			cell.CheckWorkingSetMonotonic(tc.work)

			if got := len(cell.Errors) > 0; got != tc.wantErr {
				t.Errorf("boot=%d work=%d recorded error = %t, want %t",
					tc.boot, tc.work, got, tc.wantErr)
			}
			if cell.WorkingSetRegression != tc.wantErr {
				t.Errorf("WorkingSetRegression = %t, want %t", cell.WorkingSetRegression, tc.wantErr)
			}
			// The operator has to be able to tell which figures are suspect, so both must appear.
			if tc.wantErr {
				msg := cell.Errors[0]
				if !strings.Contains(msg, "900") || !strings.Contains(msg, "400") {
					t.Errorf("error names neither figure: %q", msg)
				}
			}
		})
	}
}
