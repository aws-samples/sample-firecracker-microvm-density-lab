# Lab harness

Tooling for the microVM density sweep: discover host topology, compute per-microVM CPU
assignments for each sweep configuration, and (later) drive the sweep itself.

Written in Go and structured so the placement logic becomes the foundation of the
production placement agent in `../host-agent/`, rather than throwaway lab scaffolding.

## Status

| Package | Purpose | State |
|---------|---------|-------|
| `topology/` | Read CPU, cache, and NUMA layout from sysfs | Implemented, tested against a captured real host |
| `placement/` | Turn topology into per-microVM cpuset assignments for configs A to F | Implemented, tested |
| `firecracker/` | Minimal client for the Firecracker API over its Unix socket | Implemented, tested against a real socket |
| `launcher/` | Create cgroup hierarchies and start jailed Firecracker processes | Implemented, unit tested; **not yet run at density** |
| `measure/` | Host-side memory, counters, and latency percentiles from /proc | Implemented, tested |
| `sweep/` | Drive the config x density matrix, record results with provenance | Implemented, tested |
| `cmd/labctl/` | Read-only CLI: inspect topology, preview a plan | Implemented |
| `cmd/labsweep/` | Run the sweep, write JSON / CSV / summary | Implemented, **not yet run at density** |
| Guest workload | Compute loop inside each microVM, reported over vsock | Implemented in `../guest-agent/`, booted and verified on hardware |

`topology`, `placement`, and `labctl` are read-only and safe to run on a busy host.
`launcher` writes to cgroups and spawns processes, so it needs root and a host you are
willing to disturb.

### Why a hand-written API client

The official `firecracker-go-sdk` is the right choice for production code. This package
exists so the lab has no module dependencies to vendor onto a metal host, and so the exact
API calls appear in the blog rather than behind a wrapper. Endpoint shapes follow the
OpenAPI spec shipped with Firecracker v1.16.1.

## Usage

```bash
go build -o labctl ./cmd/labctl

./labctl topology              # what is this host?
./labctl topology -json        # same, machine-readable

./labctl plan -config B -density 90    # preview assignments
./labctl plan -config E -density 180 -json
```

Cross-compile for a metal host from a workstation:

```bash
GOOS=linux GOARCH=amd64 go build -o labctl-linux ./cmd/labctl
```

The result is CGO-free and has no runtime dependencies, so it can be copied to a host and
run directly.

## Why topology is discovered, not assumed

Instance type does not determine layout. On a captured `m8i.metal-48xl` running Amazon
Linux 2023, Linux reported **three NUMA nodes on a single socket**, 32 physical cores
each, with SMT siblings offset by 96:

```
NUMA node0 CPU(s):   0-31,96-127
NUMA node1 CPU(s):   32-63,128-159
NUMA node2 CPU(s):   64-95,160-191
```

Neither the node count nor the sibling numbering is derivable from the instance name, and
both change how a microVM must be pinned. `topology.Discover()` reads the machine.

`labctl topology` also classifies **how cache domains relate to NUMA nodes**, which is what
distinguishes one node per compute tile from a partitioning scheme. The discriminating
question is not how many cache domains sit *within* a node, but whether any single domain
**spans** nodes: a count of one per node looks identical on both architectures.

On the captured m8i, a single 480 MiB last-level cache covers all 192 CPUs and therefore all
three nodes, which is the Sub-NUMA Clustering shape. The practical consequence is that node
confinement buys memory locality but **not** cache isolation on that part. See
`sources/NUMA-TOPOLOGY-CAPTURE.md`.

## Sweep configurations

| Config | Strategy | Needs SMT |
|--------|----------|-----------|
| A | Unpinned baseline, host scheduler decides | no |
| B | One physical core per microVM, SMT siblings idle | no |
| C | SMT split: vCPU on one sibling, VMM on the other | **yes** |
| D | vCPU pinned per core, VMM threads on a shared pool | no |
| E | Oversubscribed: two vCPU threads per physical core | **yes** |
| F | SMT disabled at host level, one vCPU per core, VMM pool | **yes** |

`Config.RequiresSMT()` gates C, E, and F. AMD EC2 metal instances expose one thread per
core, so those configs have nothing to act on there: `placement.Build` returns
`ErrSMTRequired` rather than producing a plan that looks valid and measures nothing.
`labctl topology` prints which configs the current host can run.

## Two constraints the placement code enforces

**A microVM must not straddle a NUMA node.** Crossing nodes means guest memory accesses
can land on remote memory, which is the effect NUMA-aware placement exists to prevent.
`Plan.Validate` fails a plan that violates this.

**The cpuset partition flag is not delegatable.** `cpuset.cpus.partition` is owned by the
parent cgroup, so the agent must create the parent hierarchy and set partition state
itself. The jailer's `--cgroup` flag can write `cpuset.cpus` for a microVM but cannot make
that cgroup a partition root. Every plan therefore emits `ParentCPUSet`, the union of CPUs
the caller must establish on the parent first.

## Density ceilings are reported, not hidden

Strict pinning cannot exceed one microVM per allocatable physical core. Asking for more
returns a plan with fewer microVMs plus a note recording the shortfall, because the
ceiling is itself a result the sweep wants to capture. Silently oversubscribing would
misattribute a latency change to density when it came from contention.

Only config E is permitted to place two microVMs on one physical core, and `Validate`
rejects CPU sharing under any other config.

## Testing

```bash
go test ./...
```

`topology/fixture_test.go` replays the exact sysfs values captured from a live
`m8i.metal-48xl`, so the parsing is checked against real kernel output rather than a
synthesised approximation. `placement/placement_test.go` covers the invariants above,
including the no-SMT host case and the round-robin spread that keeps low-density runs from
measuring a single NUMA node.

## The cgroup constraint, in code

`cpuset.cpus.partition` is owned by the parent cgroup and is not delegatable. The jailer
can write `cpuset.cpus` for a microVM's own cgroup through `--cgroup`, but it cannot make
that cgroup a partition root. The launch sequence therefore is:

1. `launcher.CreateHierarchy` makes the parent, assigns the plan's `ParentCPUSet`, and
   delegates `+cpuset +cpu` to children.
2. `SetExclusive` declares which CPUs may be handed to child partitions. Skipping this
   makes every child partition request fail as invalid.
3. `CreateChild` makes the per-microVM cgroup and sets its `cpuset.cpus`.
4. The jailer runs with `--parent-cgroup` and **no** `--cgroup` entries, which under
   cgroup v2 makes it *move* the process into the existing cgroup rather than create one.
   Passing a `--cgroup` would take the microVM out from under the parent that owns
   partition state.

`SetPartition` reads the value back after writing it. The kernel can accept the write and
still refuse the transition, reporting something like
`isolated invalid (Invalid cpuset.cpus)`. Treating that as success would mean running the
experiment without the isolation it claims to test.

## What `measure/` answers without a guest

Host-side metrics need no guest cooperation, which is deliberate: the questions the density
model is least sure about are answerable from `/proc` alone.

**Is an idle warm pool nearly free?** The model assumes copy-on-write sharing makes it so.
`RSS` cannot settle this, because it counts shared pages in full for every process that maps
them, so summing it across 180 microVMs overstates memory by roughly the sharing factor.
`PSS` divides each shared page by the number of mappers, so `FleetMemory.TotalPSSKiB` is the
figure a density claim can rest on. `SharingRatio()` reports how far apart the two are, which
is exactly the error an RSS-based estimate would carry.

**What does one more microVM actually cost?** `PerVMPrivateDirtyKiB`. Private dirty memory
cannot be shared or reclaimed, so it is the true marginal cost, and it is the number to
multiply when projecting a larger fleet.

**Idle or working?** Both, and they are not the same question. Every cell records memory twice:
`*_boot` after the fleet settles, and `*_working` after the workload has run. Until 2026-09-18
only the first existed, under unqualified names, so a workload that created its own footprint —
a sandbox holding a resident set, a test suite growing a heap — was reported at its idle size.
**Size a fleet on the working figure**; the boot figure is what an idle warm-pool microVM costs.

The working figure is a high-water mark rather than a final state, because no balloon device is
configured and nothing calls `MADV_DONTNEED`: once the guest faults a page it stays resident in
the VMM's anonymous mapping. That is why one read at the end suffices and nothing samples during
the workload — reading `smaps_rollup` for 240 processes on a timer would add work inside the very
latency region being measured. `CheckWorkingSetMonotonic` records an error if the figure falls,
since that would mean the premise no longer holds.

Percentiles use nearest-rank, so a reported p99 is an observed sample rather than an
interpolation between two. Tail latency is what matters for a density claim: a healthy mean
alongside a degrading p99 means some rollouts are slow, which surfaces as reduced training
throughput rather than as an error.

Note from `sources/NUMA-TOPOLOGY-CAPTURE.md` that on m8i a single last-level cache spans all
NUMA nodes, so cache-pressure counters are a whole-host signal. That is the correct
granularity for this part, not a limitation of the tooling.

## Running a sweep

```bash
labsweep -dry-run                        # plan the matrix, no launching, no root needed
sudo labsweep -configs B,D -densities 10,20 -out results/
sudo labsweep -out results/              # full matrix, 5 repeats
sudo labsweep -repeats 1 -out results/   # single pass, for a smoke test only
```

`-dry-run` prints every cell with its planned microVM count, its round and sample counts, any
shortfall, and a rough time estimate. Worth running before a paid session: it catches an
impossible matrix, and warns about sample-starved cells, without spending hardware time.

Five artifacts are written per run:

| File | Contents |
|------|----------|
| `sweep-*.json` | Aggregates plus full host provenance, machine-readable |
| `sweep-*.csv` | One row per cell per repeat, for plotting |
| `variance-*.csv` | One row per cell, spread across repeats |
| `sweep-*.txt` | Human-readable summary, caveats first |
| `samples-*.csv` | **One row per microVM per round per repeat.** The reanalysable artifact |

Set `SWEEP_S3_BUCKET` to have results uploaded to
`s3://<bucket>/benchmark_results/<run timestamp>/` on completion. Sweeps run on ephemeral
instances that get terminated immediately afterwards, so results living only on instance-local
disk are one forgotten step from being lost.

A timestamped prefix **per run** rather than one shared directory: filenames repeat across runs
with the same run label, so a flat layout would let a later sweep overwrite an earlier one's
artifacts. The upload prints an explicit success or failure line for the whole set, because a
silent absence of "uploaded" lines scrolls past easily at the end of a long sweep. The instance
profile needs `s3:PutObject` under `benchmark_results/*`; see `LAB-RESOURCES.md`.

### Three workloads, and why each exists

`-working-set-kib 0` (the default) runs an arithmetic compute loop: a ~16-byte working set that
lives in registers and L1. It is a reasonable proxy for serial policy-inference compute and it
resists being optimised away, but it **never reaches last-level cache**. Measured on m8i, where a
single L3 is shared by all 96 cores, taking a host from 10 to 90 microVMs moved the mean 0.4%.

`-working-set-kib N` switches to a dependent-load pointer chase over an N KiB buffer. Each step's
address is the value just loaded, so loads serialise and the loop runs at the latency of whichever
cache level holds the buffer. Local measurement spans 1 ns/access at 16 KiB to 86 ns at 64 MiB.

**Use the pointer chase for anything cache-related.** A cache-affinity placement config measured
against the compute loop would report no effect whether or not the placement worked. The summary
prints a `cache-insensitive` note when the compute loop is in use, so a null result cannot be
mistaken for evidence. Full rationale and the probe plan: `analysis/CACHE-AFFINITY.md`.

`-resident-kib N` switches to an **agentic-sandbox** shape: a resident buffer of N KiB held for
the whole episode, a real `fork`+`execv`+`waitpid` per step (`-steps`), and `-write-kib` bytes
written to a guest tmpfs each step. Total, spawn, and write times are reported separately.

This exists because neither other workload forks a process, holds memory, or writes a byte, and a
real RLVR rollout does all three repeatedly. Those are different kernel paths: they take kernel
locks and touch shared structures, so they are the paths most likely to contend at density. It is
also the only workload that can answer **whether memory or cores is the binding constraint** --
every earlier measurement used a 128 MiB guest, which no real sandbox would use.

Two properties of this workload are worth knowing before reading its numbers:

- **tmpfs pages are guest memory.** The rootfs is read-only so one page-cache copy can serve the
  fleet, so writes go to a tmpfs the agent mounts on /tmp, and those pages count against `-mem`
  alongside the resident set. That is a real property of a read-only-rootfs sandbox design, not a
  harness artifact, and it is why resident and write sizes are separate parameters.
- **`Validate` refuses an undersized guest** before any hardware time is spent. Without that, the
  guest OOMs mid-episode and the host sees a microVM that stopped answering, which at density
  reads as a density ceiling rather than the sizing error it is.

The reply verbs differ (`OK`, `OKC`, `OKS`) and the runner refuses a cell where the guest's reply
kind does not match the request, so an agent that silently ignored a verb cannot produce one
workload's timings under another's label.

### Sampling: two problems, two flags

Both were found in the 2026-08-01 dataset, and they are not the same problem.

**`-target-samples` (default 450) fixes within-cell starvation.** Samples are
`density x rounds`, so a fixed round count leaves low-density cells structurally under-sampled:
at 5 rounds a 10-microVM cell yields 50 samples, and at 50 samples the nearest-rank p99 is rank
`ceil(50 x 0.99)` = 50, *which is the maximum*. Every density-10 cell in that run reported
`p99 == max`. Rounds now scale inversely with density to hit the target, and since low-density
cells are the cheap ones this costs very little wall clock. `Rounds` acts as a floor, so
enabling this can only add samples; high-density cells are unchanged.

**`-repeats` (default 5) bounds run-to-run variance.** One run per cell gives no variance
information, so no difference between configurations can be separated from noise. Five gives a
median-of-five p99 and a range to report. `-shuffle` (default on) randomises cell order within
each repeat, because consecutive cells share thermal and page-cache state and a fixed order
would make any adjacency effect inseparable from the configuration; the seed is recorded so an
ordering can be replayed.

Neither fixes **between-host** variance. `ThreadsPerCore` is fixed at launch, so an SMT
comparison necessarily crosses physical machines and stays confounded with "this particular
host" regardless of repeat count. That needs multiple hosts per condition.

### Why raw samples are kept

Percentiles cannot be recomputed from percentiles. Keeping only aggregates makes several
questions permanently unanswerable: whether a large jitter figure was one outlier or a wide
distribution, whether specific microVMs were consistently slow (which would point at
particular cores or NUMA nodes), what p95 or p999 were, and how much variance there is
between runs of the same cell.

Each row carries the cpuset the microVM was pinned to, which is what makes a slow measurement
traceable back to specific hardware, and its repeat index, without which samples from separate
repeats are indistinguishable and across-repeat variance is uncomputable from the very data
retained to compute it. At a few tens of thousands of rows the file is tens of kilobytes,
against a paid metal session to reproduce it.

## How results avoid lying

**Provenance travels with every number.** Each run records the instance type, CPU model, core
and thread counts, NUMA node count, cache mode, SMT state, kernel and Firecracker versions,
and the exact workload. A latency figure without that context cannot be defended in review,
and a reader cannot tell whether it applies to their hardware.

**Partial failure is recorded, not hidden.** If 90 microVMs were requested and 87 responded,
the cell says so and `ShortfallPercent` reports the gap. Presenting latency for 87 as though
it were 90 would understate contention and overstate achievable density, which is the exact
error the experiment exists to avoid. `Trustworthy()` gates on no errors, at most 5%
shortfall, and a non-zero sample count.

**The summary leads with caveats.** Someone skimming for a density number meets the reasons
not to trust it before the number itself. Skipped configurations are listed with the reason,
so an absent row reads as a hardware property rather than an oversight.

**Sample size is stated, not assumed.** A single-repeat run says up front that its differences
cannot be separated from noise. A cell with too few samples for a meaningful p99 is named, along
with which rank its p99 actually is. Both are printed before the results table.

**Comparisons carry their noise floor.** With repeats available, the summary reports the largest
observed p99 spread and states that smaller differences are noise rather than findings. The
B-versus-E comparison says explicitly whether the two configurations' ranges overlap, because a
delta between two overlapping ranges is not an established ordering.

**Comparisons are only made at equal density.** The config B versus config E question (one
physical core per microVM against two SMT threads) is the SMT tradeoff the sweep exists to
settle. Comparing different fleet sizes would confound density with placement strategy, so
`compareAtEqualDensity` refuses to pair cells from different density levels or to use an
untrustworthy cell.

**A workload reply without a checksum is rejected.** The guest agent returns a checksum from
its compute loop. Without that check, a guest whose loop was optimised away would present as
an implausibly fast host rather than as a bug.

## Next

**Guest workload.** Running a compute loop inside each microVM needs two things that do not
exist yet: a rootfs (no builder in `../scripts/` yet) and a guest agent to accept a task over
vsock and report timings. Until then, `measure/` covers host-side observation only, which is
enough for the warm-pool memory question but not for per-episode latency.

**Validation at density.** `launcher/` and `measure/` have unit tests but have never run
against 180 concurrent microVMs. Kernel cgroup validation, jail staging, `smaps_rollup`
parsing against real kernel output, and behaviour under load can only be checked on a metal
host.
