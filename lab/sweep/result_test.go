// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package sweep

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/measure"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
)

func TestShortfallPercent(t *testing.T) {
	cases := []struct {
		density, responded int
		want               float64
	}{
		{90, 90, 0},
		{90, 87, 3.3333},
		{90, 81, 10},
		{100, 0, 100},
		{0, 0, 0}, // must not divide by zero
	}
	// Compare with a tolerance: pinning an exact float literal tests the literal's
	// precision rather than the calculation.
	const tol = 0.001
	for _, c := range cases {
		cell := CellResult{Density: c.density, Responded: c.responded}
		got := cell.ShortfallPercent()
		if diff := got - c.want; diff > tol || diff < -tol {
			t.Errorf("density %d responded %d: shortfall = %v, want %v (+/- %v)",
				c.density, c.responded, got, c.want, tol)
		}
	}
}

// A cell that lost a tenth of its fleet saw less contention than the configuration
// nominally creates, so its latency figures must not be quoted unqualified.
func TestTrustworthyRejectsLargeShortfall(t *testing.T) {
	cases := []struct {
		name string
		cell CellResult
		want bool
	}{
		{
			"full fleet, samples, no errors",
			CellResult{Density: 90, Responded: 90, SampleCount: 450},
			true,
		},
		{
			"3 of 90 missing is within tolerance",
			CellResult{Density: 90, Responded: 87, SampleCount: 435},
			true,
		},
		{
			"10% short is not trustworthy",
			CellResult{Density: 90, Responded: 81, SampleCount: 405},
			false,
		},
		{
			"errors present, even with a full fleet",
			CellResult{Density: 90, Responded: 90, SampleCount: 450,
				Errors: []string{"swap is in use"}},
			false,
		},
		{
			"no samples means nothing to report",
			CellResult{Density: 90, Responded: 90, SampleCount: 0},
			false,
		},
	}
	for _, c := range cases {
		if got := c.cell.Trustworthy(); got != c.want {
			t.Errorf("%s: Trustworthy() = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestSkipRecordsReason(t *testing.T) {
	var r Results
	r.Skip(placement.ConfigC, "requires SMT; host reports 1 thread per core")

	if len(r.SkippedConfigs) != 1 {
		t.Fatalf("SkippedConfigs has %d entries, want 1", len(r.SkippedConfigs))
	}
	// A skipped configuration must carry its reason, or an absent row reads as an
	// oversight rather than a property of the hardware.
	if !strings.Contains(r.SkippedConfigs["C"], "requires SMT") {
		t.Errorf("skip reason = %q, want it to explain why", r.SkippedConfigs["C"])
	}
}

func TestFillLatencyFromSamples(t *testing.T) {
	var l measure.LatencySamples
	for i := 1; i <= 100; i++ {
		l.Add(time.Duration(i) * time.Millisecond)
	}

	var cell CellResult
	fillLatency(&cell, &l)

	if cell.SampleCount != 100 {
		t.Errorf("SampleCount = %d, want 100", cell.SampleCount)
	}
	// 1..100 ms in microseconds: mean 50.5 ms = 50500 us, p99 = 99000 us.
	if cell.MeanLatencyUS != 50500 {
		t.Errorf("MeanLatencyUS = %v, want 50500", cell.MeanLatencyUS)
	}
	if cell.P99LatencyUS != 99000 {
		t.Errorf("P99LatencyUS = %v, want 99000", cell.P99LatencyUS)
	}
	if cell.MaxLatencyUS != 100000 {
		t.Errorf("MaxLatencyUS = %v, want 100000", cell.MaxLatencyUS)
	}
	if cell.JitterUS != 99000 {
		t.Errorf("JitterUS = %v, want 99000", cell.JitterUS)
	}
}

func TestFillLatencyEmptyLeavesZero(t *testing.T) {
	var l measure.LatencySamples
	var cell CellResult
	fillLatency(&cell, &l)
	if cell.SampleCount != 0 || cell.MeanLatencyUS != 0 {
		t.Error("fillLatency on empty samples should leave the cell untouched")
	}
}

func TestParseRunReply(t *testing.T) {
	// The agent replies "OK <iterations> <total_us> <mean_ns> <checksum>".
	rr, ok := parseRunReply("OK 5000000 7739 1 11372319519890737312")
	if !ok {
		t.Fatal("failed to parse a valid reply")
	}
	if want := 7739 * time.Microsecond; rr.Duration() != want {
		t.Errorf("Duration() = %v, want %v", rr.Duration(), want)
	}
	// Every field is retained, because they all end up in the raw sample row.
	if rr.Iterations != 5000000 {
		t.Errorf("Iterations = %d, want 5000000", rr.Iterations)
	}
	if rr.MeanNS != 1 {
		t.Errorf("MeanNS = %d, want 1", rr.MeanNS)
	}
	if rr.Checksum != 11372319519890737312 {
		t.Errorf("Checksum = %d, want 11372319519890737312", rr.Checksum)
	}
}

// Raw samples are the reanalysable artifact: percentiles cannot be recomputed from
// percentiles, so every field needed for later analysis must survive the CSV round trip.
func TestWriteSamplesCSV(t *testing.T) {
	r := &Results{}
	r.AddSamples([]Sample{
		{Config: placement.ConfigB, Density: 90, VMIndex: 0, Node: 0, Round: 0,
			DurationUS: 7736.0, GuestMeanNS: 1, Checksum: 11372319519890737312, CPUSet: "2"},
		{Config: placement.ConfigB, Density: 90, VMIndex: 1, Node: 1, Round: 0,
			DurationUS: 7840.0, GuestMeanNS: 1, Checksum: 11372319519890737312, CPUSet: "34"},
	})

	var sb strings.Builder
	if err := r.WriteSamplesCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != 3 { // header + 2 samples
		t.Fatalf("samples CSV has %d lines, want 3", len(lines))
	}
	// The cpuset and node must travel with each sample, so a slow measurement can be traced
	// back to specific CPUs. That traceability is the reason for keeping per-VM rows at all.
	for _, want := range []string{"numa_node", "checksum", "cpuset"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q", want)
		}
	}
	if !strings.HasSuffix(lines[1], ",2") {
		t.Errorf("row 1 = %q, want it to end with cpuset 2", lines[1])
	}
	if !strings.HasSuffix(lines[2], ",34") {
		t.Errorf("row 2 = %q, want it to end with cpuset 34", lines[2])
	}
}

func TestSamplesExcludedFromSummaryJSON(t *testing.T) {
	r := &Results{Cells: []CellResult{{Config: placement.ConfigB, Density: 10}}}
	r.AddSamples([]Sample{{Config: placement.ConfigB, Density: 10, DurationUS: 7736.0}})

	var sb strings.Builder
	if err := r.WriteJSON(&sb); err != nil {
		t.Fatal(err)
	}
	// Samples are excluded from the JSON summary deliberately: at tens of thousands of rows,
	// inlining them would make the summary unreadable and slow to parse.
	if strings.Contains(sb.String(), "duration_us") {
		t.Error("raw samples were inlined into the JSON summary; they belong in their own file")
	}
}

// A reply without a checksum could come from a guest whose compute loop was optimised away,
// which would present as an implausibly fast host rather than a bug. Reject it.
func TestParseRunReplyRequiresChecksum(t *testing.T) {
	for _, reply := range []string{
		"OK 5000000 7739 1",         // checksum missing
		"OK 5000000 7739",           // truncated
		"OK",                        // bare
		"ERR unrecognised request",  // agent error
		"",                          // empty
		"OK 5000000 abc 1 123",      // non-numeric total
		"OK 5000000 7739 1 notanum", // non-numeric checksum
	} {
		if _, ok := parseRunReply(reply); ok {
			t.Errorf("parseRunReply(%q) accepted an invalid reply", reply)
		}
	}
}

func TestWriteCSVIncludesUntrustworthyRows(t *testing.T) {
	r := &Results{
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450, MeanLatencyUS: 1000},
			// A cell that hit the density ceiling. Filtering it out would hide the ceiling,
			// which is one of the results worth reporting.
			{Config: placement.ConfigB, Density: 180, Responded: 90, SampleCount: 450, MeanLatencyUS: 2000},
		},
	}
	var sb strings.Builder
	if err := r.WriteCSV(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 { // header plus two rows
		t.Fatalf("CSV has %d lines, want 3 (header + 2 cells)", len(lines))
	}
	if !strings.Contains(lines[1], "true") {
		t.Error("full-fleet row should be marked trustworthy")
	}
	if !strings.Contains(lines[2], "false") {
		t.Error("shortfall row should be marked untrustworthy, not omitted")
	}
}

// The summary must lead with caveats. Someone skimming for a density number should meet the
// reasons not to trust it before the number.
func TestSummarizeLeadsWithCaveats(t *testing.T) {
	r := &Results{
		Host: HostInfo{
			InstanceType: "m8i.metal-48xl", CPUModel: "Intel(R) Xeon(R) 6975P-C",
			PhysicalCores: 96, ThreadsPerCore: 2, NUMANodes: 3, L3Domains: 1,
			CacheMode: "unified-across-nodes", SMTState: "on",
			FirecrackerVer: "1.16.1", GuestKernel: "vmlinux-6.1.102",
		},
		Workload: WorkloadSpec{Iterations: 5_000_000, Rounds: 5, VCPUCount: 1, MemSizeMiB: 128},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450, MeanLatencyUS: 1000, P99LatencyUS: 1200},
			{Config: placement.ConfigB, Density: 180, Responded: 90, SampleCount: 450, MeanLatencyUS: 2000, P99LatencyUS: 5000},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	caveatIdx := strings.Index(out, "CAVEATS")
	tableIdx := strings.Index(out, "p99 us")
	if caveatIdx < 0 {
		t.Fatal("summary omitted the caveats section despite an untrustworthy cell")
	}
	if tableIdx >= 0 && caveatIdx > tableIdx {
		t.Error("caveats appear after the results table; they should come first")
	}
	// The host's cache relationship changes how cache counters must be read, so it belongs
	// in any report of these numbers.
	if !strings.Contains(out, "unified-across-nodes") {
		t.Error("summary omitted the cache mode, which is needed to interpret the results")
	}
}

func TestSummarizeReportsSkippedConfigs(t *testing.T) {
	r := &Results{
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450},
		},
	}
	r.Skip(placement.ConfigC, "requires SMT; host reports 1 thread per core")

	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "SKIPPED") || !strings.Contains(out, "requires SMT") {
		t.Error("summary should report skipped configurations and why")
	}
}

// The B-versus-E comparison is the SMT question the sweep exists to answer, and it must only
// be made at equal density: comparing different fleet sizes would confound density with
// placement strategy.
func TestCompareAtEqualDensityOnly(t *testing.T) {
	r := &Results{
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450, P99LatencyUS: 1000},
			{Config: placement.ConfigE, Density: 90, Responded: 90, SampleCount: 450, P99LatencyUS: 1500},
			// A higher density for E only. The comparison must not pair E@180 with B@90.
			{Config: placement.ConfigE, Density: 180, Responded: 180, SampleCount: 900, P99LatencyUS: 4000},
		},
	}
	b, e, density, ok := r.compareAtEqualDensity(placement.ConfigB, placement.ConfigE)
	if !ok {
		t.Fatal("no comparison found despite both configs present at density 90")
	}
	if density != 90 {
		t.Errorf("compared at density %d, want 90 (the only shared level)", density)
	}
	if b.MedianP99US != 1000 || e.MedianP99US != 1500 {
		t.Errorf("paired the wrong cells: B p99 %v, E p99 %v", b.MedianP99US, e.MedianP99US)
	}
}

func TestCompareSkipsUntrustworthyCells(t *testing.T) {
	r := &Results{
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450, P99LatencyUS: 1000},
			// E at the same density but with errors: must not be used for the comparison.
			{Config: placement.ConfigE, Density: 90, Responded: 90, SampleCount: 450,
				P99LatencyUS: 1500, Errors: []string{"launch failed"}},
		},
	}
	if _, _, _, ok := r.compareAtEqualDensity(placement.ConfigB, placement.ConfigE); ok {
		t.Error("comparison used an untrustworthy cell")
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	r := &Results{
		Host:     HostInfo{InstanceType: "m8i.metal-48xl", CacheMode: "unified-across-nodes"},
		Workload: WorkloadSpec{Iterations: 1000, Rounds: 2},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 10, Responded: 10, SampleCount: 20},
		},
	}
	var sb strings.Builder
	if err := r.WriteJSON(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"m8i.metal-48xl", "unified-across-nodes", "density_requested"} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %q", want)
		}
	}
}

func TestConfigValidateRejectsMissingArtifacts(t *testing.T) {
	c := DefaultConfig()
	c.Configs = []placement.Config{placement.ConfigB}
	c.Densities = []int{10}
	c.FirecrackerBin = "/definitely/not/here"

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a missing firecracker binary")
	}
	// Failing before hardware time is spent is the point, so the message must say what to fix.
	if !strings.Contains(err.Error(), "firecracker") {
		t.Errorf("error %q should name the missing artifact", err)
	}
}

func TestConfigValidateRequiresMatrix(t *testing.T) {
	base := DefaultConfig()
	// Point artifacts at something that exists so only the matrix is under test.
	base.FirecrackerBin = "/bin/sh"
	base.JailerBin = "/bin/sh"
	base.KernelPath = "/bin/sh"
	base.RootfsPath = "/bin/sh"

	noConfigs := base
	noConfigs.Densities = []int{10}
	if err := noConfigs.Validate(); err == nil {
		t.Error("Validate accepted an empty configuration list")
	}

	noDensities := base
	noDensities.Configs = []placement.Config{placement.ConfigB}
	if err := noDensities.Validate(); err == nil {
		t.Error("Validate accepted an empty density list")
	}

	rootUID := base
	rootUID.Configs = []placement.Config{placement.ConfigB}
	rootUID.Densities = []int{10}
	rootUID.BaseUID = 0
	if err := rootUID.Validate(); err == nil {
		t.Error("Validate accepted BaseUID 0, which would run jailed VMMs as root")
	}
}

// EC2 CpuOptions are fixed at launch, so a mismatch between the requested and observed thread
// count means the run measured a different SMT configuration than intended. Detecting that is
// the whole point of recording both values.
func TestSMTConsistent(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		observed  int
		want      bool
	}{
		{"smt-on host as requested", 2, 2, true},
		{"smt-off host as requested", 1, 1, true},
		{"requested off but SMT is on", 1, 2, false},
		{"requested on but SMT is off", 2, 1, false},
		{"caller stated no expectation", 0, 2, true},
	}
	for _, c := range cases {
		h := HostInfo{RequestedThreadsPerCore: c.requested, ThreadsPerCore: c.observed}
		if got := h.SMTConsistent(); got != c.want {
			t.Errorf("%s: SMTConsistent() = %t, want %t", c.name, got, c.want)
		}
	}
}

// A mismatch must appear before the results table. Someone reading for a density number has to
// meet the fact that the numbers describe the wrong configuration first.
func TestSummarizeLeadsWithSMTMismatch(t *testing.T) {
	r := &Results{
		Host: HostInfo{
			InstanceType: "m8i.metal-48xl", PhysicalCores: 96, ThreadsPerCore: 2,
			RequestedThreadsPerCore: 1, RunLabel: "smt-off",
			NUMANodes: 3, L3Domains: 1, CacheMode: "unified-across-nodes",
		},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7736, P99LatencyUS: 7840},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	idx := strings.Index(out, "SMT MISMATCH")
	if idx < 0 {
		t.Fatal("summary omitted the SMT mismatch warning")
	}
	if table := strings.Index(out, "p99 us"); table >= 0 && idx > table {
		t.Error("SMT mismatch warning appears after the results table; it must come first")
	}
	// The label distinguishes this dataset from its SMT-on counterpart.
	if !strings.Contains(out, "smt-off") {
		t.Error("summary omitted the run label, so this dataset is indistinguishable from the other host's")
	}
}

// A fixed round count makes low-density cells sample-starved: at 5 rounds a 10-microVM cell
// yields 50 samples, where the nearest-rank p99 is rank 50 of 50, the maximum. Scaling rounds
// inversely with density is what fixes that.
func TestRoundsForScalesInverselyWithDensity(t *testing.T) {
	w := WorkloadSpec{Rounds: 5, TargetSamples: 450}
	cases := []struct {
		density    int
		wantRounds int
	}{
		{10, 45}, // 450/10
		{45, 10}, // 450/45
		{90, 5},  // 450/90 = 5, equals the floor
		{135, 5}, // 450/135 rounds up to 4, below the floor of 5
		{180, 5}, // 450/180 = 2.5, well below the floor
		{7, 65},  // ceiling division: 450/7 = 64.3, so 65 to land at or above target
		{100, 5}, // 450/100 = 4.5 rounds up to 5
		{451, 5}, // one round would suffice, but the floor holds
	}
	for _, c := range cases {
		if got := w.RoundsFor(c.density); got != c.wantRounds {
			t.Errorf("RoundsFor(%d) = %d, want %d", c.density, got, c.wantRounds)
		}
	}
}

// Every cell must clear the threshold below which a p99 is indistinguishable from the maximum.
// That is the property the scaling exists for, so it is asserted directly rather than inferred
// from the round counts above.
func TestRoundsForClearsP99Threshold(t *testing.T) {
	w := WorkloadSpec{Rounds: 5, TargetSamples: 450}
	for _, density := range []int{10, 45, 81, 90, 135, 180} {
		samples := w.RoundsFor(density) * density
		if samples < minSamplesForP99 {
			t.Errorf("density %d yields %d samples, under the %d needed for p99 to differ from max",
				density, samples, minSamplesForP99)
		}
		// At the target, p99 must not be the maximum. This is the failure mode in the
		// 2026-08-01 dataset, where all six density-10 cells reported p99 == max.
		if rank := nearestRank(samples, 99); rank == samples {
			t.Errorf("density %d: p99 is rank %d of %d, which is the maximum", density, rank, samples)
		}
	}
}

// Rounds is a floor, so enabling target samples can only add measurements. Otherwise turning
// the feature on would silently reduce sampling at high density.
func TestRoundsForTreatsRoundsAsFloor(t *testing.T) {
	w := WorkloadSpec{Rounds: 20, TargetSamples: 450}
	if got := w.RoundsFor(180); got != 20 {
		t.Errorf("RoundsFor(180) = %d, want the floor of 20; scaling must never reduce rounds", got)
	}
}

func TestRoundsForDisabledAndDegenerateInputs(t *testing.T) {
	// Zero target disables scaling entirely.
	off := WorkloadSpec{Rounds: 5, TargetSamples: 0}
	if got := off.RoundsFor(10); got != 5 {
		t.Errorf("with TargetSamples 0, RoundsFor(10) = %d, want the configured 5", got)
	}
	// A zero or negative density must not divide by zero or return zero rounds.
	on := WorkloadSpec{Rounds: 5, TargetSamples: 450}
	for _, d := range []int{0, -1} {
		if got := on.RoundsFor(d); got != 5 {
			t.Errorf("RoundsFor(%d) = %d, want the configured 5", d, got)
		}
	}
	// Rounds below 1 must still produce at least one round, or a cell would collect nothing.
	zero := WorkloadSpec{Rounds: 0, TargetSamples: 0}
	if got := zero.RoundsFor(90); got != 1 {
		t.Errorf("RoundsFor with Rounds 0 = %d, want at least 1", got)
	}
}

// The cap exists so a very low density and a very high target cannot produce a session far
// longer than intended.
func TestRoundsForRespectsCap(t *testing.T) {
	w := WorkloadSpec{Rounds: 5, TargetSamples: 1_000_000}
	if got := w.RoundsFor(1); got != MaxRoundsPerCell {
		t.Errorf("RoundsFor(1) with a huge target = %d, want the cap of %d", got, MaxRoundsPerCell)
	}
}

// Repeats of the same cell must be summarised as a spread, not pooled. Pooling would hide the
// run-to-run movement that is the reason for repeating.
func TestVarianceAcrossRepeats(t *testing.T) {
	r := &Results{
		Repeats: 3,
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7700, P99LatencyUS: 7800, JitterUS: 150},
			{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7800, P99LatencyUS: 8000, JitterUS: 200},
			{Config: placement.ConfigB, Density: 90, Repeat: 2, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7900, P99LatencyUS: 7900, JitterUS: 175},
		},
	}
	v := r.Variance()
	if len(v) != 1 {
		t.Fatalf("Variance() returned %d groups, want 1 (same config and density)", len(v))
	}
	got := v[0]
	if got.Repeats != 3 {
		t.Errorf("Repeats = %d, want 3", got.Repeats)
	}
	if got.MeanOfMeansUS != 7800 {
		t.Errorf("MeanOfMeansUS = %v, want 7800", got.MeanOfMeansUS)
	}
	// Median of 7800, 7900, 8000 is 7900, not the mean. A p99 is already a tail statistic, so
	// one pathological repeat must not drag the summary.
	if got.MedianP99US != 7900 {
		t.Errorf("MedianP99US = %v, want 7900", got.MedianP99US)
	}
	if got.MinP99US != 7800 || got.MaxP99US != 8000 {
		t.Errorf("p99 range = %v-%v, want 7800-8000", got.MinP99US, got.MaxP99US)
	}
	if got.MinJitterUS != 150 || got.MaxJitterUS != 200 {
		t.Errorf("jitter range = %v-%v, want 150-200", got.MinJitterUS, got.MaxJitterUS)
	}
	// Spread is the effect size a finding must exceed: (8000-7800)/7900 = 2.53%.
	if diff := got.SpreadPercent() - 2.5316; diff > 0.001 || diff < -0.001 {
		t.Errorf("SpreadPercent() = %v, want ~2.532", got.SpreadPercent())
	}
	if !got.AllTrustworthy {
		t.Error("AllTrustworthy = false, but every repeat was a full clean fleet")
	}
}

// An even repeat count is the common case (4 repeats, 6 repeats), so the median must average
// the two middle values rather than picking one.
func TestMedianEvenCount(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{7800, 8000}, 7900},
		{[]float64{1, 2, 3, 4}, 2.5},
		{[]float64{5}, 5},
		{nil, 0},
	}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// One bad repeat must mark the whole group, because a spread that mixes measurement noise with
// partial failure is not a variance estimate.
func TestVarianceFlagsUntrustworthyRepeat(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				P99LatencyUS: 7800, MeanLatencyUS: 7700},
			{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				P99LatencyUS: 9000, MeanLatencyUS: 8000, Errors: []string{"launch failed"}},
		},
	}
	v := r.Variance()
	if len(v) != 1 {
		t.Fatalf("Variance() returned %d groups, want 1", len(v))
	}
	if v[0].AllTrustworthy {
		t.Error("AllTrustworthy = true despite a repeat with errors")
	}
}

// A cell that measured nothing must not be averaged in as a zero, which would drag every
// statistic toward zero and make a broken cell look fast.
func TestVarianceIgnoresZeroSampleCells(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7800, P99LatencyUS: 7900},
			// Nothing measured: no samples, so all latency fields are zero.
			{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 0, SampleCount: 0},
		},
	}
	v := r.Variance()[0]
	if v.MeanOfMeansUS != 7800 {
		t.Errorf("MeanOfMeansUS = %v, want 7800; the empty repeat must not be averaged in", v.MeanOfMeansUS)
	}
	if v.MinP99US != 7900 {
		t.Errorf("MinP99US = %v, want 7900; a zero from an empty cell must not become the minimum", v.MinP99US)
	}
}

// Single-repeat cells are reported with their repeat count rather than omitted, so a reader can
// see which cells have no variance information instead of inferring it from absence.
func TestVarianceIncludesSingleRepeatCells(t *testing.T) {
	r := &Results{
		Repeats: 1,
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450, P99LatencyUS: 7900},
			{Config: placement.ConfigE, Density: 90, Responded: 90, SampleCount: 450, P99LatencyUS: 8400},
		},
	}
	v := r.Variance()
	if len(v) != 2 {
		t.Fatalf("Variance() returned %d groups, want 2", len(v))
	}
	for _, g := range v {
		if g.Repeats != 1 {
			t.Errorf("%s @ %d: Repeats = %d, want 1", g.Config, g.Density, g.Repeats)
		}
		if g.SpreadPercent() != 0 {
			t.Errorf("%s @ %d: spread = %v, want 0 for a single repeat", g.Config, g.Density, g.SpreadPercent())
		}
	}
}

// The 2026-08-01 dataset's density-10 cells reported p99 == max across all six configs, which
// looked like a tail estimate and was a single worst observation. The summary must say so.
func TestSummarizeWarnsOnSampleStarvation(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl", ThreadsPerCore: 2},
		Cells: []CellResult{
			// 50 samples: nearest-rank p99 is rank 50 of 50, the maximum.
			{Config: placement.ConfigB, Density: 10, Repeat: 0, Responded: 10, SampleCount: 50,
				MeanLatencyUS: 7709, P99LatencyUS: 7729, MaxLatencyUS: 7729},
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7736, P99LatencyUS: 7840},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "too few samples") {
		t.Error("summary did not warn that a cell's p99 is not a meaningful percentile")
	}
	// The warning must come before the table, like the other caveats.
	if idx, table := strings.Index(out, "too few samples"), strings.Index(out, "p99 us"); table >= 0 && idx > table {
		t.Error("sample-starvation warning appears after the results table; it must come first")
	}
	// Only the starved cell should be named, not the adequately sampled one.
	if !strings.Contains(out, "B @ 10") {
		t.Error("warning did not identify which cell was starved")
	}
}

func TestSummarizeQuietWhenSamplesAdequate(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 10, Responded: 10, SampleCount: 450,
				MeanLatencyUS: 7709, P99LatencyUS: 7729},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "too few samples") {
		t.Error("warned about sample starvation when 450 samples were collected")
	}
}

// A single-repeat run must say up front that no difference it shows can be separated from
// noise. That is the caveat the 2026-08-01 dataset needed and did not carry.
func TestSummarizeWarnsOnSingleRepeat(t *testing.T) {
	r := &Results{
		Repeats: 1,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7736, P99LatencyUS: 7840},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "one repeat") {
		t.Error("summary did not warn that a single repeat carries no variance information")
	}
	// With one repeat there is nothing to compare, so the variance table must be absent
	// rather than showing a misleading zero spread.
	if strings.Contains(out, "ACROSS-REPEAT VARIANCE") {
		t.Error("printed a variance table for a single repeat, where spread is unmeasured rather than zero")
	}
}

func TestSummarizeReportsVarianceForRepeats(t *testing.T) {
	r := &Results{
		Repeats: 3,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7700, P99LatencyUS: 7800, JitterUS: 150},
			{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7800, P99LatencyUS: 8000, JitterUS: 200},
			{Config: placement.ConfigB, Density: 90, Repeat: 2, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7900, P99LatencyUS: 7900, JitterUS: 175},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "ACROSS-REPEAT VARIANCE") {
		t.Fatal("summary omitted the variance section despite multiple repeats")
	}
	// The largest spread is the noise floor a finding must clear, so it must be stated
	// rather than left for a reader to compute from the table.
	if !strings.Contains(out, "Largest p99 spread") {
		t.Error("summary did not state the noise floor findings must exceed")
	}
	if !strings.Contains(out, "noise rather than as findings") {
		t.Error("summary did not tell the reader how to use the spread")
	}
}

// A delta smaller than the observed run-to-run spread is not a finding. Reporting it without
// that qualification is how a single-sample artifact becomes a blog claim.
func TestCompareQualifiesOverlappingRanges(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Cells: []CellResult{
			// B and E ranges overlap: B's slowest repeat is slower than E's fastest.
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7700, P99LatencyUS: 7800},
			{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7800, P99LatencyUS: 8600},
			{Config: placement.ConfigE, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7790, P99LatencyUS: 8400},
			{Config: placement.ConfigE, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7800, P99LatencyUS: 8500},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "Ranges overlap") {
		t.Error("summary reported a B-versus-E delta without noting that the ranges overlap")
	}
}

func TestCompareConfirmsSeparatedRanges(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Cells: []CellResult{
			// Every E repeat is slower than every B repeat, so the direction holds.
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7700, P99LatencyUS: 7800},
			{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7710, P99LatencyUS: 7850},
			{Config: placement.ConfigE, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7790, P99LatencyUS: 8400},
			{Config: placement.ConfigE, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7800, P99LatencyUS: 8500},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "do not overlap") {
		t.Error("summary did not confirm the direction holds when ranges are separated")
	}
	if strings.Contains(out, "Ranges overlap") {
		t.Error("summary claimed overlap for clearly separated ranges")
	}
}

// Without a repeat column, samples from separate repeats are indistinguishable in the raw file,
// making across-repeat variance uncomputable from the very data retained to compute it.
func TestSamplesCSVCarriesRepeat(t *testing.T) {
	r := &Results{}
	r.AddSamples([]Sample{
		{Config: placement.ConfigB, Density: 90, Repeat: 0, VMIndex: 0, Round: 0,
			DurationUS: 7736.0, Checksum: 1, CPUSet: "2"},
		{Config: placement.ConfigB, Density: 90, Repeat: 2, VMIndex: 0, Round: 0,
			DurationUS: 7900.0, Checksum: 1, CPUSet: "2"},
	})
	var sb strings.Builder
	if err := r.WriteSamplesCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if !strings.Contains(lines[0], "repeat") {
		t.Fatalf("samples header %q missing a repeat column", lines[0])
	}
	// Same config, density, vm, and round: only the repeat distinguishes these rows.
	if !strings.Contains(lines[1], "B,90,0,") || !strings.Contains(lines[2], "B,90,2,") {
		t.Errorf("repeat index not recorded per row:\n%s\n%s", lines[1], lines[2])
	}
}

func TestWriteVarianceCSV(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7700, P99LatencyUS: 7800, JitterUS: 150},
			{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
				MeanLatencyUS: 7800, P99LatencyUS: 8000, JitterUS: 200},
		},
	}
	var sb strings.Builder
	if err := r.WriteVarianceCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != 2 { // header plus one group
		t.Fatalf("variance CSV has %d lines, want 2", len(lines))
	}
	for _, want := range []string{"repeats", "median_p99_us", "p99_spread_pct", "all_trustworthy"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q", want)
		}
	}
	if !strings.HasPrefix(lines[1], "B,90,2,") {
		t.Errorf("row = %q, want it to start with config, density, and repeat count", lines[1])
	}
}

func TestCellCSVCarriesRepeatAndRounds(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 10, Repeat: 1, RoundsRun: 45,
				Responded: 10, SampleCount: 450},
		},
	}
	var sb strings.Builder
	if err := r.WriteCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	for _, want := range []string{"repeat", "rounds_run"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q", want)
		}
	}
	// Rounds actually run can exceed the configured floor, so the effective value must travel
	// with the cell rather than only the requested one.
	if !strings.Contains(lines[1], "B,10,1,45,") {
		t.Errorf("row = %q, want repeat 1 and 45 rounds recorded", lines[1])
	}
}

func TestNearestRankMatchesPercentileImplementation(t *testing.T) {
	// The summary claims which observation a p99 is, so its rank arithmetic must match what
	// measure.LatencySamples.Percentile actually does.
	cases := []struct {
		n, want int
	}{
		{50, 50},   // ceil(49.5) = 50, the maximum: the 2026-08-01 failure mode
		{100, 99},  // first count where p99 is not the maximum
		{450, 446}, // the target
		{900, 891},
		{1, 1},
		{0, 0},
	}
	for _, c := range cases {
		if got := nearestRank(c.n, 99); got != c.want {
			t.Errorf("nearestRank(%d, 99) = %d, want %d", c.n, got, c.want)
		}
	}
	// Cross-check against the real implementation on a set whose values encode their rank.
	var l measure.LatencySamples
	for i := 1; i <= 450; i++ {
		l.Add(time.Duration(i) * time.Microsecond)
	}
	wantRank := nearestRank(450, 99)
	if got := l.Percentile(99); got != time.Duration(wantRank)*time.Microsecond {
		t.Errorf("Percentile(99) = %v, but nearestRank says rank %d", got, wantRank)
	}
}

func TestSummarizeQuietWhenSMTMatches(t *testing.T) {
	r := &Results{
		Host: HostInfo{
			InstanceType: "m8i.metal-48xl", PhysicalCores: 96, ThreadsPerCore: 1,
			RequestedThreadsPerCore: 1, RunLabel: "smt-off",
			NUMANodes: 3, L3Domains: 1, CacheMode: "unified-across-nodes",
		},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "SMT MISMATCH") {
		t.Error("warned about a mismatch when requested and observed agree")
	}
}

// The two workloads must be requested with distinct verbs, or a cache run would silently
// execute the compute loop.
func TestWorkloadRequestSelectsVerb(t *testing.T) {
	compute := WorkloadSpec{Iterations: 5_000_000}
	if got, want := compute.Request(), "RUN 5000000"; got != want {
		t.Errorf("compute request = %q, want %q", got, want)
	}
	cache := WorkloadSpec{Iterations: 2_000_000, WorkingSetKiB: 4096}
	if got, want := cache.Request(), "RUN_CACHE 4096 2000000"; got != want {
		t.Errorf("cache request = %q, want %q", got, want)
	}
}

// OKC carries the working set in field 1 and iterations in field 2, whereas OK has iterations
// in field 1. Reading one as the other would record a buffer size as an iteration count.
func TestParseRunReplyDistinguishesWorkloads(t *testing.T) {
	rr, ok := parseRunReply("OKC 4096 2000000 91234 45 1200 987654321")
	if !ok {
		t.Fatal("failed to parse a valid OKC reply")
	}
	if rr.Kind != "cache" {
		t.Errorf("Kind = %q for an OKC reply, want cache", rr.Kind)
	}
	if rr.WorkingSetKiB != 4096 {
		t.Errorf("WorkingSetKiB = %d, want 4096", rr.WorkingSetKiB)
	}
	if rr.Iterations != 2_000_000 {
		t.Errorf("Iterations = %d, want 2000000 (field 2, not field 1)", rr.Iterations)
	}
	// TotalUS must be the chase, not the setup: setup is excluded from the measurement.
	if rr.TotalUS != 91234 {
		t.Errorf("TotalUS = %d, want the chase duration 91234", rr.TotalUS)
	}
	if rr.SetupUS != 1200 {
		t.Errorf("SetupUS = %d, want 1200", rr.SetupUS)
	}
	if rr.MeanNS != 45 {
		t.Errorf("MeanNS = %d, want 45", rr.MeanNS)
	}

	// A compute reply must report a different kind, or the mismatch guard would never fire.
	c, ok := parseRunReply("OK 5000000 7739 1 11372319519890737312")
	if !ok || c.Kind != "compute" {
		t.Errorf("an OK reply must parse as compute, got %q ok=%t", c.Kind, ok)
	}
	if c.WorkingSetKiB != 0 || c.SetupUS != 0 {
		t.Error("an OK reply must leave the cache-only fields zero")
	}
}

// An OKE reply must be read field-for-field, and its turn/verify split must survive the parse.
//
// The split is the whole reason the episode workload exists: every earlier figure in this project
// timed a verifier run and reported it as an episode. A parser that read the fields in the wrong
// order would report a turn count as a latency, or fold verify time into turn time, and the
// resulting number would look entirely plausible.
func TestParseRunReplyHandlesEpisode(t *testing.T) {
	// OKE <turns> <turns_ok> <total_us> <turns_us> <verify_us> <exit_code> <peak_rss_kib>
	rr, ok := parseRunReply("OKE 14 13 3500000 2000000 1500000 0 185000")
	if !ok {
		t.Fatal("failed to parse a valid OKE reply")
	}
	if rr.Kind != "episode" {
		t.Errorf("Kind = %q, want episode", rr.Kind)
	}
	if rr.Turns != 14 || rr.TurnsOK != 13 {
		t.Errorf("Turns/TurnsOK = %d/%d, want 14/13", rr.Turns, rr.TurnsOK)
	}
	if rr.TotalUS != 3_500_000 {
		t.Errorf("TotalUS = %d, want 3500000 (field 3)", rr.TotalUS)
	}
	if rr.TurnsUS != 2_000_000 {
		t.Errorf("TurnsUS = %d, want 2000000", rr.TurnsUS)
	}
	if rr.VerifyUS != 1_500_000 {
		t.Errorf("VerifyUS = %d, want 1500000", rr.VerifyUS)
	}
	// The two components must be distinct values, not one figure duplicated: that is what a
	// collapsed split would look like.
	if rr.TurnsUS == rr.VerifyUS {
		t.Error("TurnsUS and VerifyUS are equal; the split has collapsed")
	}
	if rr.TurnsUS+rr.VerifyUS > rr.TotalUS {
		t.Errorf("turns %d + verify %d exceeds total %d", rr.TurnsUS, rr.VerifyUS, rr.TotalUS)
	}
	if rr.PeakRSSKiB != 185_000 {
		t.Errorf("PeakRSSKiB = %d, want 185000", rr.PeakRSSKiB)
	}
	if !rr.HasExitCode {
		t.Error("HasExitCode must be set: exit 0 is the reward and must be distinguishable from unset")
	}

	// A non-zero exit is a legitimate result -- a rollout that failed its instance -- so it must
	// parse rather than being rejected as malformed.
	failed, ok := parseRunReply("OKE 14 11 3500000 2000000 1500000 1 185000")
	if !ok || failed.ExitCode != 1 {
		t.Errorf("a failed episode must parse with its exit code, got ok=%t exit=%d", ok, failed.ExitCode)
	}

	// OKE and OKT both carry an exit code but have completely different layouts, so the tags must
	// not be interchangeable.
	task, ok := parseRunReply("OKT 1 199066 5624 87000")
	if !ok || task.Kind != "task" {
		t.Errorf("OKT must still parse as task, got %q ok=%t", task.Kind, ok)
	}
	if task.Turns != 0 || task.TurnsUS != 0 {
		t.Error("an OKT reply must leave the episode-only fields zero")
	}
}

func TestParseRunReplyRejectsTruncatedOKE(t *testing.T) {
	// A short reply must be refused rather than parsed with missing fields defaulted to zero: a
	// zero verify time would read as an episode whose verifier was free.
	for _, bad := range []string{
		"OKE",
		"OKE 14",
		"OKE 14 13 3500000 2000000 1500000 0", // one field short
		"OKE x 13 3500000 2000000 1500000 0 185000",
		"OKE 14 13 3500000 2000000 x 0 185000",
	} {
		if _, ok := parseRunReply(bad); ok {
			t.Errorf("parseRunReply(%q) accepted a malformed OKE reply", bad)
		}
	}
}

func TestParseRunReplyRejectsTruncatedOKC(t *testing.T) {
	for _, bad := range []string{
		"OKC 4096 2000000 91234 45 1200", // checksum missing
		"OKC 4096 2000000 91234",         // badly truncated
		"OKC",                            // bare
		"OKC a 2000000 91234 45 1200 99", // non-numeric working set
		"OKC 4096 2000000 91234 45 x 99", // non-numeric setup
	} {
		if _, ok := parseRunReply(bad); ok {
			t.Errorf("parseRunReply(%q) accepted an invalid OKC reply", bad)
		}
	}
}

// A dataset must record its working set per sample, so a working-set sweep is reanalysable
// from the raw file alone.
func TestSamplesCSVCarriesWorkingSet(t *testing.T) {
	r := &Results{}
	r.AddSamples([]Sample{
		{Config: placement.ConfigB, Density: 90, VMIndex: 0, DurationUS: 91234,
			WorkingSetKiB: 4096, SetupUS: 1200, CPUSet: "2"},
	})
	var sb strings.Builder
	if err := r.WriteSamplesCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	for _, want := range []string{"working_set_kib", "setup_us"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q", want)
		}
	}
	// cpuset must stay last so adding a column never shifts it.
	if !strings.HasSuffix(lines[0], "cpuset") || !strings.HasSuffix(lines[1], ",2") {
		t.Errorf("cpuset is not the final column:\n  %s\n  %s", lines[0], lines[1])
	}
	if !strings.Contains(lines[1], ",4096,1200.0,") {
		t.Errorf("row = %q, want working set and setup recorded", lines[1])
	}
}

// A cache-insensitive workload cannot evaluate cache-affinity placement, so the summary has to
// say so. Otherwise a null result reads as evidence of no effect.
func TestSummarizeStatesWorkloadCacheSensitivity(t *testing.T) {
	base := HostInfo{InstanceType: "m8a.metal-48xl"}
	cells := []CellResult{{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450}}

	compute := &Results{Repeats: 5, Host: base, Workload: WorkloadSpec{Iterations: 5_000_000}, Cells: cells}
	var sb strings.Builder
	if err := compute.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "cache-insensitive") {
		t.Error("summary did not warn that the compute loop cannot evaluate cache affinity")
	}
	if !strings.Contains(sb.String(), "arithmetic compute loop") {
		t.Error("summary did not name the workload")
	}

	cache := &Results{Repeats: 5, Host: base,
		Workload: WorkloadSpec{Iterations: 2_000_000, WorkingSetKiB: 4096}, Cells: cells}
	var sb2 strings.Builder
	if err := cache.Summarize(&sb2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb2.String(), "cache-insensitive") {
		t.Error("warned about cache insensitivity for the pointer-chase workload")
	}
	if !strings.Contains(sb2.String(), "pointer chase over 4096 KiB") {
		t.Error("summary did not report the working set size")
	}
}

// Sandbox is checked first because it subsumes the others: a caller who set it does not want a
// pointer chase, so a stale WorkingSetKiB must not win.
func TestWorkloadRequestSelectsSandbox(t *testing.T) {
	w := WorkloadSpec{
		Iterations:    5_000_000,
		WorkingSetKiB: 4096, // must be ignored once Sandbox is set
		Sandbox:       &SandboxSpec{ResidentKiB: 2_097_152, Steps: 20, WriteKiB: 256},
	}
	if got, want := w.Request(), "RUN_SANDBOX 2097152 20 256"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	if got := w.Kind(); got != "sandbox" {
		t.Errorf("Kind() = %q, want sandbox", got)
	}
}

func TestWorkloadKindIsThreeWay(t *testing.T) {
	cases := []struct {
		name string
		spec WorkloadSpec
		want string
	}{
		{"compute", WorkloadSpec{Iterations: 1000}, "compute"},
		{"cache", WorkloadSpec{WorkingSetKiB: 1024}, "cache"},
		{"sandbox", WorkloadSpec{Sandbox: &SandboxSpec{ResidentKiB: 1024}}, "sandbox"},
	}
	for _, c := range cases {
		if got := c.spec.Kind(); got != c.want {
			t.Errorf("%s: Kind() = %q, want %q", c.name, got, c.want)
		}
	}
}

// OKS field positions differ from OK and OKC, so reading one as another would record a
// resident-set size as an iteration count.
func TestParseRunReplyHandlesSandbox(t *testing.T) {
	// OKS <resident_kib> <steps> <write_kib> <total_us> <spawn_us> <write_us> <spawns_ok> <checksum>
	rr, ok := parseRunReply("OKS 2097152 20 256 918234 4210 31005 20 987654321 2145728")
	if !ok {
		t.Fatal("failed to parse a valid OKS reply")
	}
	if rr.Kind != "sandbox" {
		t.Errorf("Kind = %q, want sandbox", rr.Kind)
	}
	if rr.ResidentKiB != 2_097_152 {
		t.Errorf("ResidentKiB = %d, want 2097152", rr.ResidentKiB)
	}
	// TotalUS must be the episode total, not the spawn or write component.
	if rr.TotalUS != 918234 {
		t.Errorf("TotalUS = %d, want 918234", rr.TotalUS)
	}
	if rr.SpawnUS != 4210 || rr.WriteUS != 31005 {
		t.Errorf("spawn/write = %d/%d, want 4210/31005", rr.SpawnUS, rr.WriteUS)
	}
	if rr.SpawnsOK != 20 {
		t.Errorf("SpawnsOK = %d, want 20", rr.SpawnsOK)
	}
	if rr.Checksum != 987654321 {
		t.Errorf("Checksum = %d, want 987654321", rr.Checksum)
	}
	// Spawn and write are components of the total, so together they cannot exceed it.
	if rr.SpawnUS+rr.WriteUS > rr.TotalUS {
		t.Error("spawn + write exceeds total, so they are not components of it")
	}
}

func TestParseRunReplyRejectsTruncatedOKS(t *testing.T) {
	for _, bad := range []string{
		"OKS 2097152 20 256 918234 4210 31005 20 987654321", // peak_rss missing: what a
		// pre-2026-09-18 agent answers. Must fail rather than report a zero peak.
		"OKS 2097152 20 256 918234 4210 31005 20", // checksum missing
		"OKS 2097152 20 256",                      // badly truncated
		"OKS",                                     // bare
		"OKS x 20 256 918234 4210 31005 20 99 4096",   // non-numeric resident
		"OKS 2097152 20 256 918234 4210 x 20 99 4096", // non-numeric write time
	} {
		if _, ok := parseRunReply(bad); ok {
			t.Errorf("parseRunReply(%q) accepted an invalid OKS reply", bad)
		}
	}
}

// Each reply tag must map to exactly one kind, which is what lets the runner's mismatch guard
// compare what was asked against what came back.
func TestReplyTagsAreDistinguishable(t *testing.T) {
	compute, ok := parseRunReply("OK 5000000 7739 1 123")
	if !ok || compute.Kind != "compute" {
		t.Errorf("OK should parse as compute, got %q ok=%t", compute.Kind, ok)
	}
	cache, ok := parseRunReply("OKC 4096 2000000 91234 45 1200 123")
	if !ok || cache.Kind != "cache" {
		t.Errorf("OKC should parse as cache, got %q ok=%t", cache.Kind, ok)
	}
	sandbox, ok := parseRunReply("OKS 1024 5 64 5000 100 200 5 123 40960")
	if !ok || sandbox.Kind != "sandbox" {
		t.Errorf("OKS should parse as sandbox, got %q ok=%t", sandbox.Kind, ok)
	}
	// A compute reply must leave the sandbox fields zero, or the guard could not tell them apart.
	if compute.ResidentKiB != 0 || compute.SpawnsOK != 0 {
		t.Error("an OK reply must leave sandbox-only fields zero")
	}
}

// An undersized guest must be refused before hardware time is spent. An OOM kill mid-episode
// presents as a microVM that stopped answering, which at density looks like a spurious density
// ceiling rather than the configuration error it is.
func TestValidateRejectsUndersizedGuestForSandbox(t *testing.T) {
	base := DefaultConfig()
	base.FirecrackerBin = "/bin/sh"
	base.JailerBin = "/bin/sh"
	base.KernelPath = "/bin/sh"
	base.RootfsPath = "/bin/sh"
	base.Configs = []placement.Config{placement.ConfigB}
	base.Densities = []int{10}

	tooSmall := base
	tooSmall.Workload.Sandbox = &SandboxSpec{ResidentKiB: 2 * 1024 * 1024, Steps: 10, WriteKiB: 256}
	tooSmall.Workload.MemSizeMiB = 128
	err := tooSmall.Validate()
	if err == nil {
		t.Fatal("Validate accepted a 2 GiB resident set in a 128 MiB guest")
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("error %q should say the guest is too small", err)
	}

	okCfg := base
	okCfg.Workload.Sandbox = &SandboxSpec{ResidentKiB: 2 * 1024 * 1024, Steps: 10, WriteKiB: 256}
	okCfg.Workload.MemSizeMiB = okCfg.Workload.Sandbox.GuestMemoryFloorMiB()
	if err := okCfg.Validate(); err != nil {
		t.Errorf("Validate rejected a guest exactly at the floor: %v", err)
	}

	negative := base
	negative.Workload.Sandbox = &SandboxSpec{ResidentKiB: -1}
	negative.Workload.MemSizeMiB = 4096
	if err := negative.Validate(); err == nil {
		t.Error("Validate accepted a negative resident set")
	}
}

// The floor must account for writes as well as the resident set, because tmpfs pages are guest
// memory. Counting only the resident set would let a write-heavy episode OOM.
func TestGuestMemoryFloorIncludesWrites(t *testing.T) {
	quiet := SandboxSpec{ResidentKiB: 1024 * 1024, WriteKiB: 0}
	busy := SandboxSpec{ResidentKiB: 1024 * 1024, WriteKiB: 512 * 1024}
	if busy.GuestMemoryFloorMiB() <= quiet.GuestMemoryFloorMiB() {
		t.Errorf("floor ignores writes: quiet %d MiB, busy %d MiB",
			quiet.GuestMemoryFloorMiB(), busy.GuestMemoryFloorMiB())
	}
	// Headroom for the guest kernel and agent must be included, or a guest sized exactly at the
	// resident set would have nothing left to boot with.
	bare := SandboxSpec{ResidentKiB: 0, WriteKiB: 0}
	if bare.GuestMemoryFloorMiB() <= 0 {
		t.Error("floor must include overhead for the kernel and agent even at zero resident")
	}
}

// Overhead must scale with the resident set, not stay flat.
//
// Regression test for a real failure: a flat 64 MiB overhead let a 2 GiB resident set in a
// 2176 MiB guest pass validation on 2026-08-02, and every microVM then died with
// "read reply: EOF" because the guest kernel OOM-killed the agent. The host had 768 GiB free,
// so the kill was inside the guest. Guest page tables grow with the memory they map, so a
// constant cannot cover both a 128 MiB and a 6 GiB guest.
func TestGuestMemoryFloorScalesWithResidentSet(t *testing.T) {
	small := SandboxSpec{ResidentKiB: 128 * 1024}
	large := SandboxSpec{ResidentKiB: 4096 * 1024}

	smallSlack := small.GuestMemoryFloorMiB() - small.ResidentKiB/1024
	largeSlack := large.GuestMemoryFloorMiB() - large.ResidentKiB/1024
	if largeSlack <= smallSlack {
		t.Errorf("overhead is flat: 128 MiB guest gets %d MiB slack, 4 GiB guest gets %d MiB. "+
			"Guest page tables scale with mapped memory, so a constant will under-provision "+
			"large guests and they will OOM mid-episode",
			smallSlack, largeSlack)
	}

	// The specific configuration that failed on hardware must now be refused.
	failed := SandboxSpec{ResidentKiB: 2 * 1024 * 1024, Steps: 20, WriteKiB: 256}
	if floor := failed.GuestMemoryFloorMiB(); floor <= 2176 {
		t.Errorf("floor for a 2 GiB resident set is %d MiB, but 2176 MiB was measured to OOM; "+
			"the floor must exceed what is known to fail", floor)
	}
}

// The summary must name the sandbox workload, and must not claim cache-insensitivity for it:
// unlike the compute loop, this workload does touch memory.
func TestSummarizeDescribesSandboxWorkload(t *testing.T) {
	r := &Results{
		Repeats: 5,
		Host:    HostInfo{InstanceType: "m8a.metal-48xl"},
		Workload: WorkloadSpec{
			Rounds:     5,
			VCPUCount:  1,
			MemSizeMiB: 2304,
			Sandbox:    &SandboxSpec{ResidentKiB: 2 * 1024 * 1024, Steps: 20, WriteKiB: 256},
		},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "agentic sandbox") {
		t.Error("summary did not name the sandbox workload")
	}
	if !strings.Contains(out, "2097152 KiB resident") {
		t.Error("summary did not report the resident set size")
	}
	if strings.Contains(out, "cache-insensitive") {
		t.Error("summary claimed cache-insensitivity for a workload that touches memory")
	}
}

// Sandbox timings must survive the CSV round trip, or a density effect could not be attributed
// to fork/exec or writeback rather than only observed in the total.
func TestSamplesCSVCarriesSandboxFields(t *testing.T) {
	r := &Results{}
	r.AddSamples([]Sample{
		{Config: placement.ConfigB, Density: 90, DurationUS: 918234,
			SpawnUS: 4210, WriteUS: 31005, SpawnsOK: 20, CPUSet: "7"},
	})
	var sb strings.Builder
	if err := r.WriteSamplesCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	for _, want := range []string{"spawn_us", "write_us", "spawns_ok"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q", want)
		}
	}
	// cpuset must remain the final column so adding fields never shifts it.
	if !strings.HasSuffix(lines[0], "cpuset") || !strings.HasSuffix(lines[1], ",7") {
		t.Errorf("cpuset is not last:\n  %s\n  %s", lines[0], lines[1])
	}
	if !strings.Contains(lines[1], "4210.0,31005.0,20,") {
		t.Errorf("row = %q, want spawn, write, and spawns_ok recorded", lines[1])
	}
}

// varianceCells builds three repeats of one cell with distinct p50/p99/p100 values, so a test can
// tell which percentile a field was populated from. Deliberately not monotonic across repeats:
// median must be picked per percentile, not by ordering whole repeats.
func varianceCells() []CellResult {
	return []CellResult{
		{Config: placement.ConfigB, Density: 90, Repeat: 0, Responded: 90, SampleCount: 450,
			MeanLatencyUS: 1000, P50LatencyUS: 900, P99LatencyUS: 2000, MaxLatencyUS: 5000, JitterUS: 150},
		{Config: placement.ConfigB, Density: 90, Repeat: 1, Responded: 90, SampleCount: 450,
			MeanLatencyUS: 1100, P50LatencyUS: 1000, P99LatencyUS: 2200, MaxLatencyUS: 9000, JitterUS: 200},
		{Config: placement.ConfigB, Density: 90, Repeat: 2, Responded: 90, SampleCount: 450,
			MeanLatencyUS: 1050, P50LatencyUS: 950, P99LatencyUS: 2100, MaxLatencyUS: 7000, JitterUS: 175},
	}
}

// All three percentiles must reach CellVariance. Only mean and p99 used to, so p50 and p100 were
// dropped from the file every published comparison reads, and the tail-versus-median distinction
// had to be recomputed from raw samples each time.
func TestVarianceCarriesP50AndP100(t *testing.T) {
	r := &Results{Repeats: 3, Cells: varianceCells()}
	v := r.Variance()
	if len(v) != 1 {
		t.Fatalf("Variance() returned %d groups, want 1", len(v))
	}
	g := v[0]

	// Medians of 900/950/1000, 2000/2100/2200, 5000/7000/9000.
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"MedianP50US", g.MedianP50US, 950},
		{"MinP50US", g.MinP50US, 900},
		{"MaxP50US", g.MaxP50US, 1000},
		{"MedianP99US", g.MedianP99US, 2100},
		{"MedianP100US", g.MedianP100US, 7000},
		{"MinP100US", g.MinP100US, 5000},
		{"MaxP100US", g.MaxP100US, 9000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// p100 must come from MaxLatencyUS, not from p99 or jitter. Distinct magnitudes above make a
	// wrong source detectable rather than plausible.
	if g.MedianP100US == g.MedianP99US {
		t.Error("p100 equals p99; it is being populated from the wrong field")
	}
}

// TailRatio bounds max-of-G amplification, so it must be p99/p50 and not any other pairing.
// It has been computed by hand in every analysis so far; a wrong definition here would be
// copied into a published claim.
func TestTailAndWorstRatios(t *testing.T) {
	r := &Results{Repeats: 3, Cells: varianceCells()}
	g := r.Variance()[0]

	// 2100/950 and 7000/950.
	if diff := g.TailRatio() - 2100.0/950.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("TailRatio() = %v, want p99/p50 = %v", g.TailRatio(), 2100.0/950.0)
	}
	if diff := g.WorstRatio() - 7000.0/950.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("WorstRatio() = %v, want p100/p50 = %v", g.WorstRatio(), 7000.0/950.0)
	}
	// The worst task cannot be faster than the tail, so this ordering must always hold.
	if g.WorstRatio() < g.TailRatio() {
		t.Errorf("WorstRatio %v below TailRatio %v; p100 cannot be under p99",
			g.WorstRatio(), g.TailRatio())
	}
}

// A cell that measured nothing must not divide by zero, and must report 0 rather than NaN or Inf,
// because a NaN in a CSV silently poisons every downstream aggregation.
func TestRatiosSafeWithNoSamples(t *testing.T) {
	r := &Results{Repeats: 1, Cells: []CellResult{
		{Config: placement.ConfigB, Density: 90, Responded: 0, SampleCount: 0},
	}}
	g := r.Variance()[0]
	if g.TailRatio() != 0 || g.WorstRatio() != 0 {
		t.Errorf("ratios on an empty cell = %v/%v, want 0/0",
			g.TailRatio(), g.WorstRatio())
	}
}

func TestVarianceCSVCarriesAllThreePercentiles(t *testing.T) {
	r := &Results{Repeats: 3, Cells: varianceCells()}
	var sb strings.Builder
	if err := r.WriteVarianceCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("variance CSV has %d lines, want 2", len(lines))
	}
	for _, want := range []string{
		"median_p50_us", "min_p50_us", "max_p50_us",
		"median_p99_us", "median_p100_us", "min_p100_us", "max_p100_us",
		"tail_ratio", "worst_ratio",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q", want)
		}
	}
	// Header and row must have equal field counts, or every column after the mismatch is
	// mislabelled for anyone parsing positionally.
	if h, d := len(strings.Split(lines[0], ",")), len(strings.Split(lines[1], ",")); h != d {
		t.Errorf("header has %d fields, data row has %d", h, d)
	}
	if !strings.Contains(lines[1], "950.0,900.0,1000.0") {
		t.Errorf("row = %q, want the p50 triple", lines[1])
	}
	if !strings.Contains(lines[1], "7000.0,5000.0,9000.0") {
		t.Errorf("row = %q, want the p100 triple", lines[1])
	}
}

// The summary table is what a reader sees on the terminal at the end of a paid session, so all
// three percentiles and both ratios must be there rather than only p99.
func TestSummarizeShowsAllThreePercentiles(t *testing.T) {
	r := &Results{
		Repeats: 3,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Cells:   varianceCells(),
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"med p50", "med p99", "med p100", "p99/p50", "p100/50"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary table missing column %q", want)
		}
	}
}

// Task overrides every other workload, so a stale -resident-kib or -working-set-kib must not win.
func TestWorkloadRequestSelectsTask(t *testing.T) {
	w := WorkloadSpec{
		Iterations:    5_000_000,
		WorkingSetKiB: 4096,
		Sandbox:       &SandboxSpec{ResidentKiB: 1024},
		Task:          &TaskSpec{InstanceID: "django__django-11099"},
	}
	if got, want := w.Request(), "RUN_TASK"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	if got := w.Kind(); got != "task" {
		t.Errorf("Kind() = %q, want task", got)
	}
}

// OKT field positions differ from OK/OKC/OKS, so reading one as another would record a peak RSS
// as an iteration count.
func TestParseRunReplyHandlesTask(t *testing.T) {
	// OKT <exit_code> <total_us> <setup_us> <peak_rss_kib>
	rr, ok := parseRunReply("OKT 0 4820000 1200 1834200")
	if !ok {
		t.Fatal("failed to parse a valid OKT reply")
	}
	if rr.Kind != "task" {
		t.Errorf("Kind = %q, want task", rr.Kind)
	}
	if !rr.HasExitCode {
		t.Error("HasExitCode = false; a zero exit would be indistinguishable from no task result")
	}
	if rr.ExitCode != 0 || rr.TotalUS != 4820000 || rr.SetupUS != 1200 || rr.PeakRSSKiB != 1834200 {
		t.Errorf("fields = exit %d, total %d, setup %d, rss %d",
			rr.ExitCode, rr.TotalUS, rr.SetupUS, rr.PeakRSSKiB)
	}

	// A non-zero exit must parse rather than being rejected: a failing task is data.
	fail, ok := parseRunReply("OKT 1 900000 1100 512000")
	if !ok || fail.ExitCode != 1 {
		t.Errorf("non-zero exit did not parse: ok=%t code=%d", ok, fail.ExitCode)
	}
	// 128+SIGKILL, which is how a guest-OOM-killed test suite reports.
	sig, ok := parseRunReply("OKT 137 900000 1100 512000")
	if !ok || sig.ExitCode != 137 {
		t.Errorf("signal exit did not parse: ok=%t code=%d", ok, sig.ExitCode)
	}
}

// HasExitCode is what separates "task passed" from "not a task workload". Without it every
// synthetic cell would look like a passing task and the pass-rate check would never fire.
func TestOnlyTaskRepliesCarryAnExitCode(t *testing.T) {
	for _, reply := range []string{
		"OK 5000000 7739 1 123",
		"OKC 4096 2000000 91234 45 1200 123",
		"OKS 1024 5 64 5000 100 200 5 123 40960",
	} {
		rr, ok := parseRunReply(reply)
		if !ok {
			t.Fatalf("%q did not parse", reply)
		}
		if rr.HasExitCode {
			t.Errorf("%q reports HasExitCode; only OKT should", reply)
		}
	}
}

func TestParseRunReplyRejectsTruncatedOKT(t *testing.T) {
	for _, bad := range []string{
		"OKT 0 4820000 1200", // peak RSS missing
		"OKT 0",
		"OKT",
		"OKT x 4820000 1200 1834200", // non-numeric exit code
		"OKT 0 4820000 1200 y",       // non-numeric RSS
	} {
		if _, ok := parseRunReply(bad); ok {
			t.Errorf("parseRunReply(%q) accepted an invalid OKT reply", bad)
		}
	}
}

// **A task that crashes early looks fast.** A cell whose tasks failed must not be quoted as a
// latency result, which is the same trap as the undersized guest whose OOM read as a density
// ceiling.
func TestTrustworthyRequiresTasksToPass(t *testing.T) {
	cases := []struct {
		name string
		cell CellResult
		want bool
	}{
		{
			"synthetic workload is unaffected",
			CellResult{Density: 90, Responded: 90, SampleCount: 450},
			true,
		},
		{
			"all tasks passed",
			CellResult{Density: 90, Responded: 90, SampleCount: 450, TaskRuns: 90, TaskPassed: 90},
			true,
		},
		{
			"one task failed",
			CellResult{Density: 90, Responded: 90, SampleCount: 450, TaskRuns: 90, TaskPassed: 89},
			false,
		},
		{
			"every task failed, and the cell looks fast",
			CellResult{Density: 90, Responded: 90, SampleCount: 450, TaskRuns: 90, TaskPassed: 0},
			false,
		},
	}
	for _, c := range cases {
		if got := c.cell.Trustworthy(); got != c.want {
			t.Errorf("%s: Trustworthy() = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestPassRate(t *testing.T) {
	// No task runs means the question does not apply, so 1.0 rather than a divide by zero.
	empty := CellResult{}
	if empty.PassRate() != 1.0 {
		t.Errorf("PassRate() with no task runs = %v, want 1.0", empty.PassRate())
	}
	half := CellResult{TaskRuns: 100, TaskPassed: 50}
	if half.PassRate() != 0.5 {
		t.Errorf("PassRate() = %v, want 0.5", half.PassRate())
	}
}

// Validate must name a conflict rather than silently resolving it: someone who passed both
// probably expected the sandbox.
func TestValidateRejectsConflictingWorkloads(t *testing.T) {
	base := DefaultConfig()
	base.FirecrackerBin = "/bin/sh"
	base.JailerBin = "/bin/sh"
	base.KernelPath = "/bin/sh"
	base.RootfsPath = "/bin/sh"
	base.Configs = []placement.Config{placement.ConfigB}
	base.Densities = []int{10}

	both := base
	both.Workload.Task = &TaskSpec{InstanceID: "x"}
	both.Workload.Sandbox = &SandboxSpec{ResidentKiB: 1024}
	both.Workload.MemSizeMiB = 4096
	err := both.Validate()
	if err == nil {
		t.Fatal("Validate accepted both a task and a sandbox workload")
	}
	if !strings.Contains(err.Error(), "pick one") {
		t.Errorf("error %q should tell the caller to pick one", err)
	}

	noID := base
	noID.Workload.Task = &TaskSpec{}
	if err := noID.Validate(); err == nil {
		t.Error("Validate accepted a task with no instance ID")
	}

	taskOnly := base
	taskOnly.Workload.Task = &TaskSpec{InstanceID: "django__django-11099"}
	if err := taskOnly.Validate(); err != nil {
		t.Errorf("Validate rejected a valid task-only config: %v", err)
	}
}

func TestSamplesCSVCarriesTaskFields(t *testing.T) {
	r := &Results{}
	r.AddSamples([]Sample{
		{Config: placement.ConfigB, Density: 90, DurationUS: 4820000,
			ExitCode: 0, PeakRSSKiB: 1834200, InstanceID: "django__django-11099", CPUSet: "7"},
	})
	var sb strings.Builder
	if err := r.WriteSamplesCSV(&sb); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	for _, want := range []string{"exit_code", "peak_rss_kib", "instance_id"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q", want)
		}
	}
	// cpuset must stay last so adding columns never shifts it.
	if !strings.HasSuffix(lines[0], "cpuset") || !strings.HasSuffix(lines[1], ",7") {
		t.Errorf("cpuset is not last:\n  %s\n  %s", lines[0], lines[1])
	}
	if !strings.Contains(lines[1], "django__django-11099") {
		t.Errorf("row = %q, want the instance ID", lines[1])
	}
	if h, d := len(strings.Split(lines[0], ",")), len(strings.Split(lines[1], ",")); h != d {
		t.Errorf("header has %d fields, data row has %d", h, d)
	}
}

// The summary must name the task, and must not claim cache-insensitivity for it.
func TestSummarizeNamesTheTask(t *testing.T) {
	r := &Results{
		Repeats: 3,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Workload: WorkloadSpec{
			Rounds: 5, VCPUCount: 1, MemSizeMiB: 4096,
			Task: &TaskSpec{InstanceID: "django__django-11099"},
		},
		Cells: []CellResult{
			{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450,
				TaskRuns: 90, TaskPassed: 90},
		},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "real task django__django-11099") {
		t.Error("summary did not name the task")
	}
	if strings.Contains(out, "cache-insensitive") {
		t.Error("summary claimed cache-insensitivity for a real task")
	}
}

// A task workload must not be described as a sandbox. An if-chain here originally let Sandbox
// overwrite Task, so the summary named the wrong workload.
func TestSummarizeWorkloadPrecedence(t *testing.T) {
	r := &Results{
		Repeats: 2,
		Host:    HostInfo{InstanceType: "m8i.metal-48xl"},
		Workload: WorkloadSpec{
			Rounds: 5, VCPUCount: 1, MemSizeMiB: 4096,
			WorkingSetKiB: 4096,
			Sandbox:       &SandboxSpec{ResidentKiB: 1024},
			Task:          &TaskSpec{InstanceID: "sympy__sympy-20590"},
		},
		Cells: []CellResult{{Config: placement.ConfigB, Density: 90, Responded: 90, SampleCount: 450}},
	}
	var sb strings.Builder
	if err := r.Summarize(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "real task sympy__sympy-20590") {
		t.Error("task did not win precedence in the summary")
	}
	if strings.Contains(out, "agentic sandbox") || strings.Contains(out, "pointer chase") {
		t.Error("a lower-precedence workload overwrote the task description")
	}
}

// Both memory phases must land in separate fields and both must reach the CSV.
//
// The defect this guards: until 2026-09-18 there was one set of memory fields, filled after boot,
// so a workload that created its own footprint was published at its idle size. A regression that
// wrote the working figure over the boot one, or dropped either column, would restore that.
func TestCSVCarriesBothMemoryPhases(t *testing.T) {
	r := &Results{Cells: []CellResult{{
		Config: "A", Density: 2, Responded: 2,
		FleetRSSKiBBoot: 1000, FleetPSSKiBBoot: 900,
		PerVMPrivateDirtyKiBBoot: 100, SharingRatioBoot: 1.11,
		FleetRSSKiBWorking: 9000, FleetPSSKiBWorking: 8000,
		PerVMPrivateDirtyKiBWorking: 4000, SharingRatioWorking: 1.13,
	}}}

	var buf bytes.Buffer
	if err := r.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + 1 row, got %d lines", len(lines))
	}
	head, row := strings.Split(lines[0], ","), strings.Split(lines[1], ",")
	// A mismatch here silently shifts every column after the short one, which would attribute
	// one measurement's value to another's name.
	if len(head) != len(row) {
		t.Fatalf("header has %d columns but the row has %d", len(head), len(row))
	}

	idx := map[string]int{}
	for i, h := range head {
		idx[h] = i
	}
	for _, want := range []struct {
		col string
		val string
	}{
		{"fleet_rss_kib_boot", "1000"},
		{"fleet_pss_kib_boot", "900"},
		{"per_vm_private_dirty_kib_boot", "100"},
		{"fleet_rss_kib_working", "9000"},
		{"fleet_pss_kib_working", "8000"},
		{"per_vm_private_dirty_kib_working", "4000"},
	} {
		i, ok := idx[want.col]
		if !ok {
			t.Errorf("CSV has no %s column", want.col)
			continue
		}
		if row[i] != want.val {
			t.Errorf("%s = %q, want %q", want.col, row[i], want.val)
		}
	}
	// The bare names must be gone: leaving one would make it ambiguous which phase it held.
	for _, gone := range []string{"fleet_rss_kib", "fleet_pss_kib", "per_vm_private_dirty_kib", "sharing_ratio"} {
		if _, ok := idx[gone]; ok {
			t.Errorf("unqualified column %q is still emitted; it does not say which phase it is", gone)
		}
	}
}

// The summary must show both figures and say which one to size a fleet on, since quoting the boot
// figure as though it were the working set is the mistake that shipped.
func TestSummarizeShowsBothMemoryPhases(t *testing.T) {
	r := &Results{
		Host: HostInfo{InstanceType: "m8a.metal-48xl"},
		Cells: []CellResult{{
			Config: "A", Density: 2, Responded: 2, SampleCount: 200,
			TaskRuns: 2, TaskPassed: 2,
			FleetPSSKiBBoot: 102400, PerVMPrivateDirtyKiBBoot: 51200, SharingRatioBoot: 1.2,
			FleetPSSKiBWorking: 2097152, PerVMPrivateDirtyKiBWorking: 1048576, SharingRatioWorking: 1.3,
		}},
	}
	var buf bytes.Buffer
	if err := r.Summarize(&buf); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"PSS boot", "PSS work", "51200 KiB at boot", "1048576 KiB after the workload", "20.5x"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
}
