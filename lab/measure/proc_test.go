// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package measure

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func withProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSmapsRollup emits the field set and formatting the kernel uses, so the parser is
// tested against realistic input rather than a simplified shape.
func writeSmapsRollup(t *testing.T, root string, pid, rss, pss, sharedClean, privateDirty, swap int) {
	t.Helper()
	content := fmt.Sprintf(`55a0b2e00000-7ffd0a1f4000 ---p 00000000 00:00 0                          [rollup]
Rss:             %6d kB
Pss:             %6d kB
Pss_Dirty:        %5d kB
Shared_Clean:    %6d kB
Shared_Dirty:         0 kB
Private_Clean:      128 kB
Private_Dirty:   %6d kB
Referenced:      %6d kB
Anonymous:       %6d kB
LazyFree:             0 kB
AnonHugePages:        0 kB
Swap:            %6d kB
SwapPss:              0 kB
Locked:               0 kB
`, rss, pss, privateDirty, sharedClean, privateDirty, rss, privateDirty, swap)
	writeFile(t, filepath.Join(root, strconv.Itoa(pid), "smaps_rollup"), content)
}

func TestReadProcessMemory(t *testing.T) {
	root := withProcFixture(t)
	writeSmapsRollup(t, root, 1234, 8192, 3072, 5120, 2048, 0)

	m, err := ReadProcessMemory(1234)
	if err != nil {
		t.Fatalf("ReadProcessMemory: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"PID", m.PID, 1234},
		{"RSS", m.RSSKiB, 8192},
		{"PSS", m.PSSKiB, 3072},
		{"SharedClean", m.SharedCleanKiB, 5120},
		{"PrivateDirty", m.PrivateDirtyKiB, 2048},
		{"Swap", m.SwapKiB, 0},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// Pss_Dirty appears before Private_Dirty in real output and both end in "_Dirty". A parser
// matching loosely would read the wrong field, so confirm exact key matching.
func TestReadProcessMemoryDoesNotConfusePssDirty(t *testing.T) {
	root := withProcFixture(t)
	content := `55a0b2e00000-7ffd0a1f4000 ---p 00000000 00:00 0                          [rollup]
Rss:               8192 kB
Pss:               3072 kB
Pss_Dirty:          999 kB
Private_Dirty:     2048 kB
Swap:                 0 kB
`
	writeFile(t, filepath.Join(root, "42", "smaps_rollup"), content)

	m, err := ReadProcessMemory(42)
	if err != nil {
		t.Fatal(err)
	}
	if m.PrivateDirtyKiB != 2048 {
		t.Errorf("PrivateDirty = %d, want 2048 (Pss_Dirty must not be matched)", m.PrivateDirtyKiB)
	}
}

func TestReadProcessMemoryMissingPID(t *testing.T) {
	withProcFixture(t)
	if _, err := ReadProcessMemory(99999); err == nil {
		t.Error("ReadProcessMemory on a nonexistent pid returned nil error")
	}
}

// The headline warm-pool question: RSS summed across a fleet counts shared pages once per
// process, so it overstates real usage. PSS is the figure that supports a density claim.
func TestFleetMemorySharingRatio(t *testing.T) {
	root := withProcFixture(t)

	// Ten processes each mapping 100 MiB, of which 90 MiB is shared and 10 MiB private.
	// PSS attributes 1/10 of the shared portion to each, so PSS is 9 + 10 = 19 MiB.
	var pids []int
	for i := 0; i < 10; i++ {
		pid := 2000 + i
		pids = append(pids, pid)
		writeSmapsRollup(t, root, pid,
			102400, // RSS 100 MiB
			19456,  // PSS 19 MiB
			92160,  // Shared_Clean 90 MiB
			10240,  // Private_Dirty 10 MiB
			0)
	}

	fm, err := ReadFleetMemory(pids)
	if err != nil {
		t.Fatalf("ReadFleetMemory: %v", err)
	}
	if len(fm.Processes) != 10 {
		t.Fatalf("read %d processes, want 10", len(fm.Processes))
	}
	if fm.TotalRSSKiB != 1024000 {
		t.Errorf("TotalRSS = %d, want 1024000", fm.TotalRSSKiB)
	}
	if fm.TotalPSSKiB != 194560 {
		t.Errorf("TotalPSS = %d, want 194560", fm.TotalPSSKiB)
	}
	// RSS overstates by roughly 5.3x here, which is exactly the error a density estimate
	// based on RSS would carry.
	if ratio := fm.SharingRatio(); ratio < 5.2 || ratio > 5.3 {
		t.Errorf("SharingRatio = %.2f, want about 5.26", ratio)
	}
	// Private dirty is the true marginal cost of one more microVM.
	if fm.PerVMPrivateDirtyKiB != 10240 {
		t.Errorf("PerVMPrivateDirty = %d, want 10240", fm.PerVMPrivateDirtyKiB)
	}
}

// A microVM exiting between enumeration and reading is expected at density. Losing that one
// process is acceptable; losing the whole sample is not.
func TestFleetMemorySkipsVanishedProcesses(t *testing.T) {
	root := withProcFixture(t)
	writeSmapsRollup(t, root, 1, 1024, 512, 256, 256, 0)
	writeSmapsRollup(t, root, 3, 1024, 512, 256, 256, 0)

	fm, err := ReadFleetMemory([]int{1, 2, 3}) // pid 2 does not exist
	if err != nil {
		t.Fatalf("ReadFleetMemory returned an error for a vanished pid: %v", err)
	}
	if len(fm.Processes) != 2 {
		t.Errorf("read %d processes, want 2 (the missing pid should be skipped)", len(fm.Processes))
	}
}

func TestFleetMemoryEmpty(t *testing.T) {
	withProcFixture(t)
	fm, err := ReadFleetMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	if fm.SharingRatio() != 0 {
		t.Errorf("SharingRatio on an empty fleet = %v, want 0", fm.SharingRatio())
	}
	if fm.PerVMPrivateDirtyKiB != 0 {
		t.Error("PerVMPrivateDirty on an empty fleet should be 0")
	}
}

func TestReadHostMemory(t *testing.T) {
	root := withProcFixture(t)
	writeFile(t, filepath.Join(root, "meminfo"), `MemTotal:       792080000 kB
MemFree:        700000000 kB
MemAvailable:   750000000 kB
Buffers:            50000 kB
Cached:          40000000 kB
SwapTotal:              0 kB
SwapFree:               0 kB
HugePages_Total:        0
HugePages_Free:         0
`)
	h, err := ReadHostMemory()
	if err != nil {
		t.Fatal(err)
	}
	if h.TotalKiB != 792080000 {
		t.Errorf("TotalKiB = %d", h.TotalKiB)
	}
	if h.CachedKiB != 40000000 {
		t.Errorf("CachedKiB = %d", h.CachedKiB)
	}
	if h.SwapInUse() {
		t.Error("SwapInUse() = true with SwapTotal 0")
	}
}

// Swap in use means guest memory can reach host storage, which invalidates the security
// posture the provisioning scripts establish. It must be detected, not ignored.
func TestSwapInUseDetected(t *testing.T) {
	root := withProcFixture(t)
	writeFile(t, filepath.Join(root, "meminfo"), `MemTotal:       792080000 kB
SwapTotal:        8388608 kB
SwapFree:         8000000 kB
`)
	h, err := ReadHostMemory()
	if err != nil {
		t.Fatal(err)
	}
	if !h.SwapInUse() {
		t.Error("SwapInUse() = false when swap is partially consumed")
	}
}

func TestReadHostCounters(t *testing.T) {
	root := withProcFixture(t)
	writeFile(t, filepath.Join(root, "stat"), `cpu  1000 0 500 90000 100 0 50 0 0 0
cpu0 100 0 50 9000 10 0 5 0 0 0
intr 123456789 0 0 0
ctxt 987654321
btime 1750000000
processes 45678
procs_running 3
procs_blocked 1
softirq 111111 0 0 0
`)
	c, err := ReadHostCounters()
	if err != nil {
		t.Fatal(err)
	}
	if c.ContextSwitches != 987654321 {
		t.Errorf("ContextSwitches = %d", c.ContextSwitches)
	}
	if c.ProcsRunning != 3 || c.ProcsBlocked != 1 {
		t.Errorf("procs running/blocked = %d/%d, want 3/1", c.ProcsRunning, c.ProcsBlocked)
	}
}

func TestCounterDeltaRate(t *testing.T) {
	earlier := &HostCounters{ContextSwitches: 1000, At: time.Now()}
	later := &HostCounters{ContextSwitches: 3000, At: earlier.At.Add(2 * time.Second)}

	d := later.Sub(earlier)
	if d.ContextSwitches != 2000 {
		t.Errorf("delta = %d, want 2000", d.ContextSwitches)
	}
	if rate := d.ContextSwitchesPerSecond(); rate != 1000 {
		t.Errorf("rate = %v, want 1000/s", rate)
	}
}

// A counter read against a newer baseline, or a wrap, must not produce a negative-looking
// rate from unsigned arithmetic.
func TestCounterDeltaSaturatesAtZero(t *testing.T) {
	later := &HostCounters{ContextSwitches: 100, At: time.Now()}
	earlier := &HostCounters{ContextSwitches: 500, At: later.At.Add(-time.Second)}

	if d := later.Sub(earlier); d.ContextSwitches != 0 {
		t.Errorf("delta = %d, want 0 when the counter went backwards", d.ContextSwitches)
	}
}

func TestCounterDeltaZeroElapsed(t *testing.T) {
	now := time.Now()
	a := &HostCounters{ContextSwitches: 100, At: now}
	b := &HostCounters{ContextSwitches: 200, At: now}
	if rate := b.Sub(a).ContextSwitchesPerSecond(); rate != 0 {
		t.Errorf("rate with zero elapsed = %v, want 0 rather than a division by zero", rate)
	}
}

func TestLatencyPercentiles(t *testing.T) {
	var l LatencySamples
	// 1..100 ms, added out of order to confirm sorting happens internally.
	for i := 100; i >= 1; i-- {
		l.Add(time.Duration(i) * time.Millisecond)
	}

	if l.Len() != 100 {
		t.Fatalf("Len = %d, want 100", l.Len())
	}
	if got, want := l.Min(), 1*time.Millisecond; got != want {
		t.Errorf("Min = %v, want %v", got, want)
	}
	if got, want := l.Max(), 100*time.Millisecond; got != want {
		t.Errorf("Max = %v, want %v", got, want)
	}
	// Nearest-rank p99 of 1..100 is the 99th value.
	if got, want := l.Percentile(99), 99*time.Millisecond; got != want {
		t.Errorf("p99 = %v, want %v", got, want)
	}
	if got, want := l.Percentile(50), 50*time.Millisecond; got != want {
		t.Errorf("p50 = %v, want %v", got, want)
	}
	if got, want := l.Mean(), 50500*time.Microsecond; got != want {
		t.Errorf("Mean = %v, want %v", got, want)
	}
	if got, want := l.Jitter(), 99*time.Millisecond; got != want {
		t.Errorf("Jitter = %v, want %v", got, want)
	}
}

// p99 must report an observed sample, not an interpolation. With one clear outlier, the p99
// of a 100-sample set should surface it rather than average it away.
func TestPercentileReportsObservedValue(t *testing.T) {
	var l LatencySamples
	for i := 0; i < 99; i++ {
		l.Add(time.Millisecond)
	}
	l.Add(500 * time.Millisecond) // single tail outlier

	if got := l.Percentile(100); got != 500*time.Millisecond {
		t.Errorf("p100 = %v, want the outlier 500ms", got)
	}
	if got := l.Mean(); got > 10*time.Millisecond {
		t.Errorf("Mean = %v; the outlier should barely move the mean, which is why p99 matters", got)
	}
}

func TestLatencyEmpty(t *testing.T) {
	var l LatencySamples
	if l.Mean() != 0 || l.Percentile(99) != 0 || l.Jitter() != 0 {
		t.Error("empty LatencySamples should report zeros rather than panicking")
	}
}

// Percentile rank must round up, not down.
//
// A floor-based rank returns the wrong sample for small sets and, at p99 of exactly 100
// samples, silently reports the 99th value when asked for a rank that should include the
// tail. A mutation changing ceil to floor initially survived the other percentile tests,
// which is why this case is pinned explicitly with sets small enough that the off-by-one
// is unambiguous.
func TestPercentileRankRoundsUp(t *testing.T) {
	cases := []struct {
		name    string
		samples []time.Duration
		p       float64
		want    time.Duration
	}{
		// With 4 samples, p25 covers the first quarter: ceil(4*0.25)=1, so sample 1.
		{"4 samples p25", ms(1, 2, 3, 4), 25, 1 * time.Millisecond},
		// ceil(4*0.26)=2, so the boundary moves to sample 2. A floor rank would give 1.
		{"4 samples p26", ms(1, 2, 3, 4), 26, 2 * time.Millisecond},
		// ceil(4*0.5)=2. A floor rank also gives 2 here, so this alone is not decisive.
		{"4 samples p50", ms(1, 2, 3, 4), 50, 2 * time.Millisecond},
		// ceil(4*0.51)=3.
		{"4 samples p51", ms(1, 2, 3, 4), 51, 3 * time.Millisecond},
		// The decisive small case: ceil(3*0.34)=2, floor would give 1.
		{"3 samples p34", ms(10, 20, 30), 34, 20 * time.Millisecond},
		// ceil(10*0.01)=1: the lowest sample, not index 0 of an empty rank.
		{"10 samples p1", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 1, 1 * time.Millisecond},
		// A single sample answers every percentile.
		{"1 sample p99", ms(42), 99, 42 * time.Millisecond},
	}
	for _, c := range cases {
		var l LatencySamples
		for _, s := range c.samples {
			l.Add(s)
		}
		if got := l.Percentile(c.p); got != c.want {
			t.Errorf("%s: Percentile(%v) = %v, want %v", c.name, c.p, got, c.want)
		}
	}
}

// A floor-based rank can produce rank 0 for a low percentile on a small set, which would
// index out of bounds without clamping. Confirm the clamp holds.
func TestPercentileLowEndDoesNotUnderflow(t *testing.T) {
	var l LatencySamples
	l.Add(5 * time.Millisecond)
	l.Add(7 * time.Millisecond)

	for _, p := range []float64{0.0001, 0.5, 1, 5} {
		if got := l.Percentile(p); got != 5*time.Millisecond {
			t.Errorf("Percentile(%v) = %v, want the minimum 5ms", p, got)
		}
	}
}

func ms(vals ...int) []time.Duration {
	out := make([]time.Duration, len(vals))
	for i, v := range vals {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}
