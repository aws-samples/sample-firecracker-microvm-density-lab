#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Bake a provisioned lab host into a reusable AMI.
#
# Every session so far has started from the stock Amazon Linux 2023 image and rebuilt the same
# things: the guest kernel, the guest agent, the base rootfs, and the task rootfs for whichever
# benchmark that session measured. That is roughly 40 minutes of paid bare-metal time before the
# first measurement, on an instance that costs the same per hour whether it is compiling or
# measuring. The cache-affinity session cost ~$18 for 1.2 h that was mostly provisioning.
#
# Baking once turns that into a launch. An instance from this AMI comes up with the kernel args and
# KVM module parameters already applied -- so it needs no reboot -- and with every guest artifact
# already on disk.
#
# What the image carries:
#   - the provisioned host state that needs a reboot to take effect (kernel cmdline, KVM module
#     parameters, swap off, raised fd/PID limits)
#   - /opt/firecracker/vmlinux, rootfs.ext4, and any rootfs-*.ext4 task images built on the host
#   - the labsweep and labctl binaries, and the scripts directory
#
# What it does not, and cannot, carry: NUMA discovery, cpuset allocation, SMT state, and warm-pool
# prefill are all runtime decisions made against the host the instance actually landed on.
# Firecracker snapshots are also deliberately excluded -- they only resume on identical hardware,
# so a snapshot baked on one SKU is a trap on another.
#
# The host is validated before anything is captured. An AMI baked from a half-provisioned host is
# indistinguishable from a good one until a sweep produces wrong numbers, which is the expensive
# way to find out.
#
# Usage:
#   ./bake-lab-ami.sh --profile <aws-profile> --instance-id i-abc123
#   ./bake-lab-ami.sh --profile <aws-profile> --instance-id i-abc123 --dry-run
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

PROFILE=""
REGION="${AWS_REGION:-us-east-1}"
INSTANCE_ID=""
AMI_NAME=""
REMOTE_SCRIPTS="/root/scripts"
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)     PROFILE="$2"; shift 2 ;;
    --region)      REGION="$2"; shift 2 ;;
    --instance-id) INSTANCE_ID="$2"; shift 2 ;;
    --name)        AMI_NAME="$2"; shift 2 ;;
    --scripts-dir) REMOTE_SCRIPTS="$2"; shift 2 ;;
    --dry-run)     DRY_RUN=1; shift ;;
    -h|--help)     sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$PROFILE" ]] || die "--profile is required"
[[ -n "$INSTANCE_ID" ]] || die "--instance-id is required"
require_cmd aws

aws_() { aws --region "$REGION" --profile "$PROFILE" "$@"; }

section "Inspecting $INSTANCE_ID"

read -r INSTANCE_TYPE STATE <<<"$(aws_ ec2 describe-instances --instance-ids "$INSTANCE_ID" \
  --query 'Reservations[0].Instances[0].[InstanceType,State.Name]' --output text)"
[[ -n "$INSTANCE_TYPE" && "$INSTANCE_TYPE" != "None" ]] || die "instance $INSTANCE_ID not found in $REGION"
[[ "$STATE" == "running" ]] || die "instance is $STATE; it must be running so the host can be validated before capture"
ok "$INSTANCE_ID is a running $INSTANCE_TYPE"

SKU="$(cut -d. -f1 <<<"$INSTANCE_TYPE")"

section "Validating the host before capture"

# Driven over SSM because that is the only channel these hosts have -- they launch without a key
# pair. validate-host.sh exits 2 on warnings, which SSM would report as a failed command, so the
# exit code is echoed and the wrapper always exits 0. The distinction matters: warnings are
# acceptable in an image, failures are not.
PARAMS_FILE="$(mktemp)"
trap 'rm -f "$PARAMS_FILE"' EXIT
cat >"$PARAMS_FILE" <<JSON
{"commands": [
  "set +e",
  "${REMOTE_SCRIPTS}/validate-host.sh > /tmp/bake-validate.log 2>&1",
  "echo VALIDATE_EXIT=\$?",
  "echo FIRECRACKER=\$(firecracker --version 2>/dev/null | head -1)",
  "echo ---ARTIFACTS---",
  "ls -1 ${ARTIFACT_DIR} 2>/dev/null",
  "echo ---LOG---",
  "cat /tmp/bake-validate.log",
  "exit 0"
]}
JSON

CMD_ID="$(aws_ ssm send-command --instance-ids "$INSTANCE_ID" \
  --document-name AWS-RunShellScript --parameters "file://${PARAMS_FILE}" \
  --query 'Command.CommandId' --output text)"
# The waiter's own failure is ignored because a non-zero validate-host.sh exit surfaces as a Failed
# invocation, and that case is diagnosed below by parsing the output rather than by exit status.
aws_ ssm wait command-executed --command-id "$CMD_ID" --instance-id "$INSTANCE_ID" >/dev/null 2>&1 || true

# Status is fetched separately from the output, in its own call, because the output is multi-line and
# would not survive being read into a shell variable alongside another field. It is needed because
# the waiter caps at 100 s (delay 5 x 20 attempts) and gives up silently: without the status, a
# command still InProgress looks identical to one that never reached the host, and the operator gets
# sent to debug IAM for what is only slowness.
SSM_STATUS="$(aws_ ssm get-command-invocation --command-id "$CMD_ID" --instance-id "$INSTANCE_ID" \
  --query 'Status' --output text)"
OUTPUT="$(aws_ ssm get-command-invocation --command-id "$CMD_ID" --instance-id "$INSTANCE_ID" \
  --query 'StandardOutputContent' --output text)"

VALIDATE_EXIT="$(awk -F= '/^VALIDATE_EXIT=/ {print $2; exit}' <<<"$OUTPUT")"
if [[ -z "$VALIDATE_EXIT" ]]; then
  case "$SSM_STATUS" in
    InProgress|Pending|Delayed)
      die "validate-host.sh is still running on the host after the SSM waiter gave up (status ${SSM_STATUS}). Nothing was captured. Re-run once it finishes: aws ssm get-command-invocation --command-id ${CMD_ID} --instance-id ${INSTANCE_ID}" ;;
    *)
      die "no usable response from validate-host.sh over SSM (status ${SSM_STATUS:-unknown}); check that the instance profile grants ssm:UpdateInstanceInformation and that ${REMOTE_SCRIPTS}/validate-host.sh exists" ;;
  esac
fi

case "$VALIDATE_EXIT" in
  0) ok "validate-host.sh: all checks passed" ;;
  2) warn "validate-host.sh passed with warnings; they will be baked into every instance launched from this image" ;;
  *) sed -n '/^---LOG---/,$p' <<<"$OUTPUT" | grep -E '\[FAIL\]|\[WARN\]' >&2 || true
     die "validate-host.sh exited ${VALIDATE_EXIT}; the host is not ready to bake" ;;
esac

# The version reported by the binary on the host, not FIRECRACKER_VERSION from common.sh. The two can
# disagree: validate-host.sh treats a mismatch against the pin as a warning, and a warning is allowed
# to bake. Tagging the pin would mean the tag can never contradict it, which defeats the point of
# recording it -- a host running something else would bake into an image claiming the pinned version.
FC_VERSION="$(awk '/^FIRECRACKER=/ {sub(/^FIRECRACKER=/, ""); print; exit}' <<<"$OUTPUT")"
[[ -n "$FC_VERSION" ]] || FC_VERSION="unknown"
# An empty shorthand value is a hard parser error rather than an empty tag, so a space would break
# create-image outright. Underscored for the tag; the description keeps the readable form.
FC_VERSION_TAG="${FC_VERSION// /_}"

ARTIFACTS="$(sed -n '/^---ARTIFACTS---/,/^---LOG---/p' <<<"$OUTPUT" | grep -E '\.ext4$|^vmlinux$' || true)"

# -F because the '.' in rootfs.ext4 is otherwise a regex wildcard: -x anchors the line but does not
# make the pattern literal, so a file named rootfsXext4 would satisfy the requirement.
for required in vmlinux rootfs.ext4; do
  grep -qxF "$required" <<<"$ARTIFACTS" \
    || die "${ARTIFACT_DIR}/${required} is missing; run setup-lab-host.sh before baking"
done
ok "guest artifacts: $(tr '\n' ' ' <<<"$ARTIFACTS")"

# Recorded as a tag so a stale image is identifiable without launching it. A Firecracker version
# change invalidates snapshots fleet-wide, so knowing which build an image carries is the
# difference between a five-second check and a failed restore at density.
# Joined with '+' rather than a comma: this lands in a --tag-specifications shorthand value, and
# the AWS CLI splits that on commas at the top level, so a comma here becomes a malformed tag.
TASK_IMAGES="$(grep -E '^rootfs-.*\.ext4$' <<<"$ARTIFACTS" | paste -sd+ - || true)"
[[ -n "$TASK_IMAGES" ]] || TASK_IMAGES="none"
log "task images: ${TASK_IMAGES}"

[[ -n "$AMI_NAME" ]] || AMI_NAME="fc-lab-${SKU}-$(date -u +%Y%m%d-%H%M%S)"

if (( DRY_RUN )); then
  section "Dry run"
  cat <<PLAN

  Would create an AMI from ${INSTANCE_ID} (${INSTANCE_TYPE}):

    Name:        ${AMI_NAME}
    Firecracker: ${FC_VERSION:-unknown}
    Kernel:      ${GUEST_KERNEL_VERSION}
    Task images: ${TASK_IMAGES}

  The instance would be rebooted for a consistent snapshot. Re-run without --dry-run to bake.

PLAN
  exit 0
fi

section "Creating the image"

# The instance is allowed to reboot. --no-reboot would capture the filesystem live, and the guest
# artifacts here are multi-GiB ext4 files that may have dirty pages outstanding -- a torn rootfs
# boots and then fails in ways that look like a Firecracker bug. The reboot also proves the baked
# kernel args survive one, which is the whole reason this image exists.
IMAGE_ID="$(aws_ ec2 create-image \
  --instance-id "$INSTANCE_ID" \
  --name "$AMI_NAME" \
  --description "Firecracker density lab, baked from ${INSTANCE_TYPE}. Firecracker ${FC_VERSION}, guest kernel ${GUEST_KERNEL_VERSION}. Task images are listed in the TaskImages tag." \
  --tag-specifications \
    "ResourceType=image,Tags=[{Key=Name,Value=${AMI_NAME}},{Key=Purpose,Value=firecracker-density-lab},{Key=LabAmi,Value=true},{Key=BakedFrom,Value=${INSTANCE_TYPE}},{Key=FirecrackerVersion,Value=${FC_VERSION_TAG}},{Key=GuestKernelVersion,Value=${GUEST_KERNEL_VERSION}},{Key=TaskImages,Value=${TASK_IMAGES}}]" \
    "ResourceType=snapshot,Tags=[{Key=Name,Value=${AMI_NAME}},{Key=Purpose,Value=firecracker-density-lab},{Key=LabAmi,Value=true}]" \
  --query 'ImageId' --output text)"

ok "created ${IMAGE_ID} (${AMI_NAME})"
log "waiting for it to become available; this takes several minutes for a 100 GiB root volume"

# The waiter caps at 600 s (delay 15 x 40 attempts), which a 100 GiB snapshot can exceed. Treating a
# waiter timeout as a bake failure would be actively expensive: the operator re-runs, and a second
# create-image means a second AMI and a second billed snapshot while the first is still completing.
# So the state is re-read, and only a terminal state is fatal. 'unknown' means the describe call
# itself failed, which is transient and not evidence about the image, so it keeps waiting.
ROUNDS=0
until aws_ ec2 wait image-available --image-ids "$IMAGE_ID" 2>/dev/null; do
  IMAGE_STATE="$(aws_ ec2 describe-images --image-ids "$IMAGE_ID" \
    --query 'Images[0].State' --output text 2>/dev/null || echo unknown)"
  case "$IMAGE_STATE" in
    available) break ;;
    pending|unknown)
      (( ++ROUNDS <= 3 )) || die "${IMAGE_ID} is still ${IMAGE_STATE} after ~40 minutes. It may yet complete: check the console rather than baking again, since a second bake means a second billed snapshot."
      log "state ${IMAGE_STATE} after the waiter's ceiling; continuing to wait (round ${ROUNDS} of 3)" ;;
    *) die "${IMAGE_ID} is in state '${IMAGE_STATE}'. Deregister it and delete its snapshot before retrying, or a dead image and its storage are left behind." ;;
  esac
done
ok "${IMAGE_ID} is available"

cat <<SUMMARY

  AMI:         ${IMAGE_ID}
  Name:        ${AMI_NAME}
  Baked from:  ${INSTANCE_TYPE}
  Task images: ${TASK_IMAGES}

  Launch from it. --golden picks the newest lab AMI in the account, so a saved command line does
  not need editing after a rebake:

    ./launch-lab-host.sh --profile ${PROFILE} --region ${REGION} \\
      --instance-type ${INSTANCE_TYPE} --golden

  No provisioning and no reboot. Confirm the state survived the capture, then sweep:

    ${REMOTE_SCRIPTS}/validate-host.sh

  Terminate the host this was baked from -- the image no longer needs it:

    aws ec2 terminate-instances --instance-ids ${INSTANCE_ID} --region ${REGION} --profile ${PROFILE}

  Deregistering the AMI does NOT delete its snapshot, so a bucket-style orphan is easy to leave
  behind. Remove both when the lab phase is finished:

    aws ec2 deregister-image --image-id ${IMAGE_ID} --region ${REGION} --profile ${PROFILE}
    aws ec2 describe-snapshots --owner-ids self --region ${REGION} --profile ${PROFILE} \\
      --filters Name=tag:LabAmi,Values=true --query 'Snapshots[].SnapshotId' --output text \\
      | xargs -n1 aws ec2 delete-snapshot --region ${REGION} --profile ${PROFILE} --snapshot-id

SUMMARY
