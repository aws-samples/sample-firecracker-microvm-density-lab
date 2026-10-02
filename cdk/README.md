# CDK: lab infrastructure on EC2 metal

Python AWS CDK app that provisions the infrastructure the Firecracker microVM
density lab runs on: a VPC, the scoped IAM role and results bucket, and **one
EC2 metal reference host launched from the custom golden AMI you bake in step 1
of the quick start**. This is the deployment layer only. Nothing
density-critical lives here: core pinning, NUMA placement, jailer configuration,
and cgroup partition setup are in `../scripts/` (baked into the AMI) and
`../host-agent/` (applied at runtime). See the repo root `README.md` for the
division of responsibilities.

Production deployment on EKS with Karpenter metal node pools is discussed in
the repo root `README.md` and is intentionally **not** modeled here.

## The two-step flow

The CDK app assumes the golden AMI **already exists in your account**. It never
falls back to a stock AMI: a stock Amazon Linux node has no Firecracker, no
jailer, and no guest kernel — it would deploy cleanly and fail only at sweep
time.

**Step 1 — bake and register the AMI** (once per instance type, see
[`../scripts/README.md`](../scripts/README.md)):

```bash
# Bake with the scripts/ flow (launch, provision, validate, capture):
../scripts/bake-lab-ami.sh --profile <profile> --instance-id <i-...>

# Register the resulting AMI ID for CDK to find:
aws ssm put-parameter \
  --name /firecracker-microvm-density/ami/m8i.metal-48xl \
  --type String --overwrite \
  --value ami-0123456789abcdef0
```

The parameter path embeds the instance type because **AMIs are not portable
between SKUs**: kernel settings and Firecracker snapshots baked on `m8i` are
wrong on `m8a`. Bake and register one AMI per SKU you plan to run.

**Step 2 — deploy the lab:**

```bash
cdk deploy --all                                 # m8i.metal-48xl (Intel, SMT)
cdk deploy --all -c instanceType=m8a.metal-48xl  # AMD, no SMT
```

If the SSM parameter for the selected instance type does not exist, the deploy
**fails fast** with a parameter-not-found error pointing back at step 1. To
bypass the parameter entirely, pass the AMI ID explicitly (requires a concrete
region, e.g. `CDK_DEFAULT_REGION`):

```bash
cdk deploy --all -c amiId=ami-0123456789abcdef0
```

## What it provisions

| Stack | Contents |
|-------|----------|
| `FcDensity-Network` | VPC across 2 AZs, public/private subnets, S3 gateway endpoint, and interface endpoints (SSM, CloudWatch) so the private metal host is reachable over Session Manager without any inbound path. |
| `FcDensity-Iam` | The instance role + profile the lab host runs under: SSM Session Manager access and a **scoped** S3 policy for density-sweep results, plus the results bucket itself. |
| `FcDensity-LabHost` | One `.metal` reference host from the custom AMI: private subnet, no-ingress security group, **IMDSv2 required**, encrypted 100 GiB gp3 root volume, and boot-time wiring of `SWEEP_S3_BUCKET`. |

The `FcDensity-LabHost` outputs include the exact command to connect:

```bash
aws ssm start-session --target i-...
```

From that session, run the sweep exactly as [`../lab/README.md`](../lab/README.md)
documents. `SWEEP_S3_BUCKET` is already set (user data writes
`/etc/firecracker-lab/env` for the host-agent systemd unit and
`/etc/profile.d/firecracker-lab.sh` for interactive sessions), so results
upload to `s3://<bucket>/benchmark_results/<run timestamp>/` on completion.

## Instance type is a context parameter

Both M8 metal variants are supported and the app reads the instance type from
CDK context, exactly as the repo root `README.md` documents:

```bash
cdk deploy --all                                 # default: m8i.metal-48xl
cdk deploy --all -c instanceType=m8a.metal-48xl  # AMD, no SMT
```

Only `m8i.metal-48xl` and `m8a.metal-48xl` are accepted; any other value fails
synth with a clear error, since Firecracker requires `/dev/kvm` and that needs a
`.metal` instance.

## The scoped S3 policy

The sweep harness (`../lab/cmd/labsweep`) uploads results to
`s3://$SWEEP_S3_BUCKET/benchmark_results/<run timestamp>/` on completion. The
inline policy mirrors `../lab/LAB-RESOURCES.md` and is deliberately narrow:

- `s3:GetObject` on the whole bucket — fetch staged binaries.
- `s3:PutObject` **confined to `benchmark_results/*`** — a sweep can publish
  results but cannot overwrite its own inputs.
- `s3:ListBucket` on the bucket — enumerate a prefix from the CLI.
- **No `s3:DeleteObject`** — results are append-only from the host's perspective.

Provide an existing bucket with `-c transferBucketName=<name>`; otherwise the app
creates one (encrypted, versioned, TLS-enforced, public access blocked).

The role deliberately carries no ECR permissions. Every artifact on the
critical path stages through the S3 bucket; if you convert container images to
rootfs images *on the host* against a private ECR registry, add
`AmazonEC2ContainerRegistryReadOnly` back in `stacks/iam_stack.py`.

## Teardown

```bash
cdk destroy --all
```

This terminates the metal host (its root volume is deleted with it) and removes
the VPC and IAM resources. Two things are retained **by design**:

- **The results bucket.** Benchmark results are the product of the lab, so the
  bucket survives `cdk destroy`. When you have harvested the results:
  ```bash
  aws s3 rm s3://<bucket> --recursive
  aws s3 rb s3://<bucket>
  ```
  (The bucket is versioned; use the console or `delete-objects` with versions
  if `s3 rb` complains.)
- **The golden AMI and its EBS snapshot** — created in step 1, outside
  CloudFormation. Deregistering the AMI does **not** delete its snapshot;
  remove both, as [`../lab/LAB-RESOURCES.md`](../lab/LAB-RESOURCES.md)
  documents:
  ```bash
  aws ec2 deregister-image --image-id <ami-...>
  aws ec2 describe-snapshots --owner-ids self \
    --filters Name=tag:LabAmi,Values=true \
    --query 'Snapshots[].SnapshotId' --output text \
    | xargs -n1 aws ec2 delete-snapshot --snapshot-id
  ```

Bare metal instances bill by the hour and are expensive; destroy the LabHost
stack as soon as the sweep results are in S3.

## Prerequisites

- Python 3.11 or newer (the repo standard; the app itself imports under 3.9+).
- Node.js and the AWS CDK CLI (`npm i -g aws-cdk`), or run via `npx aws-cdk`.
- AWS credentials for `cdk deploy` (synth needs none when run
  environment-agnostically).
- The golden AMI from step 1, registered in SSM Parameter Store (see above).

## Usage

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# Synthesize (no credentials required):
cdk synth

# Deploy (account/region come from CDK_DEFAULT_ACCOUNT / CDK_DEFAULT_REGION or
# your configured profile; nothing is hardcoded):
cdk bootstrap
cdk deploy --all
cdk deploy --all -c instanceType=m8a.metal-48xl
```

Account and region are never hardcoded — they come from the standard CDK
environment variables. Running `cdk synth` without those set produces
environment-agnostic templates (two dummy AZs), which is how CI validates the app
without credentials.

## Layout

```
cdk/
  app.py                         App entry point; wires the three stacks.
  cdk.json                       CDK config and default context.
  requirements.txt               aws-cdk-lib, constructs.
  stacks/
    __init__.py
    config.py                    Pinned versions + instance-type -> MetalSpec.
    network_stack.py             VPC, subnets, VPC endpoints.
    iam_stack.py                 Instance role/profile + scoped S3 policy + bucket.
    lab_host_stack.py            The metal reference host from the custom AMI.
```

## Notes on aws-cdk-lib enum names

- The 48xl metal size is `ec2.InstanceSize.XLARGE48METAL` (there is no
  `METAL48XL`). `InstanceType.of(InstanceClass.M8I, InstanceSize.XLARGE48METAL)`
  renders `m8i.metal-48xl`. Verified against aws-cdk-lib 2.255.
- `ec2.Instance(require_imdsv2=True)` synthesizes a launch template with
  `HttpTokens: required` attached to the instance.
