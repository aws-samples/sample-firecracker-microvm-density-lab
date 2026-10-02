# Host Agent

The **Host Agent** is the per-node agent binary from the blog post's architecture
diagram. In that diagram an Amazon EKS cluster runs on EC2 metal nodes, and kubelet runs
one Host Agent on each metal node as a DaemonSet. This directory is its Go implementation.

**In this repo the agent runs directly on the metal lab host** — as a systemd service
([`systemd/host-agent.service`](systemd/host-agent.service)) or started by hand over SSM.
Running it as a privileged DaemonSet on EKS with Karpenter metal node pools is the
production deployment, described (as guidance, not code) in the repo root
[`README.md`](../README.md#production-deployment).

It reuses the validated `lab/` packages by importing them — it does **not** fork them. A
`replace` directive in `go.mod` points the shared module at `../lab`, so the agent compiles
against the exact `topology`, `placement`, `launcher`, and `firecracker` code the density
experiment used.

Pinned versions: **Firecracker v1.16.1**, **guest kernel 6.18**.
Supported instances: **m8i.metal-48xl** (Intel, SMT) and **m8a.metal-48xl** (AMD, no SMT).

## The four duties, and where each lives

The diagram assigns the Host Agent four duties. Each maps to one internal package:

| Diagram duty | Package | What it does |
|---|---|---|
| 1. Discover CPU topology | `internal/hostinfo` | Wraps `lab/topology.Discover`; logs NUMA node count, physical cores, SMT state, and cache mode at startup. |
| 2. Create per-microVM cgroup hierarchies | `internal/orchestrator` | Builds the parent hierarchy via `lab/launcher` and a child cgroup per microVM. |
| 3. Orchestrate Firecracker jailer processes | `internal/orchestrator` | Prepares a jail and launches a jailed Firecracker per microVM via `lab/launcher` + `lab/firecracker`, with CPU pinning and NUMA-aware memory. |
| 4. Manage a warm pool for dispatch | `internal/warmpool` | Keeps N pre-booted microVMs ready, hands one out on `Acquire`, refills asynchronously, and launches on demand beyond the pool. |

Observability for all of it is `internal/metrics` (Prometheus), and `cmd/host-agent`
wires the pieces together with flags and graceful shutdown.

## How placement, pinning, and NUMA work

On startup the agent discovers the node topology, then asks `lab/placement.Build` for a
plan given a **configuration** (A–F) and a **density** (number of microVM slots):

- Each `placement.VM` carries a `Node` (its NUMA node) and a `VCPUSet` (its `cpuset.cpus`).
- The agent writes each microVM's `cpuset.mems` to its `Node`, so **guest memory allocates
  local to the vCPU** — NUMA-aware memory placement across nodes 0 and 1.
- Guest memory size (`-mem-mib`) is applied per microVM via `PutMachineConfig`; combined
  with per-node `cpuset.mems` this is the memory-sizing-across-NUMA-nodes behaviour the
  diagram calls out.

Configurations **C, E, and F require SMT**. On an AMD `m8a.metal-48xl` (one thread per
core) the agent refuses those configs at startup with a clear error, mirroring
`placement.Config.RequiresSMT()`.

### The non-delegatable partition rule

`cpuset.cpus.partition` is owned by the **parent** cgroup and is not delegatable. So the
agent, not the jailer, establishes isolation:

1. `CreateHierarchy(name, ParentCPUSet, mems)` — parent owns the union of all CPUs.
2. `SetExclusive(ParentCPUSet)` — declare which CPUs may back child partitions.
3. `SetPartition(root)` — make the parent a partition root.
4. Per microVM: `CreateChild(id, VCPUSet, mems)`, then optionally
   `SetChildPartition(id, isolated)` to remove the scheduler from the vCPU thread.
5. The jailer runs with `--parent-cgroup <name>/<id>` and **no** `--cgroup`, so it *moves*
   Firecracker into the pre-created child cgroup rather than creating its own. This is what
   keeps partition ownership on the parent.

Steps 2–4 are best-effort: if the kernel rejects exclusive CPUs (for example on a host
where another controller holds them), the agent logs a warning and falls back to plain
`cpuset` pinning, which is still meaningful. This tolerance matches `lab/sweep`.

## Boot sequence per microVM

Reproduces `lab/sweep`'s validated sequence:

1. `CreateChild` cgroup from the parent side.
2. `PrepareJail` — stage the guest kernel (`vmlinux`) and read-only `rootfs.ext4` into the
   jail (hard-linked, so one page-cache copy serves the fleet).
3. `Launch` the jailer (daemonized, new PID namespace).
4. `WaitReady` on the API socket.
5. `PutMachineConfig` (vCPU count, NUMA-aware memory) → `PutBootSource` (kernel 6.18,
   serial disabled, `panic=1`) → `PutDrive` (read-only rootfs) → `PutVsock` → `Start`.
6. Poll the guest agent over vsock (`CONNECT 5000` then `PING`) until it answers `OK`.

The guest-agent vsock protocol (`PING`, `RUN`, `RUN_TASK`, `RUN_EPISODE`, …) is reachable
via `orchestrator.AgentRequest` for dispatching work to an acquired microVM.

## Warm pool

`internal/warmpool` keeps `-warm-pool-size` microVMs pre-booted and ready. `Acquire`:

- hands out an idle warm microVM instantly when one exists (fast dispatch path), and
- launches **on demand** beyond the pool when it is empty.

A refiller tops the pool back to target in the background so refills never block dispatch.
`Release` tears a used microVM down (guest state is dirty) and triggers a refill, preserving
the clean-slate guarantee each dispatch expects. The manager is thread-safe.

## Metrics

Served at `-metrics-addr` (`/metrics`), plus `/healthz`. The default bind is
**`127.0.0.1:9090`** — a host running untrusted workloads should not expose
unauthenticated listeners it does not need. A production scraper overrides it
deliberately (`-metrics-addr=:9090`) behind a NetworkPolicy or equivalent. Exposed series:

- `hostagent_warm_pool_size`, `hostagent_warm_pool_target`
- `hostagent_microvms_in_use`, `hostagent_microvms_on_demand`
- `hostagent_launches_total{source}`, `hostagent_launch_failures_total`
- `hostagent_acquire_total{outcome}`, `hostagent_warm_pool_refills_total`
- `hostagent_launch_latency_seconds` (histogram)
- `hostagent_microvms_by_node{node}`
- `hostagent_topology_info{instance_type,numa_nodes,physical_cores,threads_per_core,has_smt,cache_mode,config}`

## Build and run

```sh
# From this directory. The replace directive resolves lab/ locally.
go mod tidy
go build ./...
go vet ./...
go test ./internal/warmpool/   # unit tests that need neither root nor hardware

# Run on a prepared metal host (needs root, cgroup v2, jailer/firecracker, baked artifacts):
sudo ./host-agent \
  -config=D -density=96 -warm-pool-size=16 \
  -kernel=/opt/firecracker/vmlinux -rootfs=/opt/firecracker/rootfs.ext4 \
  -instance-type=m8i.metal-48xl -metrics-addr=:9090
```

Host prerequisites (see `../scripts/`): cgroup v2 with `cpuset`, `cpu`, and `memory`
controllers; the jailer and firecracker binaries; and a baked kernel + rootfs. The agent
requires root because it writes the host cpuset partition tree and kills jailed PIDs.

## Running as a service

[`systemd/host-agent.service`](systemd/host-agent.service) runs the agent as a root
service on the lab host. The AMI bake stages the binary and unit; enable it with:

```sh
sudo cp systemd/host-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now host-agent
```

It reads `SWEEP_S3_BUCKET` and other lab wiring from `/etc/firecracker-lab/env`
(written by the CDK LabHost stack's user data). Note that density sweeps
(`lab/cmd/labsweep`) launch their own microVMs — stop the agent before a sweep so the
two do not contend for the cgroup partition tree:

```sh
sudo systemctl stop host-agent
```

In production the same binary runs as a privileged, hostPID DaemonSet that mounts
`/sys/fs/cgroup` and `/srv/jailer` on every node of an EKS Karpenter metal node pool —
see the repo root [`README.md`](../README.md#production-deployment) for that discussion.
The agent itself is deployment-agnostic: everything it needs is flags, the artifacts
under `/opt/firecracker`, and root on a cgroup v2 host.

## Relationship to `lab/`

`lab/` is the source of truth for topology, placement, jailer/cgroup, and the Firecracker
API client. The Host Agent imports those packages unchanged; the Lab Sweep component
(`lab/cmd/labsweep`) is what identifies the best `-config`/pinning combination for a given
workload, which an operator then feeds to this agent.
