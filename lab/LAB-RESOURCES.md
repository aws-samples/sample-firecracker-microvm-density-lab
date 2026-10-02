# Lab AWS resources

Resources created to support density lab sessions. All are tagged
`Purpose=firecracker-density-lab` and `Ephemeral=true` so they can be found and removed.

None of these are part of the published reference implementation. They exist only to run
the experiments that produce the measured numbers.

## Persistent between sessions

| Resource | Name | Purpose |
|----------|------|---------|
| IAM role | `fc-lab-ssm-role` | Lets a lab instance be driven over SSM Session Manager, so no SSH key or inbound port is needed |
| IAM inline policy | `fc-lab-s3-transfer` | Read staged artifacts, write results under `benchmark_results/` |
| IAM instance profile | `fc-lab-ssm-profile` | Attaches the role to instances |
| S3 bucket | `fc-lab-transfer-<account-id>` | Staging for build artifacts, and durable storage for results |
| AMI + EBS snapshot | `fc-lab-<sku>-<timestamp>` | Provisioned host state and guest artifacts, so a session does not re-provision |

The bucket exists because SSM `SendCommand` rejects payloads beyond roughly 45 KB of
base64, and a compiled Go binary is a few megabytes. Small source trees can be shipped
inline; binaries and rootfs images cannot.

The AMIs are baked by `src/scripts/bake-lab-ami.sh`, one per SKU, and tagged `LabAmi=true`
with `BakedFrom=<instance-type>`. They are the only lab resource with a non-trivial idle cost:
each carries a snapshot of a 100 GiB root volume, billed on used blocks. Launch from one with
`launch-lab-host.sh --golden`.

### The S3 policy, and why it is scoped the way it is

`AmazonSSMManagedInstanceCore` grants no S3 access, so without this policy the host cannot fetch
its own binaries or upload its results. The 2026-08-01 session worked around it with presigned
URLs for the download and hand-transferred 109,350 sample rows out over SSM in base64 chunks,
which is slow and easy to get wrong.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "ReadStagedArtifacts",   "Effect": "Allow", "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::fc-lab-transfer-<account-id>/*" },
    { "Sid": "WriteBenchmarkResults", "Effect": "Allow", "Action": ["s3:PutObject"],
      "Resource": "arn:aws:s3:::fc-lab-transfer-<account-id>/benchmark_results/*" },
    { "Sid": "ListBucketForCLI",      "Effect": "Allow", "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::fc-lab-transfer-<account-id>" }
  ]
}
```

Three deliberate restrictions:

- **One bucket, named explicitly.** No wildcard across account buckets.
- **Write is confined to `benchmark_results/*`.** A lab host can publish results but cannot
  overwrite the binaries and images staged for it to consume, so a bug in the sweep cannot
  corrupt the inputs of the next run.
- **No `s3:DeleteObject`.** Results are append-only from the host's perspective. Combined with
  the timestamped prefix per run, a sweep cannot destroy an earlier dataset.

Apply with:

```bash
aws iam put-role-policy --role-name fc-lab-ssm-role \
  --policy-name fc-lab-s3-transfer --policy-document file://s3-policy.json --profile "$PROFILE"
```

## Created and destroyed per session

Metal instances, tagged `Name=fc-lab-<sku>-sessionN`. Launched with
`--instance-initiated-shutdown-behavior terminate` and a `DeleteOnTermination` root volume
so a shutdown from inside the guest cannot leave a running instance or an orphaned volume.

## Cleanup

Run when the lab phase is finished, not between sessions:

```bash
PROFILE=<your-profile>
ACCOUNT=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)

# Confirm nothing is still running first.
aws ec2 describe-instances --profile "$PROFILE" \
  --filters Name=tag:Purpose,Values=firecracker-density-lab \
            Name=instance-state-name,Values=running,pending \
  --query 'Reservations[].Instances[].InstanceId' --output text

# Bucket, then IAM. Confirm the datasets are committed to sources/lab-results/ first: the
# bucket holds the only other copy, and reproducing a run costs a paid metal session.
aws s3 ls "s3://fc-lab-transfer-$ACCOUNT/benchmark_results/" --recursive --profile "$PROFILE"
aws s3 rm "s3://fc-lab-transfer-$ACCOUNT" --recursive --profile "$PROFILE"
aws s3 rb "s3://fc-lab-transfer-$ACCOUNT" --profile "$PROFILE"

aws iam delete-role-policy --role-name fc-lab-ssm-role \
  --policy-name fc-lab-s3-transfer --profile "$PROFILE"
aws iam remove-role-from-instance-profile \
  --instance-profile-name fc-lab-ssm-profile --role-name fc-lab-ssm-role --profile "$PROFILE"
aws iam delete-instance-profile --instance-profile-name fc-lab-ssm-profile --profile "$PROFILE"
aws iam detach-role-policy --role-name fc-lab-ssm-role \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore --profile "$PROFILE"
aws iam delete-role --role-name fc-lab-ssm-role --profile "$PROFILE"

# AMIs, then their snapshots. Deregistering an image does NOT delete the snapshot behind it,
# so doing only the first half leaves the storage cost in place with nothing pointing at it.
# Collect the snapshot IDs before deregistering: afterwards the image no longer lists them.
REGION=<your-region>
SNAPS=$(aws ec2 describe-images --owners self --region "$REGION" --profile "$PROFILE" \
  --filters Name=tag:LabAmi,Values=true \
  --query 'Images[].BlockDeviceMappings[].Ebs.SnapshotId' --output text)

for ami in $(aws ec2 describe-images --owners self --region "$REGION" --profile "$PROFILE" \
    --filters Name=tag:LabAmi,Values=true --query 'Images[].ImageId' --output text); do
  aws ec2 deregister-image --image-id "$ami" --region "$REGION" --profile "$PROFILE"
done

for snap in $SNAPS; do
  aws ec2 delete-snapshot --snapshot-id "$snap" --region "$REGION" --profile "$PROFILE"
done
```

Verify afterwards:

```bash
aws s3 ls --profile "$PROFILE" | grep fc-lab            # expect no output
aws iam list-instance-profiles --profile "$PROFILE" \
  --query 'InstanceProfiles[?contains(InstanceProfileName, `fc-lab`)]' --output text
aws ec2 describe-images --owners self --region "$REGION" --profile "$PROFILE" \
  --filters Name=tag:LabAmi,Values=true --query 'Images[].ImageId' --output text
aws ec2 describe-snapshots --owner-ids self --region "$REGION" --profile "$PROFILE" \
  --filters Name=tag:Purpose,Values=firecracker-density-lab \
  --query 'Snapshots[].SnapshotId' --output text
```

## Ongoing cost when idle

The IAM role and instance profile are free. The bucket costs only for stored objects,
which are a few megabytes, so cents per month.

**The baked AMIs are the one idle cost worth tracking.** Each holds an EBS snapshot of a
100 GiB root volume, billed on used blocks — a few GiB for a host-only image, and more once
task rootfs images are baked in. That is single-digit dollars per month per SKU, which pays
for itself against one avoided 40-minute provisioning run but should still be cleaned up when
the lab phase ends.

Metal instances remain the dominant cost and exist only while a session is active.
