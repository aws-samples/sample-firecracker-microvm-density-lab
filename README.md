# Scaling RL rollouts with Firecracker on Amazon EC2 metal

Reference implementation for running dense [Firecracker](https://firecracker-microvm.github.io/)
microVM sandboxes on Amazon EC2 bare metal instances, for reinforcement learning (RL) rollouts
and agentic code execution.

Companion repo for the AWS Compute Blog post *"Scaling RL rollouts with Firecracker on Amazon
EC2 metal."*

> **Sample code.** This repository contains sample code to illustrate the architecture from the
> blog post and reproduce its measurements. It is **not thoroughly tested, secured, and optimized
> for production use.** Use it as a reference to understand the architecture and patterns, then
> review, test, and harden it for your own environment before running production workloads. No
> warranty is provided; see [LICENSE](LICENSE).

Pinned versions: **Firecracker v1.16.1**, **guest kernel 6.18**.

Supported instances: **m8i.metal-48xl** (Intel, SMT) and **m8a.metal-48xl** (AMD, no SMT).

---

## Architecture

The system packs many single-tenant Firecracker microVMs onto one EC2 metal host and pins each
to specific CPUs and NUMA-local memory, so that hundreds of RL rollout sandboxes share a host
without stepping on one another.

```
                Amazon EKS control plane
                           │
        ┌──────────────────┴───────────────────┐
        │      EC2 metal node (m8i / m8a)      │
        │                                      │
        │    kubelet ── Host Agent (DaemonSet) │
        │                  │                   │
        │    1. discover CPU topology          │
        │    2. create per-microVM cgroups     │
        │    3. orchestrate Firecracker jailer │
        │    4. manage warm pool for dispatch  │
        │                  │                   │
        │     ┌────────────┴────────────┐      │
        │     │  Warm Pool │ On-Demand  │      │
        │     │   pre-booted microVMs   │      │
        │     └──────┬───────────┬──────┘      │
        │      NUMA node 0    NUMA node 1      │
        │     (pinned vCPUs, local memory)     │
        └──────────────────────────────────────┘
```

- On each metal host, a **Host Agent** has four duties:
  1. **Discover CPU topology** — NUMA node count, physical cores, SMT state, cache mode.
  2. **Create per-microVM cgroup hierarchies** — a cpuset partition per sandbox.
  3. **Orchestrate Firecracker jailer processes** — one jailed Firecracker per microVM, with CPU
     pinning and NUMA-aware memory placement.
  4. **Manage a warm pool for dispatch** — keep a set of pre-booted microVMs ready, hand one out
     on request, and launch **on-demand** microVMs beyond the pool.
- Bare metal is required because Firecracker needs direct access to `/dev/kvm`, which is not
  available on virtualized instances.
- Placement applies **per-microVM CPU pinning**, **NUMA-aware memory placement**, and **memory
  sizing** across **NUMA nodes 0 and 1**, so each microVM's vCPUs and its memory sit on the same
  socket.
- A **Lab Sweep** component sweeps placement configurations at a range of densities and measures
  the result, so you can identify the best combination of settings for a given workload before
  running it in production.

### What is implemented in code, and what is production guidance

The diagram shows the **production shape**: an Amazon EKS cluster whose kubelet runs the Host
Agent as a DaemonSet on every metal node. This repository implements everything *inside* the
metal host box — and ships the lab that measures it — as working code. The EKS layer around it
is provided as **written guidance, not code**:

| | Delivered as |
|---|---|
| Host Agent (all four duties), placement, jailer/cgroup orchestration, warm pool, metrics | **Code** — `host-agent/`, `lab/`, run directly on the metal host (systemd service or SSM) |
| Density sweep harness, measurement, analysis | **Code** — `lab/`, `analysis/` |
| Golden host AMI (reboot-required host settings, pinned artifacts) | **Code** — `scripts/` bake flow |
| Lab infrastructure (VPC, IAM, results bucket, one metal reference host) | **Code** — `cdk/` |
| EKS cluster, Karpenter metal node pools, Host Agent as a DaemonSet | **Guidance** — see [Production deployment](#production-deployment) |

The lab needs one reference host, not a cluster: every density-critical mechanism (pinning,
partitions, jailer, warm pool) lives on a single host, and the sweep measures exactly that.
Orchestrating many such hosts is a production concern, discussed below.

## Components

| Directory | Language | Contents |
|-----------|----------|----------|
| [`cdk/`](cdk/) | Python (aws-cdk-lib) | CDK app: VPC, IAM, results bucket, and the metal reference host launched from the custom golden AMI |
| [`host-agent/`](host-agent/) | Go | The Host Agent binary: topology discovery, cgroup/jailer orchestration, warm pool, Prometheus exporter. Runs as a systemd service on the lab host; a DaemonSet in production |
| [`guest-agent/`](guest-agent/) | Rust | In-microVM runner that serves as PID 1 and speaks a small vsock text protocol |
| [`lab/`](lab/) | Go | Density sweep harness and measurement tooling (`labctl`, `labsweep`) |
| [`scripts/`](scripts/) | Shell | Host bootstrap, golden-image builder, `docker-to-rootfs` converter |
| [`examples/`](examples/) | Mixed | Example workloads and an end-to-end integration test |
| [`analysis/`](analysis/) | Python | Post-processing that turns sweep CSVs into throughput and completion tables |

### What each component is responsible for

- **`cdk/`** provisions lab infrastructure only: VPC, IAM, the results bucket, and one metal
  reference host from the golden AMI.
- **`scripts/`** bakes the host image. Settings that need a reboot (host kernel command line,
  KVM module parameters, disabling swap and KSM) belong in the AMI.
- **`host-agent/`** handles what cannot be baked. NUMA topology is only known once the process
  runs on the hardware, so topology discovery, cpuset allocation, cgroup partitions, the warm
  pool, and metrics all happen at runtime.
- **`guest-agent/`** runs inside every microVM as PID 1, so it is kept small and carries no
  language runtime.
- **`lab/`** and **`analysis/`** measure and interpret: the sweep harness runs placement
  configurations across densities, and the analysis scripts turn the raw CSVs into throughput
  and completion figures.

`host-agent/` imports the `lab/` `topology`, `placement`, `launcher`, and `firecracker` packages
through a Go `replace` directive rather than copying them, so the agent runs the same code the
lab validated.

## Instance types

Both M8 metal variants are supported, and they differ in a way that affects tuning:

| Instance | vCPUs | Physical cores | Threads per core |
|---|---|---|---|
| `m8i.metal-48xl` | 192 | 96 | 2 (SMT) |
| `m8a.metal-48xl` | 192 | 192 | 1 (no SMT) |

SMT tuning applies to `m8i` only. On `m8a` every vCPU is a full physical core, so there is no
SMT to configure and no hyperthread-sibling side channel to mitigate. The instance type is a CDK
context parameter, not a hardcoded value:

```bash
cdk deploy --all -c instanceType=m8a.metal-48xl
```

**Snapshots are not portable between instance types.** Firecracker requires that a snapshot be
resumed on hardware identical to where it was taken, and this is stricter than "same vendor":
different microarchitectures expose different CPUID features. Build snapshots per instance type,
and note that the Firecracker version must match as well. The golden AMI is likewise baked per
instance type.

## Prerequisites

- An AWS account with capacity for `.metal` instances in the target region
- Python 3.11 or newer, and the AWS CDK CLI (for `cdk/`)
- Go 1.23 or newer (host agent and lab harness)
- A Rust toolchain with the `x86_64-unknown-linux-musl` target (guest agent)
- Docker, only if converting container images to a rootfs with `scripts/docker-to-rootfs.sh`

Bare metal instances take noticeably longer to launch than virtualized ones, because Nitro wipes
and re-verifies the host between tenants. Expect minutes.

## Quick start

Two steps: bake the golden AMI once, then deploy and run the lab against it.

1. **Bake and register the golden AMI** (once per instance type). The `scripts/` flow launches a
   host, applies the reboot-required settings, installs the pinned Firecracker and guest kernel,
   builds the guest artifacts, validates the host, and captures it as an AMI
   (see [`scripts/README.md`](scripts/README.md)):
   ```bash
   cd scripts
   ./bootstrap-lab-account.sh --profile <profile>       # one-time account prep
   ./launch-lab-host.sh --profile <profile> --instance-type m8i.metal-48xl
   ./bake-lab-ami.sh --profile <profile> --instance-id <i-...>
   # Register the AMI so CDK can find it:
   aws ssm put-parameter \
     --name /firecracker-microvm-density/ami/m8i.metal-48xl \
     --type String --overwrite --value <ami-...>
   ```
   The guest agent is baked into the rootfs inside the AMI, and the host-side harness checks
   its reply format exactly. **Re-bake the AMI whenever you pull a change to `guest-agent/`**;
   a stale agent fails each cell as a parse error rather than silently reporting wrong numbers.
2. **Deploy the lab and run a sweep.** From `cdk/`, deploy the network, IAM, and the metal
   reference host from your custom AMI (see [`cdk/README.md`](cdk/README.md)); then connect
   from your terminal over SSM and sweep:
   ```bash
   cd cdk
   python -m venv .venv && source .venv/bin/activate
   pip install -r requirements.txt
   cdk deploy --all            # add -c instanceType=m8a.metal-48xl for AMD
   # Connect (the exact command is a stack output):
   aws ssm start-session --target <i-...>
   # On the host — sweep and measure (see lab/README.md):
   sudo labsweep -run-label mylab -configs A,B,D -densities 10,45,90 -out results/
   ```
   Results land in `results/` on the host and upload to
   `s3://<bucket>/benchmark_results/<run timestamp>/` automatically (`SWEEP_S3_BUCKET` is wired
   by the deploy). Read them with the `analysis/` scripts.
3. **Tear down when the results are in S3:**
   ```bash
   cdk destroy --all
   ```
   The results bucket and the golden AMI are retained by design; see
   [`cdk/README.md`](cdk/README.md#teardown) for removing them when the lab is finished.

Each component's `README.md` has the detailed commands, including building the guest agent and
converting workload containers to rootfs images
([`guest-agent/README.md`](guest-agent/README.md), [`lab/README.md`](lab/README.md)).

## Production deployment

In production, [Karpenter](https://karpenter.sh/) provisions the metal nodes and the Host Agent
runs on each as a DaemonSet. This section is guidance only; the EKS layer is not shipped as code.

- **Node pool.** Define a Karpenter `NodePool` restricted to the supported metal SKUs, backed by
  one `EC2NodeClass` per instance type whose `amiSelectorTerms` select your golden AMI (the bake
  tags it `LabAmi=true` / `BakedFrom=<instance-type>`). Taint the pool, for example
  `firecracker.aws-samples.com/sandbox=true:NoSchedule`, so ordinary workloads never land on
  sandbox hosts.
- **Base image.** Bake the production AMI on top of the EKS-optimized AMI for your cluster's
  Kubernetes version so it carries kubelet, containerd and nodeadm, then apply the `scripts/`
  provisioning. The lab AMI starts from stock Amazon Linux 2023 because the lab host never joins
  a cluster. Re-bake on each Kubernetes minor upgrade.
- **Host Agent.** Package `host-agent/` as a container image and run it as a DaemonSet on the
  pool (node selector on the pool label, toleration for the taint). The pod must be privileged
  with `hostPID`, and mount `/sys/fs/cgroup` read-write (the cpuset partition tree cannot be
  delegated to an unprivileged container), `/srv/jailer`, and `/opt/firecracker` read-only. Set
  `terminationGracePeriodSeconds` to 60s so it can drain the warm pool on scale-in. The flags in
  [`host-agent/README.md`](host-agent/README.md) apply unchanged.
- **Sizing.** Pass the configuration and density the sweep selected as DaemonSet flags. Disable
  Karpenter consolidation for the pool or set generous disruption budgets: draining a node that
  runs hundreds of sandboxes is slow, and metal instances take minutes to launch.
- **Observability.** Scrape the agent's Prometheus endpoint with your cluster's Prometheus stack
  (pod annotations or a `ServiceMonitor`).
- **Dispatch.** Consume the warm pool through the agent's dispatch interface;
  `examples/dispatch/` shows the vsock client. The off-host scheduler that routes rollout requests
  to nodes is out of scope.

## Scope

- **This is reference code.** It is intended to illustrate the architecture from the blog post
  and to reproduce its measurements. It is **not thoroughly tested, secured, and optimized for
  production use**; review, test, and harden it before running production workloads. No warranty
  is provided; see [LICENSE](LICENSE).
- **Least-privilege IAM.** The `cdk/` IAM stack scopes the instance role to what the lab
  needs (SSM Session Manager and a scoped S3 policy for sweep results). Review and
  tighten it for your environment before production use.
- **Out of scope:** managed abstractions (SageMaker, managed node groups), the off-host
  scheduler/state manager, nested virtualization, and RL algorithm internals. The EKS/Karpenter
  production deployment is described above as guidance but not shipped as code.

To report a potential security issue, see [SECURITY.md](SECURITY.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). This project has adopted the
[Amazon Open Source Code of Conduct](https://aws.github.io/code-of-conduct); see
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

This library is licensed under the MIT-0 License. See the [LICENSE](LICENSE) file.
