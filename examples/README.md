# Examples: the full dispatch loop

This directory walks the whole loop from the blog post's architecture diagram end to end: a
**Host Agent** on an EC2 metal host discovers CPU topology, creates per-microVM cgroup
hierarchies, orchestrates jailed Firecracker processes, and manages a **warm pool**; work is
then **dispatched** to a pinned microVM over the guest agent's vsock protocol and a result is
read back. A **Lab Sweep** component finds the best placement and pinning for a given workload.
(In the diagram's production shape the Host Agent runs as an EKS DaemonSet; here it runs
directly on the lab host — see the repo root README for what is code versus guidance.)

Everything here uses the real components in this repository unchanged:

- infrastructure from [`../cdk/`](../cdk/) (Python CDK)
- the golden image built by [`../scripts/`](../scripts/) (shell)
- the [`../host-agent/`](../host-agent/) agent binary (Go)
- the [`../guest-agent/`](../guest-agent/) that runs as PID 1 inside each microVM (Rust)
- the [`../lab/`](../lab/) sweep and placement tooling (Go)

Pinned versions: **Firecracker v1.16.1**, **guest kernel 6.18**.
Supported instances: **m8i.metal-48xl** (Intel, SMT) and **m8a.metal-48xl** (AMD, no SMT).

## Contents

| Path | What it is |
|---|---|
| `dispatch/` | A small Go client that speaks the guest-agent vsock protocol: it sends `RUN_SANDBOX` (or any verb) to a running microVM and prints the parsed reply. Its own module. |
| `dispatch/e2e_test.go` | An end-to-end integration test, **gated to run only on a provisioned metal host** with a live microVM. Skips otherwise. |
| `workloads/synthetic-sandbox.md` | A dependency-free, parameterised sandbox workload (`RUN_SANDBOX`). |
| `workloads/real-tasks.md` | How to convert a real SWE-bench instance or Terminal-Bench task into a bootable rootfs for `RUN_TASK` / `RUN_EPISODE`. |

## Prerequisites

- An AWS account with capacity for `.metal` instances in your target region.
- Python 3.11+ and the AWS CDK CLI (for `../cdk/`).
- Go 1.23+ (for `../host-agent/`, `../lab/`, and `dispatch/`).
- A Rust toolchain with the `x86_64-unknown-linux-musl` target (for `../guest-agent/`).
- Docker, only if converting container images to rootfs.

Bare metal instances take minutes to launch — Nitro wipes and re-verifies the host between
tenants.

---

## Step 1 — Deploy the infrastructure with CDK

The CDK app provisions the VPC, IAM, instance profile, the results bucket, and the metal
reference host launched from the custom golden AMI (baked and registered first — see
[`../scripts/README.md`](../scripts/README.md) and the repo root quick start). It provisions
infrastructure only; nothing density-critical lives here.

```sh
cd ../cdk
python -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt

# Instance type is a context parameter, not hardcoded. Pick the metal variant you want.
cdk deploy --all -c instanceType=m8i.metal-48xl      # Intel, SMT
# or
cdk deploy --all -c instanceType=m8a.metal-48xl      # AMD, no SMT
```

See [`../cdk/README.md`](../cdk/README.md) for the stacks and parameters.

## Step 2 — Build the golden image with `scripts/`

Several density-critical settings need a reboot, so they are baked into the AMI rather than set at
first boot: host kernel command line, KVM module parameters, hugepage reservation, and disabling
swap and KSM. The scripts also build the guest kernel and a rootfs containing the guest agent.

```sh
cd ../scripts

# Build the guest kernel (6.18) and validate the host once it is up.
./build-guest-kernel.sh
./validate-host.sh                 # confirms cgroup v2, jailer, firecracker, artifacts

# Provision a running host in place (host kernel args, hugepages, KSM/swap off, etc.).
sudo ./provision-host.sh

# Or bake a reusable AMI through the pipeline.
./bake-lab-ami.sh
```

See [`../scripts/README.md`](../scripts/README.md) for the full list. To turn a real eval task
into the guest rootfs, see [`workloads/real-tasks.md`](workloads/real-tasks.md).

## Step 3 — Inspect topology and pick a placement (Lab Sweep)

Before running the agent, see what the host looks like and which placement configs it can run.
`labctl topology` is read-only and reports NUMA layout, physical cores, SMT state, cache mode,
and which sweep configs (A–F) are runnable on this host (configs C, E, and F require SMT and are
refused on an `m8a`).

```sh
cd ../lab
go run ./cmd/labctl topology
go run ./cmd/labctl plan -config D -density 96      # preview the placement plan for config D
```

The **Lab Sweep** identifies the best config and pinning for your workload by launching microVMs
across the config × density matrix and measuring latency distributions:

```sh
# Needs root and a prepared host.
sudo go run ./cmd/labsweep -dry-run                          # plan the matrix, launch nothing
sudo go run ./cmd/labsweep -configs B,D -densities 10,45,90 \
  -resident-kib 65536 -steps 20 -write-kib 256 \
  -out results/                                              # run and write JSON, CSV, a summary
```

The winning `-config` is what you pass to the host agent next.

## Step 4 — Start the Host Agent

On the prepared metal host, start the agent binary (or use the systemd unit in
[`../host-agent/systemd/`](../host-agent/systemd/host-agent.service)). It discovers topology,
builds the placement plan, establishes the parent cgroup partition tree, and boots a warm pool
of pinned microVMs. It needs root (it writes the host cpuset partition tree and manages jailed
PIDs).

```sh
cd ../host-agent
go build ./...

sudo ./host-agent \
  -config=D -density=96 -warm-pool-size=16 \
  -kernel=/opt/firecracker/vmlinux \
  -rootfs=/opt/firecracker/rootfs.ext4 \
  -instance-type=m8i.metal-48xl \
  -metrics-addr=:9090
```

Key flags (full list in [`../host-agent/README.md`](../host-agent/README.md)):

| Flag | Meaning |
|---|---|
| `-config` | Placement configuration A–F from the sweep. C/E/F require SMT. |
| `-density` | Number of microVM placement slots to plan. |
| `-warm-pool-size` | Pre-booted microVMs kept ready for instant dispatch. |
| `-mem-mib`, `-vcpus` | Guest memory (NUMA-aware via `cpuset.mems`) and vCPUs per microVM. |
| `-metrics-addr` | Prometheus endpoint (`/metrics`) and `/healthz`. |

In production the same binary runs as a privileged, hostPID DaemonSet on an EKS Karpenter
metal node pool — the diagram's "kubelet runs Host Agent as a DaemonSet." See the repo root
[`README.md`](../README.md#production-deployment) for that guidance.

## Step 5 — Dispatch a sample RL-episode workload

Once the agent has booted a warm pool, each microVM has a vsock Unix socket at
`<chroot>/v.sock` (the agent records this as `MicroVM.VsockUDS`, e.g.
`/srv/jailer/firecracker/vm0/root/v.sock`). The `dispatch` example connects to that socket, speaks
the guest-agent protocol, and prints the parsed reply.

```sh
cd ../examples
go build ./dispatch

# Liveness check.
go run ./dispatch -vsock /srv/jailer/firecracker/vm0/root/v.sock -workload ping

# Synthetic sandbox turn (fork/exec churn + resident memory + writes).
go run ./dispatch -vsock /srv/jailer/firecracker/vm0/root/v.sock \
  -workload sandbox -resident-kib 65536 -steps 20 -write-kib 256

# A full replayed RL episode (needs a rootfs converted with an episode script; see workloads/).
go run ./dispatch -vsock /srv/jailer/firecracker/vm0/root/v.sock -workload episode
```

Example output for the sandbox workload:

```
request:  RUN_SANDBOX 65536 20 256
reply:    OKS 65536 20 256 41230 3110 8894 20 8241730 67120
parsed:
  resident_kib 65536
  steps        20
  write_kib    256
  total_us     41230  (41.230 ms)
  spawn_us     3110  (3.110 ms)
  write_us     8894  (8.894 ms)
  spawns_ok    20
  checksum     8241730
  peak_rss_kib 67120
```

### A note on `Acquire`

The Host Agent keeps its warm pool **in-process** — `warmpool.Pool.Acquire` hands a microVM to
callers inside the same binary, and the agent does not expose that pool over a network or
Unix-socket RPC. There is no wire API for a separate process to acquire a microVM. Rather than
invent one, `dispatch/` talks the exact protocol the agent itself uses
(`orchestrator.AgentRequest`) against a microVM's vsock socket. If you embed the warm pool in your
own binary, call `Acquire` directly and then use the same protocol on the returned
`MicroVM.VsockUDS`.

## Step 6 — Read the metrics

The agent serves Prometheus at `-metrics-addr`:

```sh
curl -s localhost:9090/metrics | grep hostagent_
curl -s localhost:9090/healthz
```

Notable series (full list in [`../host-agent/README.md`](../host-agent/README.md)):

- `hostagent_warm_pool_size`, `hostagent_warm_pool_target`
- `hostagent_microvms_in_use`, `hostagent_microvms_on_demand`
- `hostagent_launches_total{source}`, `hostagent_launch_failures_total`
- `hostagent_acquire_total{outcome}` — `warm`, `ondemand`, or `error`
- `hostagent_launch_latency_seconds` (histogram)
- `hostagent_microvms_by_node{node}` — NUMA-node distribution
- `hostagent_topology_info{instance_type,numa_nodes,physical_cores,threads_per_core,has_smt,cache_mode,config}`

## End-to-end test

`dispatch/e2e_test.go` exercises Steps 5 in a test: PING, then `RUN_SANDBOX` with field
validation. It is **gated** and only runs on a provisioned metal host with a live microVM,
skipping everywhere else so `go test ./...` stays green on a laptop.

```sh
cd examples/dispatch
export FC_E2E_VSOCK=/srv/jailer/firecracker/vm0/root/v.sock
go test -run TestE2E -v ./...
```

Without `FC_E2E_VSOCK` (or if the socket does not exist) the test prints a skip message listing
the preconditions and passes. See the comment at the top of `e2e_test.go` for the full setup.
