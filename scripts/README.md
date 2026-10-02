# Host provisioning scripts

Prepare an EC2 bare metal instance to run dense Firecracker microVM workloads, then
verify it is ready.

## Scripts

| Script | Purpose | Root | Idempotent |
|--------|---------|------|-----------|
| `provision-host.sh` | Install Firecracker and jailer, apply host tuning and security settings | yes | yes |
| `build-guest-kernel.sh` | Build an uncompressed `vmlinux` guest kernel | yes | yes |
| `build-rootfs.sh` | Build a minimal ext4 rootfs with the guest agent as /init | yes | yes |
| `launch-lab-host.sh` | Launch a metal instance for a sweep, stock or from a baked AMI | no (needs AWS creds) | n/a |
| `bake-lab-ami.sh` | Capture a provisioned host as a reusable AMI | no (needs AWS creds) | n/a |
| `fetch-swe-agent-traj.sh` | Fetch the pinned SWE-agent trajectory the episode campaign replayed | no | yes |
| `toggle-smt.sh` | Toggle SMT on a running host and verify it took effect | yes (to change) | `status` needs none |
| `validate-host.sh` | Verify the host is ready. Read-only | no | n/a |
| `lib/common.sh` | Shared helpers and pinned versions. Sourced, not executed | n/a | n/a |

## Order of operations

```bash
sudo ./provision-host.sh          # install and tune
sudo ./build-guest-kernel.sh      # build /opt/firecracker/vmlinux
sudo reboot                       # apply kernel args and module parameters
./validate-host.sh                # confirm readiness
```

Preview any change without applying it:

```bash
./provision-host.sh --dry-run     # no root required
```

## Bake once, launch many times

The sequence above is ~40 minutes of work on a host that bills the same whether it is
compiling or measuring, and every session before 2026-08-07 paid for it again. Capture it
instead:

```bash
./bake-lab-ami.sh --profile <p> --instance-id i-abc123 --dry-run   # check what would be captured
./bake-lab-ami.sh --profile <p> --instance-id i-abc123
```

Then subsequent sessions launch a host that is ready on arrival, with no provisioning and
no reboot:

```bash
./launch-lab-host.sh --profile <p> --instance-type m8a.metal-48xl --golden
```

`--golden` resolves the newest available AMI tagged `LabAmi=true` whose `BakedFrom` tag
matches the requested instance type, so a saved command line keeps working after a rebake.
`--ami <id>` pins a specific one.

Two things to know:

- **One image per SKU.** The filter matches on `BakedFrom` rather than just the lab tag,
  because m8a and m8i differ in vendor, SMT capability, and NUMA shape. An m8i image booted
  on m8a would come up looking healthy and measure the wrong machine.
- **Rebake when a pinned version moves.** `FIRECRACKER_VERSION` and
  `GUEST_KERNEL_VERSION` are recorded as tags on the image. A Firecracker change invalidates
  snapshots fleet-wide, so the tag is what makes a stale image identifiable without booting it.

`bake-lab-ami.sh` runs `validate-host.sh` over SSM first and refuses to capture a host that
fails, because an AMI baked from a half-provisioned host is indistinguishable from a good one
until a sweep produces wrong numbers. Warnings are allowed through with a notice — they will
appear on every instance launched from the image.

The image carries the guest artifacts too (`vmlinux`, `rootfs.ext4`, and any converted task
rootfs images), which is what removes the repeated Docker conversions. Firecracker **snapshots**
are deliberately not part of this: they only resume on identical hardware and Firecracker
version, so see the snapshot portability section below.

## Benchmark inputs are pinned, not vendored

The task files and the recorded trajectory are upstream's artifacts and are deliberately not
committed here — the Terminal-Bench tasks carry the benchmark's canary string, which exists so
benchmark data can be detected in training corpora. They are fetched at a pinned revision instead,
which keeps a conversion reproducible without putting that data in one more place.

| Input | Fetched by | Pin |
|---|---|---|
| Terminal-Bench task dirs | `fetch-tbench-task.sh` | `harbor-framework/terminal-bench-1` @ `d28711d0` |
| SWE-agent trajectory | `fetch-swe-agent-traj.sh` | `SWE-agent/SWE-agent` @ `3ea751c0` |
| SWE-bench eval image | `docker-to-rootfs.sh --image` | digest in `analysis/TASK-THROUGHPUT-RESULTS.md` |

Each pin exists because upstream moved under this project at least once:

- **Terminal-Bench split into two repos.** `laude-institute/terminal-bench` redirects to
  `harbor-framework/terminal-bench-1`, which holds `original-tasks/`. The similarly named
  `harbor-framework/terminal-bench` is the successor benchmark and has no `original-tasks/` — aiming
  at it returns 404 for every task. It also removed `hello-world` on 2026-07-21, so the measured
  task now exists only at the pinned revision.
- **Eight trajectories share one instance ID.** Upstream ships one demonstration per agent
  configuration for `marshmallow-code__marshmallow-1867`, and they are not interchangeable: four use
  an `edit` syntax `traj-to-episode.py` does not read, three convert to 11 turns, and one converts
  to the 14 the campaign measured. `fetch-swe-agent-traj.sh` re-checks that turn count after
  fetching and fails if it moved, because a silently-revised demonstration would otherwise surface
  as a wrong number rather than an error.
- **`:latest` on the SWE-bench image is mutable** and has already moved once, so the digest is
  recorded alongside the tag.

## What is baked into the AMI, and what is not

The split is not arbitrary. Anything that requires a reboot goes into the image, so
that a newly launched instance is immediately usable rather than needing a
boot-configure-reboot cycle.

**Baked in (needs a reboot):**

- Host kernel command line: `quiet loglevel=1`. Console logging measurably slows
  operations; upstream measured snapshot restore going from 3ms to 8.5ms with console
  logging enabled.
- KVM module parameters: `nx_huge_pages=never` where safe, and
  `min_timer_period_us` to bound `kvm-pit` overhead.
- Swap disabled, KSM disabled.
- File descriptor and PID limits raised for 150 or more VMM processes.

**Left to the host agent at runtime (cannot be baked):**

- **NUMA topology discovery.** Actual NPS mode on AMD or SNC configuration on Intel is
  not knowable until a process is running on the target hardware.
- **cpuset allocation and partition setup.** Depends on discovered topology. Note that
  `cpuset.cpus.partition` is owned by the parent cgroup and is not delegatable, so the
  agent must write it from the parent. The jailer's `--cgroup` flag cannot set it.
- **SMT configuration.** Runtime-togglable through
  `/sys/devices/system/cpu/smt/control`, so it is a policy decision rather than an
  image property.
- **`kvm-pit` thread placement.** The kernel creates these threads after guest start,
  by which point Firecracker has dropped privileges, so an external agent must move
  them into each microVM's cgroup.
- **Warm pool prefill.** Depends on core count and configured pool size.

## Pinned versions

Set in `lib/common.sh` and overridable by environment variable:

| Variable | Default | Notes |
|----------|---------|-------|
| `FIRECRACKER_VERSION` | `v1.16.1` | **Changing this invalidates every existing snapshot.** Snapshots only resume on the Firecracker version that produced them |
| `GUEST_KERNEL_VERSION` | `6.18` | Supported from Firecracker v1.16.1 through 2028-06-01. Guest v6.1 support ends 2026-09-02 |
| `INSTALL_DIR` | `/usr/local/bin` | Where `firecracker` and `jailer` are installed |
| `ARTIFACT_DIR` | `/opt/firecracker` | Guest kernel and related artifacts |
| `JAILER_CHROOT_BASE` | `/srv/jailer` | Must not be writable by unprivileged users |

The Firecracker download is checksum-verified against the published
`.sha256.txt` before it is unpacked.

## Snapshot portability

Snapshots are less portable than is commonly assumed. Firecracker requires that a
snapshot resume on a hardware and software configuration identical to the one that
produced it, and this is stricter than "same CPU vendor": a snapshot taken on
`m6i.metal` cannot restore on `m5n.metal`, because different microarchitectures expose
different CPUID features.

Practical consequences:

- Build snapshots per instance type. `m8i` and `m8a` need separate sets.
- The Firecracker version must also match, so upgrading Firecracker means rebuilding
  snapshots fleet-wide rather than performing a rolling binary swap.
- This is why `bake-lab-ami.sh` captures guest *artifacts* but not snapshots. A rootfs and a
  `vmlinux` are portable across SKUs; a snapshot is not, so baking one into a shared image
  would produce a restore failure at density rather than at launch.

## SMT is toggled at runtime, on one host

**EC2 CpuOptions do not work on bare metal.** `RunInstances` returns
`UnsupportedOperation: The m8i.metal-48xl instance type does not support specifying CpuOptions`,
and `describe-instance-types` shows `ValidThreadsPerCore` as null for metal SKUs against `[1,2]`
for the virtualized `m8i.48xlarge`. CpuOptions is a Nitro hypervisor feature, and metal has no
hypervisor to enforce a thread count.

On metal, the kernel is the only mechanism, and it works on a *running* host:

```bash
./launch-lab-host.sh --profile <p>                     # one host, as launched: 192 vCPUs
labsweep -run-label smt-on  -requested-threads-per-core 2 ...

sudo ./toggle-smt.sh off                               # 96 online CPUs, smt/active=0
labsweep -run-label smt-off -requested-threads-per-core 1 ...
```

**One host measuring both states is better than two hosts**, because an SMT difference cannot be
host variance. `toggle-smt.sh` reads the state back after writing and fails if the kernel refused
the transition, since a sweep labelled `smt-off` that silently ran with SMT on would be worse than
no data. It also handles the two states that are not toggles: `notsupported` on AMD metal, where
there is nothing to disable, and `forceoff` from a `nosmt` boot argument, which cannot be undone
without a reboot.

Caveat worth stating in any write-up: offlining siblings at runtime is not identical to never
having booted with them. The kernel rebuilds scheduler domains, but memory was allocated and
interrupts routed while all CPUs were present.

Pass `-run-label` and `-requested-threads-per-core` to `labsweep` either way. That makes each
dataset self-describing and lets the sweep report a mismatch between the requested and observed
thread count as a leading caveat.

## Instance types

| Instance | vCPUs | Physical cores | Threads per core | SMT tuning |
|---|---|---|---|---|
| `m8i.metal-48xl` | 192 | 96 | 2 | Applies |
| `m8a.metal-48xl` | 192 | 192 | 1 | Not applicable |

On `m8a` metal, `validate-host.sh` reports SMT as `notsupported`. That is expected, not
a failure: every vCPU is a full physical core, so there is no SMT to configure and no
hyperthread-sibling side channel to mitigate.

## Exit codes

`validate-host.sh` distinguishes between conditions that block work and conditions
worth noting, so it can be used in automation:

| Code | Meaning |
|------|---------|
| 0 | All checks passed |
| 1 | One or more required checks failed. Host is not usable |
| 2 | Passed with warnings. Host is usable, but review before recording measurements |

## Validation status

Verified end to end on an `m8i.metal-48xl` running Amazon Linux 2023 (kernel
6.1.176-223.369.amzn2023) on 2026-07-30: provision, guest kernel build, reboot, and
`validate-host.sh` returning 0 with no warnings.

Verified on both `m8a.metal-48xl` and `m8i.metal-48xl` on 2026-08-07, each provisioned by
`setup-lab-host.sh` from stock Amazon Linux 2023: 21 provisioning checks, reboot, and
`validate-host.sh` returning 0 with no warnings on each. Both were then captured by
`bake-lab-ami.sh`, so that path is now proven against hardware too.

Not yet verified on a Debian or Ubuntu host.

Measured on that host, for reference:

| Step | Result |
|------|--------|
| Guest kernel v6.18 build | 42 seconds wall clock, 22m38s CPU across 192 jobs, 27 MB `vmlinux` |
| NUMA topology | **3 nodes on a single socket** (32 physical cores each). Whether this reflects compute tiles or Sub-NUMA Clustering is not yet established |
| `itlb_multihit` | `Not affected`, so `nx_huge_pages=never` is applied |

## Distro notes

Behaviour worth knowing about on Amazon Linux 2023, each of which caused a real failure
during validation:

- **`curl` must not be installed by name.** AL2023 ships `curl-minimal`, which already
  provides `/usr/bin/curl`. Requesting the `curl` package triggers an unresolvable
  conflict and aborts the whole transaction.
- **`nftables` is not preinstalled.** Without it the IMDS drop rule cannot be applied,
  so `provision-host.sh` installs it and treats a missing `nft` as fatal rather than
  producing an AMI whose guests can reach instance metadata.
- **KSM is not compiled in** (`CONFIG_KSM` unset), so `/sys/kernel/mm/ksm` is absent.
  This is the desired state, not a problem: KSM cannot be enabled.
- **`limits.d` does not apply to systemd services.** AL2023 sets
  `DefaultLimitNOFILE=65535`, which would cap the host agent regardless of
  `/etc/security/limits.d`. Systemd drop-ins are written for this reason.

## Known gaps

- `provision-host.sh` does not configure network or block device rate limiters. Those
  are per-microVM settings applied by the host agent at VM creation.
- Hugepage reservation is not configured, and the sweep does not record the host's page-size
  state. Guests are 4 KiB-backed as long as transparent hugepages are not `always`, which holds
  on the Amazon Linux 2023 default (`madvise`). Check
  `/sys/kernel/mm/transparent_hugepage/enabled` before comparing results from another host image.
- Not tested on a Debian or Ubuntu host. `setup-lab-host.sh` installs dependencies with `dnf`, so
  it needs an apt path before it would run on either.
