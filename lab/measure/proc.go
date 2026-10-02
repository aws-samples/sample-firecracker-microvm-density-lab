// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package measure collects host-side metrics for a running microVM fleet.
//
// Everything here reads /proc and sysfs, so it needs no guest cooperation and no guest
// agent. That matters because the questions it answers are the ones the density model is
// least sure about:
//
//   - What does an idle warm pool actually cost in memory? The model assumes copy-on-write
//     sharing of the snapshot or rootfs makes idle microVMs nearly free. PSS is what
//     settles it, because RSS counts shared pages once per process and so overstates the
//     total by a factor of the fleet size.
//   - Does launch latency degrade as concurrency rises?
//   - How much memory does the VMM itself add per microVM?
package measure

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// procRoot is a variable so tests can point it at a fixture tree.
var procRoot = "/proc"

// ProcessMemory is the memory footprint of one process, in kibibytes.
type ProcessMemory struct {
	PID int
	// RSS is resident set size. Shared pages are counted in full for every process that
	// maps them, so summing RSS across a fleet overstates real usage.
	RSSKiB int
	// PSS is proportional set size: each shared page is divided by the number of
	// processes mapping it. Summing PSS across the fleet gives a figure that does not
	// double count, which is the number to use for a density claim.
	PSSKiB int
	// SharedCleanKiB is unmodified shared pages, largely the rootfs and kernel image
	// mapped from page cache. A large value here is the copy-on-write sharing the density
	// model depends on.
	SharedCleanKiB int
	// PrivateDirtyKiB is memory unique to this process and modified, so it cannot be
	// shared or reclaimed. This is the true marginal cost of one more microVM.
	PrivateDirtyKiB int
	// SwapKiB should be zero on a correctly provisioned host, since swap is disabled to
	// keep guest memory off host storage.
	SwapKiB int
}

// FleetMemory aggregates a set of processes.
type FleetMemory struct {
	Processes []ProcessMemory

	TotalRSSKiB          int
	TotalPSSKiB          int
	TotalPrivateDirtyKiB int
	TotalSharedCleanKiB  int

	// PerVMPrivateDirtyKiB is the mean private dirty memory per process, which is the
	// figure to multiply when projecting a larger fleet.
	PerVMPrivateDirtyKiB int
}

// ReadProcessMemory parses /proc/<pid>/smaps_rollup for one process.
//
// smaps_rollup is used rather than smaps because it is pre-aggregated by the kernel.
// Parsing full smaps for 180 processes means reading thousands of mapping entries each, and
// the cost of that shows up in the measurement it is supposed to be observing.
func ReadProcessMemory(pid int) (*ProcessMemory, error) {
	path := filepath.Join(procRoot, strconv.Itoa(pid), "smaps_rollup")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	m := &ProcessMemory{PID: pid}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := parseKiBLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "Rss":
			m.RSSKiB = val
		case "Pss":
			m.PSSKiB = val
		case "Shared_Clean":
			m.SharedCleanKiB = val
		case "Private_Dirty":
			m.PrivateDirtyKiB = val
		case "Swap":
			m.SwapKiB = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return m, nil
}

// ReadFleetMemory aggregates memory across a set of PIDs.
//
// PIDs that have exited are skipped rather than failing the whole measurement: at density,
// a microVM ending between enumeration and reading is expected, and losing the entire
// sample to it would be worse than losing one process.
func ReadFleetMemory(pids []int) (*FleetMemory, error) {
	fm := &FleetMemory{}
	for _, pid := range pids {
		m, err := ReadProcessMemory(pid)
		if err != nil {
			if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
				continue
			}
			return nil, err
		}
		fm.Processes = append(fm.Processes, *m)
		fm.TotalRSSKiB += m.RSSKiB
		fm.TotalPSSKiB += m.PSSKiB
		fm.TotalPrivateDirtyKiB += m.PrivateDirtyKiB
		fm.TotalSharedCleanKiB += m.SharedCleanKiB
	}
	if n := len(fm.Processes); n > 0 {
		fm.PerVMPrivateDirtyKiB = fm.TotalPrivateDirtyKiB / n
	}
	return fm, nil
}

// SharingRatio reports how much larger the naive RSS sum is than the PSS sum.
//
// A ratio near 1 means little sharing, so each microVM genuinely costs its full RSS. A
// ratio well above 1 is the copy-on-write sharing the warm pool depends on, and quantifies
// how much a density estimate based on RSS would overstate memory use.
func (f *FleetMemory) SharingRatio() float64 {
	if f.TotalPSSKiB == 0 {
		return 0
	}
	return float64(f.TotalRSSKiB) / float64(f.TotalPSSKiB)
}

// HostMemory is system-wide memory state from /proc/meminfo.
type HostMemory struct {
	TotalKiB     int
	AvailableKiB int
	// CachedKiB includes the page cache backing shared rootfs and snapshot files. It
	// should grow with fleet size if sharing is working as the model assumes.
	CachedKiB    int
	SwapTotalKiB int
	SwapFreeKiB  int
	// HugePagesTotal and HugePagesFree are reported because hugepage strategy is an
	// unmeasured variable in the density model.
	HugePagesTotal int
	HugePagesFree  int
}

// ReadHostMemory parses /proc/meminfo.
func ReadHostMemory() (*HostMemory, error) {
	path := filepath.Join(procRoot, "meminfo")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := &HostMemory{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := parseKiBLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			h.TotalKiB = val
		case "MemAvailable":
			h.AvailableKiB = val
		case "Cached":
			h.CachedKiB = val
		case "SwapTotal":
			h.SwapTotalKiB = val
		case "SwapFree":
			h.SwapFreeKiB = val
		case "HugePages_Total":
			h.HugePagesTotal = val
		case "HugePages_Free":
			h.HugePagesFree = val
		}
	}
	return h, sc.Err()
}

// SwapInUse reports whether any swap is currently used. Guest memory reaching host storage
// is a data-remanence problem, so a non-zero value invalidates the security posture the
// provisioning scripts establish.
func (h *HostMemory) SwapInUse() bool {
	return h.SwapTotalKiB > 0 && h.SwapFreeKiB < h.SwapTotalKiB
}

// HostCounters are scheduler and interrupt counters from /proc/stat.
//
// These are host-wide. On a part where one last-level cache spans every NUMA node, cache
// pressure is likewise host-wide, so whole-host counters are the correct granularity rather
// than a limitation.
type HostCounters struct {
	ContextSwitches uint64
	Interrupts      uint64
	ProcessesForked uint64
	ProcsRunning    int
	ProcsBlocked    int
	At              time.Time
}

// ReadHostCounters parses /proc/stat.
func ReadHostCounters() (*HostCounters, error) {
	path := filepath.Join(procRoot, "stat")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	c := &HostCounters{At: time.Now()}
	sc := bufio.NewScanner(f)
	// /proc/stat lines can be long on a 192-CPU host.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "ctxt":
			c.ContextSwitches, _ = strconv.ParseUint(fields[1], 10, 64)
		case "intr":
			c.Interrupts, _ = strconv.ParseUint(fields[1], 10, 64)
		case "processes":
			c.ProcessesForked, _ = strconv.ParseUint(fields[1], 10, 64)
		case "procs_running":
			c.ProcsRunning, _ = strconv.Atoi(fields[1])
		case "procs_blocked":
			c.ProcsBlocked, _ = strconv.Atoi(fields[1])
		}
	}
	return c, sc.Err()
}

// CounterDelta is the change between two counter reads, with the elapsed interval so rates
// can be derived.
type CounterDelta struct {
	ContextSwitches uint64
	Interrupts      uint64
	ProcessesForked uint64
	Elapsed         time.Duration
}

// Sub returns the delta from an earlier reading.
func (c *HostCounters) Sub(earlier *HostCounters) CounterDelta {
	return CounterDelta{
		ContextSwitches: satSub(c.ContextSwitches, earlier.ContextSwitches),
		Interrupts:      satSub(c.Interrupts, earlier.Interrupts),
		ProcessesForked: satSub(c.ProcessesForked, earlier.ProcessesForked),
		Elapsed:         c.At.Sub(earlier.At),
	}
}

// ContextSwitchesPerSecond derives a rate, which is more comparable across runs of
// different length than a raw count.
func (d CounterDelta) ContextSwitchesPerSecond() float64 {
	if d.Elapsed <= 0 {
		return 0
	}
	return float64(d.ContextSwitches) / d.Elapsed.Seconds()
}

// LatencySamples is a set of durations with percentile helpers.
//
// Tail latency is the number that matters for a density claim: a mean that looks fine while
// p99 degrades means some rollouts are slow, which shows up as reduced training throughput
// rather than as an error.
type LatencySamples struct {
	samples []time.Duration
	sorted  bool
}

// Add records a sample.
func (l *LatencySamples) Add(d time.Duration) {
	l.samples = append(l.samples, d)
	l.sorted = false
}

// Len returns the sample count.
func (l *LatencySamples) Len() int { return len(l.samples) }

func (l *LatencySamples) ensureSorted() {
	if !l.sorted {
		sort.Slice(l.samples, func(i, j int) bool { return l.samples[i] < l.samples[j] })
		l.sorted = true
	}
}

// Mean returns the arithmetic mean, or zero when there are no samples.
func (l *LatencySamples) Mean() time.Duration {
	if len(l.samples) == 0 {
		return 0
	}
	var total time.Duration
	for _, s := range l.samples {
		total += s
	}
	return total / time.Duration(len(l.samples))
}

// Percentile returns the sample at the given percentile using nearest-rank, which reports
// an actually observed value rather than an interpolation between two.
func (l *LatencySamples) Percentile(p float64) time.Duration {
	if len(l.samples) == 0 {
		return 0
	}
	if p <= 0 {
		l.ensureSorted()
		return l.samples[0]
	}
	if p >= 100 {
		l.ensureSorted()
		return l.samples[len(l.samples)-1]
	}
	l.ensureSorted()
	// Nearest-rank: ceil(p/100 * N), clamped into range.
	rank := int(float64(len(l.samples))*p/100 + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(l.samples) {
		rank = len(l.samples)
	}
	return l.samples[rank-1]
}

// Min returns the smallest sample.
func (l *LatencySamples) Min() time.Duration { return l.Percentile(0) }

// Max returns the largest sample.
func (l *LatencySamples) Max() time.Duration { return l.Percentile(100) }

// Jitter returns max minus min, a cheap spread measure that does not assume a distribution.
func (l *LatencySamples) Jitter() time.Duration {
	if len(l.samples) == 0 {
		return 0
	}
	return l.Max() - l.Min()
}

// parseKiBLine parses lines shaped "Key:   1234 kB" from smaps_rollup and meminfo.
func parseKiBLine(line string) (key string, valueKiB int, ok bool) {
	name, rest, found := strings.Cut(line, ":")
	if !found {
		return "", 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.Atoi(fields[0])
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(name), v, true
}

// satSub subtracts with a floor of zero, so a counter wrap or a re-read against a newer
// baseline cannot produce a nonsensical negative rate.
func satSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
