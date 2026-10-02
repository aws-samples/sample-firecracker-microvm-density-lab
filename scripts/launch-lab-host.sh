#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Launch an EC2 bare metal instance for a density sweep.
#
# SMT is NOT set here, because EC2 CpuOptions are not available on bare metal. RunInstances
# rejects them outright:
#
#   UnsupportedOperation: The m8i.metal-48xl instance type does not support
#   specifying CpuOptions.
#
# describe-instance-types confirms why: ValidThreadsPerCore is null for metal SKUs and [1,2]
# for the virtualized m8i.48xlarge. CpuOptions is a Nitro hypervisor feature, and on metal
# there is no hypervisor between the instance and the CPU to enforce a thread count.
#
# On bare metal, SMT is toggled at runtime through the kernel instead:
#
#   echo off > /sys/devices/system/cpu/smt/control
#
# That is better for this experiment than two differently-launched hosts would have been: both
# SMT states are measured on the same physical machine, so a difference cannot be host variance.
# See toggle-smt.sh, which performs the switch and verifies it took effect.
#
# By default this launches the stock Amazon Linux 2023 image, which then needs provisioning and a
# reboot before it can measure anything. --golden launches the most recent AMI baked by
# bake-lab-ami.sh for the same instance type instead, which arrives ready and skips roughly 40
# minutes of paid provisioning.
#
# Usage:
#   ./launch-lab-host.sh --profile <aws-profile>
#   ./launch-lab-host.sh --profile <aws-profile> --instance-type m8a.metal-48xl
#   ./launch-lab-host.sh --profile <aws-profile> --golden
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

PROFILE=""
REGION="${AWS_REGION:-us-east-1}"
INSTANCE_TYPE="m8i.metal-48xl"
VOLUME_GIB=100
IAM_PROFILE="fc-lab-ssm-profile"
SUBNET_ID=""
# Empty rather than a hardcoded AZ. The region comes from the environment, so a fixed default
# such as us-east-1a silently contradicts AWS_REGION=us-west-2 and the subnet lookup then fails
# with a misleading "no subnet in us-east-1a". Resolved from the region below once both are known.
AZ=""
AMI_ID=""
GOLDEN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)       PROFILE="$2"; shift 2 ;;
    --region)        REGION="$2"; shift 2 ;;
    --instance-type) INSTANCE_TYPE="$2"; shift 2 ;;
    --volume-gib)    VOLUME_GIB="$2"; shift 2 ;;
    --iam-profile)   IAM_PROFILE="$2"; shift 2 ;;
    --subnet)        SUBNET_ID="$2"; shift 2 ;;
    --az)            AZ="$2"; shift 2 ;;
    # Rejected empty rather than ignored: falling through to the stock image would silently launch an
    # unprovisioned host for someone who asked for a specific one.
    --ami)           [[ -n "$2" ]] || die "--ami needs an AMI ID"; AMI_ID="$2"; shift 2 ;;
    --golden)        GOLDEN=1; shift ;;
    -h|--help)       sed -n '2,31p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    # --smt used to select CpuOptions ThreadsPerCore. It is rejected rather than ignored, so a
    # saved command line fails loudly instead of silently measuring the platform default.
    --smt) die "--smt is not supported: EC2 CpuOptions are unavailable on bare metal. Toggle SMT on the running host with toggle-smt.sh instead." ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$PROFILE" ]] || die "--profile is required"
require_cmd aws

[[ -n "$AMI_ID" && $GOLDEN -eq 1 ]] \
  && die "--ami and --golden both given; --golden resolves an AMI, so pass one or the other"

# A .metal instance is the only way to reach /dev/kvm on EC2, since nested virtualization is
# not offered. Launching a virtualized type would produce a host that cannot run the sweep.
case "$INSTANCE_TYPE" in
  *.metal*) ;;
  *) die "instance type $INSTANCE_TYPE is not bare metal; Firecracker needs /dev/kvm" ;;
esac

section "Resolving launch parameters"

read -r CORE_COUNT THREADS_PER_CORE <<<"$(aws ec2 describe-instance-types \
  --instance-types "$INSTANCE_TYPE" --region "$REGION" --profile "$PROFILE" \
  --query 'InstanceTypes[0].VCpuInfo.[DefaultCores,DefaultThreadsPerCore]' --output text)"
[[ "$CORE_COUNT" != "None" && -n "$CORE_COUNT" ]] || die "could not determine core count for $INSTANCE_TYPE"
# Guarded for the same reason as CORE_COUNT, and not merged with it because a null here has its own
# cause: AWS reports null ThreadsPerCore fields for metal SKUs. Without this, --output text yields
# the literal "None" and the arithmetic below aborts with "None: unbound variable" under set -u.
[[ "$THREADS_PER_CORE" != "None" && -n "$THREADS_PER_CORE" ]] || die "could not determine threads per core for $INSTANCE_TYPE"
EXPECTED_VCPUS=$(( CORE_COUNT * THREADS_PER_CORE ))
ok "$INSTANCE_TYPE has $CORE_COUNT physical cores x $THREADS_PER_CORE thread(s) = $EXPECTED_VCPUS vCPUs"

# Report whether this host can be toggled at all, since it determines which configs the sweep
# can express. An AMD metal host has one thread per core in hardware and reports
# "notsupported" in sysfs, which positively confirms the no-SMT finding rather than inferring
# it from a spec table.
if [[ "$THREADS_PER_CORE" -gt 1 ]]; then
  log "SMT is available; toggle it on the running host with toggle-smt.sh"
else
  warn "$INSTANCE_TYPE has one thread per core, so there is no SMT to toggle; configs C, E, and F will be skipped with a recorded reason"
fi

# An AZ from a different region than REGION can never resolve, and the resulting "no subnet"
# error points at the subnet rather than the mismatch. Catch it here with a message that names
# the actual problem.
if [[ -n "$AZ" && "$AZ" != "${REGION}"* ]]; then
  die "--az $AZ is not in region $REGION; pass --region $(echo "$AZ" | sed 's/[a-z]$//') or an AZ within $REGION"
fi

if [[ -z "$SUBNET_ID" ]]; then
  VPC_ID="$(aws ec2 describe-vpcs --region "$REGION" --profile "$PROFILE" \
    --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)"
  [[ "$VPC_ID" != "None" ]] || die "no default VPC; pass --subnet explicitly"

  # Not every AZ offers metal, and a default VPC does not necessarily have a subnet in every AZ,
  # so pick an AZ that satisfies both rather than assuming one.
  if [[ -z "$AZ" ]]; then
    OFFERED="$(aws ec2 describe-instance-type-offerings --region "$REGION" --profile "$PROFILE" \
      --location-type availability-zone \
      --filters "Name=instance-type,Values=${INSTANCE_TYPE}" \
      --query 'InstanceTypeOfferings[].Location' --output text)"
    [[ -n "$OFFERED" ]] || die "$INSTANCE_TYPE is not offered in any AZ of $REGION"

    for candidate in $OFFERED; do
      found="$(aws ec2 describe-subnets --region "$REGION" --profile "$PROFILE" \
        --filters Name=vpc-id,Values="$VPC_ID" Name=availability-zone,Values="$candidate" \
        --query 'Subnets[0].SubnetId' --output text)"
      if [[ "$found" != "None" && -n "$found" ]]; then
        AZ="$candidate"
        SUBNET_ID="$found"
        break
      fi
    done
    [[ -n "$SUBNET_ID" ]] || die "no subnet in any AZ offering $INSTANCE_TYPE ($OFFERED); pass --subnet explicitly"
    log "auto-selected $AZ (offers $INSTANCE_TYPE and has a default-VPC subnet)"
  else
    SUBNET_ID="$(aws ec2 describe-subnets --region "$REGION" --profile "$PROFILE" \
      --filters Name=vpc-id,Values="$VPC_ID" Name=availability-zone,Values="$AZ" \
      --query 'Subnets[0].SubnetId' --output text)"
    [[ "$SUBNET_ID" != "None" ]] || die "no subnet in $AZ; pass --subnet explicitly"
  fi
fi
ok "subnet $SUBNET_ID in $AZ"

if (( GOLDEN )); then
  # Filtered on the exact instance type the image was baked from, not just on the lab tag, and not
  # merely on the family. m8i does not describe m8a -- different vendor, different SMT capability,
  # different NUMA shape -- and sizes within a family differ in socket and node count too. Booting
  # the wrong image produces a host that looks healthy and measures the wrong machine, so an
  # over-broad match here is worse than not finding an image at all.
  AMI_ID="$(aws ec2 describe-images --region "$REGION" --profile "$PROFILE" --owners self \
    --filters Name=tag:LabAmi,Values=true "Name=tag:BakedFrom,Values=${INSTANCE_TYPE}" \
              Name=state,Values=available \
    --query 'sort_by(Images, &CreationDate)[-1].ImageId' --output text)"
  [[ "$AMI_ID" != "None" && -n "$AMI_ID" ]] \
    || die "no baked lab AMI for ${INSTANCE_TYPE} in ${REGION}. Bake one with bake-lab-ami.sh, or drop --golden to launch stock Amazon Linux 2023 and provision."
  ok "golden AMI $AMI_ID (baked from $INSTANCE_TYPE)"
elif [[ -n "$AMI_ID" ]]; then
  ok "AMI $AMI_ID (supplied)"
else
  AMI_ID="$(aws ssm get-parameter --region "$REGION" --profile "$PROFILE" \
    --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
    --query 'Parameter.Value' --output text)"
  ok "AMI $AMI_ID (stock Amazon Linux 2023; needs provisioning and a reboot)"
fi

NAME="fc-lab-$(echo "$INSTANCE_TYPE" | cut -d. -f1)"

section "Launching"

# No --cpu-options: bare metal rejects it. SMT is toggled on the running host instead.
#
# instance-initiated-shutdown-behavior=terminate and DeleteOnTermination mean a shutdown from
# inside the guest cannot leave a running instance or an orphaned volume behind.
INSTANCE_ID="$(aws ec2 run-instances \
  --region "$REGION" --profile "$PROFILE" \
  --image-id "$AMI_ID" \
  --instance-type "$INSTANCE_TYPE" \
  --subnet-id "$SUBNET_ID" \
  --iam-instance-profile "Name=${IAM_PROFILE}" \
  --block-device-mappings "DeviceName=/dev/xvda,Ebs={VolumeSize=${VOLUME_GIB},VolumeType=gp3,DeleteOnTermination=true}" \
  --instance-initiated-shutdown-behavior terminate \
  --metadata-options 'HttpTokens=required,HttpEndpoint=enabled' \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${NAME}},{Key=Purpose,Value=firecracker-density-lab},{Key=Ephemeral,Value=true}]" \
  --query 'Instances[0].InstanceId' --output text)"

ok "launched $INSTANCE_ID ($NAME)"

if (( GOLDEN )); then
  NEXT_STEP="  Already provisioned. Confirm the baked state survived the capture, then sweep:"
else
  NEXT_STEP="  Not provisioned yet. Stage the artifacts, then run setup-lab-host.sh, reboot, and
  bake an AMI so the next session can pass --golden:

    sudo ./setup-lab-host.sh --bucket fc-lab-transfer-<account-id>
    sudo reboot
    ./bake-lab-ami.sh --profile ${PROFILE} --region ${REGION} --instance-id ${INSTANCE_ID}"
fi

cat <<SUMMARY

  Instance:     $INSTANCE_ID
  Type:         $INSTANCE_TYPE
  Image:        $AMI_ID$( ((GOLDEN)) && echo " (golden)" )
  Topology:     $CORE_COUNT physical cores x $THREADS_PER_CORE thread(s) = $EXPECTED_VCPUS vCPUs
  SMT:          toggled at runtime, not at launch (CpuOptions is unavailable on metal)

${NEXT_STEP}

  Both SMT states are measured on this one host, so an SMT difference cannot be host
  variance. Run the SMT-on pass first, since it needs no change:

    ./validate-host.sh
    labsweep -run-label smt-on -requested-threads-per-core $THREADS_PER_CORE \\
      -configs A,B,C,D,E,F -densities 10,45,90,135,180 -out results/

  Then switch and re-run. toggle-smt.sh verifies the change took effect before you spend
  time on a sweep that would otherwise measure the wrong configuration:

    sudo ./toggle-smt.sh off
    labsweep -run-label smt-off -requested-threads-per-core 1 \\
      -configs A,B,C,D,E,F -densities 10,45,90,135,180 -out results/

  Configs C, E, and F require SMT siblings, so they are skipped with a recorded reason on
  the SMT-off pass rather than producing meaningless data.

  Terminate when done:

    aws ec2 terminate-instances --instance-ids $INSTANCE_ID --region $REGION --profile $PROFILE

SUMMARY
