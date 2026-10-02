// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Command labsweep runs the microVM density sweep and writes results.
//
// This is the tool that produces the numbers. It launches microVMs across placement
// configurations and density levels, drives a workload in each guest, and records latency and
// host metrics.
//
// It requires root (cgroups and the jailer), a host prepared by provision-host.sh, a guest
// kernel, and a rootfs containing the guest agent.
//
// Usage:
//
//	labsweep -dry-run                             plan the matrix without launching anything
//	labsweep -configs B,D -densities 10,20        a small run
//	labsweep -out results/                        full run, writing JSON, CSV, and a summary
//	labsweep -repeats 5 -out results/             five repeats, so variance can be reported
//	labsweep -task-instance <id> -out results/    run the real task baked into the rootfs
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/sweep"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/topology"
)

func main() {
	cfg := sweep.DefaultConfig()

	var (
		configsFlag   = flag.String("configs", "A,B,C,D,E,F", "comma-separated placement configs to run")
		densitiesFlag = flag.String("densities", "10,45,90,135,180", "comma-separated density levels")
		outDir        = flag.String("out", "", "directory for results (default: print summary only)")
		dryRun        = flag.Bool("dry-run", false, "plan the matrix and print it without launching")
		iterations    = flag.Uint64("iterations", cfg.Workload.Iterations, "compute-loop iterations per round")
		rounds        = flag.Int("rounds", cfg.Workload.Rounds, "minimum workload rounds per microVM")
		memMiB        = flag.Int("mem", cfg.Workload.MemSizeMiB, "MiB per microVM")
		// The compute loop has a ~16-byte working set and never reaches last-level cache, so it
		// cannot evaluate cache-affinity placement. This switches to a pointer chase whose
		// per-access latency tracks which cache level holds the buffer.
		workingSetKiB = flag.Int("working-set-kib", cfg.Workload.WorkingSetKiB,
			"per-microVM pointer-chase buffer in KiB; 0 uses the arithmetic compute loop")
		// The sandbox workload exercises the three paths neither other workload touches:
		// fork/exec, resident memory, and writeback. resident-kib is the parameter that decides
		// whether memory or cores is the binding density constraint, since every measurement
		// before this used a 128 MiB guest that no real sandbox would use.
		residentKiB = flag.Int("resident-kib", 0,
			"per-microVM resident set in KiB, held for the whole episode; >0 selects the sandbox workload")
		sandboxSteps = flag.Int("steps", 20,
			"tool calls per episode for the sandbox workload; each forks and reaps a child process")
		// A real task from a converted image. The command lives in the image, so the host only
		// needs the instance identity and what exit code counts as a pass.
		taskInstance = flag.String("task-instance", "",
			"run the real task baked into the guest image, labelled with this instance ID (for example django__django-11099)")
		taskExpectExit = flag.Int("task-expect-exit", 0,
			"exit code that means the task passed; a cell whose tasks exit otherwise is marked untrustworthy")
		// The task's own limit, from its upstream definition. Checked against -request-timeout so a
		// task cut off early is caught before the run rather than recorded as a failure.
		taskTimeoutSec = flag.Float64("task-timeout-sec", 0,
			"the task's own timeout from its benchmark definition (Terminal-Bench max_test_timeout_sec); 0 = unstated")
		// A full RL episode: many turns replayed from a recorded agent trajectory, then the verifier.
		// This is what a customer sizes a fleet for -- a task run is one component of it -- and the
		// guest reports turn time and verify time separately so the ratio is readable.
		episodeInstance = flag.String("episode-instance", "",
			"replay the episode baked into the guest image, labelled with this instance ID")
		episodeTurns = flag.Int("episode-turns", 0,
			"turns the trajectory has; a run reporting a different count stopped partway and is marked untrustworthy")
		episodeTurnsOK = flag.Int("episode-turns-ok", 0,
			"turns that exited zero in the recording; below -episode-turns is normal, since real rollouts contain mistakes")
		episodeExpectExit = flag.Int("episode-expect-exit", 0,
			"verifier exit code that means the episode earned its reward")
		episodeTimeoutSec = flag.Float64("episode-timeout-sec", 0,
			"how long one episode may take; checked against -request-timeout, and an episode is far longer than a verifier run")
		episodeTrajectory = flag.String("episode-trajectory", "",
			"where the recording came from, for provenance; a replay is only interpretable against the run it replays")
		sandboxWriteKiB = flag.Int("write-kib", 256,
			"bytes written per step to a guest tmpfs; note tmpfs pages are guest memory, so this counts against -mem")
		// Repeats and target samples address two different sample-size problems. Repeats bound
		// run-to-run variance across the whole matrix; target samples fix the within-cell
		// starvation that a fixed round count creates at low density.
		repeats = flag.Int("repeats", cfg.Repeats,
			"how many times to run the whole matrix; 1 gives no variance information")
		targetSamples = flag.Int("target-samples", cfg.Workload.TargetSamples,
			"samples to aim for per cell by scaling rounds inversely with density (0 = fixed rounds)")
		shuffle = flag.Bool("shuffle", cfg.ShuffleCells,
			"randomise cell order within each repeat so adjacency effects do not attach to one configuration")
		shuffleSeed = flag.Int64("shuffle-seed", 0,
			"seed for cell shuffling (0 = derive from the clock and record what was used)")
		concurrency = flag.Int("launch-concurrency", cfg.LaunchConcurrency, "microVMs to launch at once")
		kernel      = flag.String("kernel", cfg.KernelPath, "guest kernel path")
		rootfs      = flag.String("rootfs", cfg.RootfsPath, "rootfs image path")
		timeout     = flag.Duration("timeout", 0, "abort the sweep after this long (0 = no limit)")
		// Distinct from -timeout, which bounds the whole sweep. This bounds one request, and it
		// has to exceed the longest a single task may legitimately take: a request cut off early
		// is recorded as a failed task, not a slow one, so too low a value quietly reports a
		// wrong pass rate. Terminal-Bench tasks declare max_test_timeout_sec (typically 180s),
		// which is above the default suited to the synthetic workloads.
		requestTimeout = flag.Duration("request-timeout", cfg.RequestTimeout,
			"bound one workload request; must exceed the task's own timeout when running real tasks")
		// EC2 CpuOptions are fixed at launch, so SMT state is a host property. These flags
		// record which host this is so an SMT-on and an SMT-off dataset are distinguishable.
		threadsPerCore = flag.Int("requested-threads-per-core", 0,
			"CpuOptions ThreadsPerCore the instance was launched with (0 = unstated); a mismatch against the observed value is reported as a caveat")
		runLabel = flag.String("run-label", "",
			"names this host within a multi-host experiment, for example smt-on or smt-off")
	)
	flag.Parse()

	configs, err := parseConfigs(*configsFlag)
	if err != nil {
		fatal(err)
	}
	densities, err := parseDensities(*densitiesFlag)
	if err != nil {
		fatal(err)
	}

	cfg.Configs = configs
	cfg.Densities = densities
	cfg.Workload.Iterations = *iterations
	cfg.Workload.Rounds = *rounds
	cfg.Workload.TargetSamples = *targetSamples
	cfg.Workload.WorkingSetKiB = *workingSetKiB
	if *taskInstance != "" {
		cfg.Workload.Task = &sweep.TaskSpec{
			InstanceID:   *taskInstance,
			ExpectedExit: *taskExpectExit,
			TimeoutSec:   *taskTimeoutSec,
		}
	}
	if *episodeInstance != "" {
		cfg.Workload.Episode = &sweep.EpisodeSpec{
			InstanceID:       *episodeInstance,
			TrajectorySource: *episodeTrajectory,
			Turns:            *episodeTurns,
			ExpectedTurnsOK:  *episodeTurnsOK,
			ExpectedExit:     *episodeExpectExit,
			TimeoutSec:       *episodeTimeoutSec,
		}
	}
	if *residentKiB > 0 {
		cfg.Workload.Sandbox = &sweep.SandboxSpec{
			ResidentKiB: *residentKiB,
			Steps:       *sandboxSteps,
			WriteKiB:    *sandboxWriteKiB,
		}
	}
	cfg.Workload.MemSizeMiB = *memMiB
	cfg.Repeats = *repeats
	cfg.ShuffleCells = *shuffle
	cfg.ShuffleSeed = *shuffleSeed
	cfg.LaunchConcurrency = *concurrency
	cfg.RequestTimeout = *requestTimeout
	cfg.KernelPath = *kernel
	cfg.RootfsPath = *rootfs
	cfg.RequestedThreadsPerCore = *threadsPerCore
	cfg.RunLabel = *runLabel

	if *dryRun {
		if err := planOnly(cfg); err != nil {
			fatal(err)
		}
		return
	}

	// Validate before the root check, so a misconfigured flag combination is reported without
	// the caller first having to work out that sudo was needed. On a paid host that ordering is
	// the difference between one round trip and two.
	if err := cfg.Validate(); err != nil {
		fatal(err)
	}

	if os.Geteuid() != 0 {
		fatal(fmt.Errorf("must run as root: the sweep writes cgroups and invokes the jailer"))
	}

	runner, err := sweep.NewRunner(cfg)
	if err != nil {
		fatal(err)
	}

	// A sweep occupies paid hardware, so an interrupt must stop it promptly and still write
	// whatever was collected rather than discarding the session.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if *timeout > 0 {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, *timeout)
		defer tcancel()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\ninterrupted: finishing the current cell and writing results")
		cancel()
	}()

	results, runErr := runner.Run(ctx)

	// Write results even on error. Partial data from an expensive session is worth keeping.
	if *outDir != "" {
		if err := writeResults(*outDir, results); err != nil {
			fmt.Fprintf(os.Stderr, "warning: writing results: %v\n", err)
		}
	}
	if err := results.Summarize(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "warning: summarize: %v\n", err)
	}

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "\nsweep ended early: %v\n", runErr)
		os.Exit(1)
	}
}

// planOnly prints the matrix without launching, so the plan can be reviewed before spending
// hardware time.
func planOnly(cfg sweep.Config) error {
	top, err := topology.Discover()
	if err != nil {
		return fmt.Errorf("discover topology: %w", err)
	}

	fmt.Printf("Host: %d physical cores x %d threads, %d NUMA nodes, SMT %s\n",
		top.PhysicalCores(), top.ThreadsPerCore(), len(top.Nodes), top.SMTState)
	fmt.Printf("Cache: %s\n\n", top.CacheMode())

	fmt.Printf("%-4s %8s %10s %8s %9s %12s %s\n",
		"cfg", "density", "planned", "rounds", "samples", "shortfall", "notes")

	var totalCells int
	var totalRounds int
	starved := 0
	for _, c := range cfg.Configs {
		if c.RequiresSMT() && !top.HasSMT() {
			fmt.Printf("%-4s %8s %10s %8s %9s %12s requires SMT; host has %d thread(s) per core\n",
				c, "-", "-", "-", "-", "-", top.ThreadsPerCore())
			continue
		}
		for _, d := range cfg.Densities {
			plan, err := placement.Build(top, c, d, cfg.Placement)
			if err != nil {
				fmt.Printf("%-4s %8d %10s %8s %9s %12s %v\n", c, d, "-", "-", "-", "-", err)
				continue
			}
			short := d - len(plan.VMs)
			shortStr := "-"
			if short > 0 {
				shortStr = fmt.Sprintf("%d", short)
			}
			note := ""
			if len(plan.Notes) > 0 {
				note = plan.Notes[0]
			}
			// Rounds scale with requested density, but samples come from the microVMs that
			// actually run, so a planned-down cell yields fewer samples than the target.
			rounds := cfg.Workload.RoundsFor(d)
			samples := rounds * len(plan.VMs)
			if samples < 100 {
				starved++
			}
			fmt.Printf("%-4s %8d %10d %8d %9d %12s %s\n",
				c, d, len(plan.VMs), rounds, samples, shortStr, note)
			totalCells++
			totalRounds += rounds
		}
	}

	// An estimate is worth printing because it is the difference between a session that fits
	// in an hour and one that does not. Rounds vary per cell now, so the estimate sums actual
	// rounds rather than multiplying a single figure by the cell count.
	repeats := cfg.Repeats
	if repeats < 1 {
		repeats = 1
	}
	perRound := 2 * time.Second
	estimate := time.Duration(totalRounds*repeats) * perRound
	fmt.Printf("\n%d cells x %d repeat(s) = %d runs. Very rough estimate: %v plus launch and\n",
		totalCells, repeats, totalCells*repeats, estimate)
	fmt.Println("teardown per cell.")
	if cfg.ShuffleCells {
		fmt.Println("Cell order will be shuffled within each repeat; the seed is recorded in results.")
	}
	if repeats == 1 {
		fmt.Println("\nOne repeat: results will carry no run-to-run variance information, so no")
		fmt.Println("difference between configurations can be separated from noise.")
	}
	if starved > 0 {
		fmt.Printf("\n%d cell(s) will have under 100 samples, where the nearest-rank p99 is the\n", starved)
		fmt.Println("maximum or adjacent to it. Raise -target-samples to fix, or read those p99")
		fmt.Println("values as single worst observations.")
	}
	fmt.Println("\nRun without -dry-run as root to execute.")
	return nil
}

func writeResults(dir string, r *sweep.Results) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// The run timestamp is the grouping key for a whole sweep, so it is kept separate from the
	// per-file stamp: it names the S3 prefix that holds every artifact from this run.
	runStamp := r.StartedAt.UTC().Format("20060102T150405Z")

	stamp := runStamp
	// Include the run label so artifacts from two passes in the same experiment do not
	// collide when collected into one directory or bucket prefix.
	if r.Host.RunLabel != "" {
		stamp = r.Host.RunLabel + "-" + runStamp
	}

	// Deterministic order so the log reads consistently across runs.
	artifacts := []struct {
		name  string
		write func(*os.File) error
	}{
		{fmt.Sprintf("sweep-%s.json", stamp), func(f *os.File) error { return r.WriteJSON(f) }},
		{fmt.Sprintf("sweep-%s.csv", stamp), func(f *os.File) error { return r.WriteCSV(f) }},
		{fmt.Sprintf("sweep-%s.txt", stamp), func(f *os.File) error { return r.Summarize(f) }},
		// Across-repeat spread, which is what makes a difference between configurations
		// reportable rather than merely observed.
		{fmt.Sprintf("variance-%s.csv", stamp), func(f *os.File) error { return r.WriteVarianceCSV(f) }},
		// Raw samples last because it is by far the largest, and it is the file that makes
		// the dataset reanalysable: percentiles cannot be recovered from percentiles.
		{fmt.Sprintf("samples-%s.csv", stamp), func(f *os.File) error { return r.WriteSamplesCSV(f) }},
	}

	var written []string
	for _, a := range artifacts {
		name, write := a.name, a.write
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if err := write(f); err != nil {
			f.Close()
			return fmt.Errorf("write %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", path)
		written = append(written, path)
	}

	fmt.Fprintf(os.Stderr, "%d raw samples retained in samples-%s.csv\n", len(r.Samples), stamp)

	if bucket := os.Getenv("SWEEP_S3_BUCKET"); bucket != "" {
		uploadToS3(bucket, runStamp, written)
	}
	return nil
}

// S3Prefix is the top-level bucket prefix holding benchmark artifacts. Runs live in
// timestamped subdirectories beneath it.
const S3Prefix = "benchmark_results"

// uploadToS3 copies results to a bucket so they survive instance termination.
//
// A sweep runs on an ephemeral instance that is terminated as soon as it finishes. Results
// that exist only on local disk are one forgotten step away from being lost, and reproducing
// them costs another paid session. Failures here are reported but not fatal: the local files
// are still on disk, and aborting after a successful run would be worse than a failed upload.
//
// Artifacts go to benchmark_results/<run timestamp>/ rather than a single shared prefix. One
// flat directory mixes runs together, and since filenames repeat across runs when the run label
// is the same, a later sweep would overwrite an earlier one's artifacts. A timestamped prefix
// per run keeps datasets separable and makes overwriting impossible.
func uploadToS3(bucket, runStamp string, paths []string) {
	if _, err := exec.LookPath("aws"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: SWEEP_S3_BUCKET set but the aws CLI is not installed; results remain local only\n")
		return
	}
	base := fmt.Sprintf("s3://%s/%s/%s", strings.TrimPrefix(bucket, "s3://"), S3Prefix, runStamp)

	var failed int
	for _, p := range paths {
		dst := fmt.Sprintf("%s/%s", base, filepath.Base(p))
		cmd := exec.Command("aws", "s3", "cp", p, dst)
		out, err := cmd.CombinedOutput()
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "warning: upload %s: %v: %s\n", p, err, strings.TrimSpace(string(out)))
			continue
		}
		fmt.Fprintf(os.Stderr, "uploaded %s\n", dst)
	}

	// State the outcome either way. A silent absence of "uploaded" lines is easy to miss when
	// it scrolls past at the end of a long sweep, and the cost of assuming a successful upload
	// is discovering the loss after the instance is gone.
	if failed > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d of %d artifacts failed to upload. Copy them off this host before terminating it:\n  %s\n",
			failed, len(paths), base)
		return
	}
	fmt.Fprintf(os.Stderr, "all %d artifacts uploaded to %s/\n", len(paths), base)
}

func parseConfigs(s string) ([]placement.Config, error) {
	var out []placement.Config
	for _, part := range strings.Split(s, ",") {
		part = strings.ToUpper(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		cfg := placement.Config(part)
		known := false
		for _, c := range placement.AllConfigs {
			if c == cfg {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown config %q; want a subset of A B C D E F", part)
		}
		out = append(out, cfg)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no configs selected")
	}
	return out, nil
}

func parseDensities(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("density %q is not a number", part)
		}
		if n <= 0 {
			return nil, fmt.Errorf("density %d must be positive", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no densities selected")
	}
	return out, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
