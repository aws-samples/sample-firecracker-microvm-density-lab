#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Create the per-account lab resources a density session needs.
#
# These are the resources described in src/lab/LAB-RESOURCES.md: an SSM role so a host can be
# driven with no SSH key and no inbound port, an S3 bucket for staging artifacts in and results
# out, and the instance profile that binds the role to a launch. They persist between sessions and
# cost effectively nothing idle, so this runs once per account rather than once per session.
#
# The bucket is the part worth insisting on. SSM SendCommand rejects payloads beyond roughly 45 KB
# of base64 and a compiled Go binary is a few megabytes, so without it a host cannot fetch its own
# binaries. One earlier session worked around that with presigned URLs and hand-transferred 109,350
# sample rows out over SSM in base64 chunks; another lost ~8,100 samples permanently for want of a
# results destination. Two later sessions were killed externally mid-run and lost nothing, because
# labsweep was uploading to SWEEP_S3_BUCKET as cells completed.
#
# Idempotent: every step tolerates the resource already existing, so a partial run can be re-run.
#
# Usage:
#   ./bootstrap-lab-account.sh --profile <aws-profile>
#   ./bootstrap-lab-account.sh --profile <aws-profile> --dry-run
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

PROFILE=""
REGION="${AWS_REGION:-us-east-1}"
ROLE_NAME="fc-lab-ssm-role"
PROFILE_NAME="fc-lab-ssm-profile"
POLICY_NAME="fc-lab-s3-transfer"
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile) [[ $# -ge 2 ]] || die "--profile needs a value"; PROFILE="$2"; shift 2 ;;
    --region)  [[ $# -ge 2 ]] || die "--region needs a value"; REGION="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$PROFILE" ]] || die "--profile is required"
require_cmd aws

aws_() { aws --profile "$PROFILE" "$@"; }

section "Identifying the account"

ACCOUNT="$(aws_ sts get-caller-identity --query Account --output text)"
CALLER="$(aws_ sts get-caller-identity --query Arn --output text)"
BUCKET="fc-lab-transfer-${ACCOUNT}"
ok "account ${ACCOUNT} in ${REGION}"
log "caller ${CALLER}"

if (( DRY_RUN )); then
  section "Dry run"
  cat <<PLAN

  Would create, in account ${ACCOUNT}:

    IAM role             ${ROLE_NAME}          (trusts ec2.amazonaws.com)
    Managed policy       AmazonSSMManagedInstanceCore   attached to the role
    Inline policy        ${POLICY_NAME}        scoped to s3://${BUCKET}
    Instance profile     ${PROFILE_NAME}       containing the role
    S3 bucket            s3://${BUCKET}        in ${REGION}

  Re-run without --dry-run to create them.

PLAN
  exit 0
fi

section "IAM role ${ROLE_NAME}"

if aws_ iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
  ok "already exists"
else
  aws_ iam create-role --role-name "$ROLE_NAME" \
    --description "Firecracker density lab: SSM access and scoped S3 transfer" \
    --assume-role-policy-document '{
      "Version": "2012-10-17",
      "Statement": [{
        "Effect": "Allow",
        "Principal": {"Service": "ec2.amazonaws.com"},
        "Action": "sts:AssumeRole"
      }]
    }' \
    --tags Key=Purpose,Value=firecracker-density-lab >/dev/null
  ok "created"
fi

# Grants SSM Session Manager and RunShellScript. Notably grants NO S3 access, which is why the
# inline policy below exists.
aws_ iam attach-role-policy --role-name "$ROLE_NAME" \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
ok "AmazonSSMManagedInstanceCore attached"

section "Inline policy ${POLICY_NAME}"

# Three deliberate restrictions, all reasoned in src/lab/LAB-RESOURCES.md: one named bucket rather
# than a wildcard; write confined to benchmark_results/* so a bug in the sweep cannot corrupt the
# staged inputs the next run consumes; and no s3:DeleteObject, so results are append-only from the
# host's perspective and a sweep cannot destroy an earlier dataset.
POLICY_FILE="$(mktemp)"
trap 'rm -f "$POLICY_FILE"' EXIT
cat >"$POLICY_FILE" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "ReadStagedArtifacts",   "Effect": "Allow", "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::${BUCKET}/*" },
    { "Sid": "WriteBenchmarkResults", "Effect": "Allow", "Action": ["s3:PutObject"],
      "Resource": "arn:aws:s3:::${BUCKET}/benchmark_results/*" },
    { "Sid": "ListBucketForCLI",      "Effect": "Allow", "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::${BUCKET}" }
  ]
}
JSON

aws_ iam put-role-policy --role-name "$ROLE_NAME" \
  --policy-name "$POLICY_NAME" --policy-document "file://${POLICY_FILE}"
ok "scoped to s3://${BUCKET}"

section "Instance profile ${PROFILE_NAME}"

if aws_ iam get-instance-profile --instance-profile-name "$PROFILE_NAME" >/dev/null 2>&1; then
  ok "already exists"
else
  aws_ iam create-instance-profile --instance-profile-name "$PROFILE_NAME" \
    --tags Key=Purpose,Value=firecracker-density-lab >/dev/null
  ok "created"
fi

# Already-present is not an error here: the profile may carry the role from a previous partial run,
# and a profile holds at most one role so there is nothing to reconcile.
if aws_ iam add-role-to-instance-profile \
     --instance-profile-name "$PROFILE_NAME" --role-name "$ROLE_NAME" 2>/dev/null; then
  ok "role added"
else
  IN_PROFILE="$(aws_ iam get-instance-profile --instance-profile-name "$PROFILE_NAME" \
    --query 'InstanceProfile.Roles[0].RoleName' --output text)"
  [[ "$IN_PROFILE" == "$ROLE_NAME" ]] \
    || die "${PROFILE_NAME} already contains role '${IN_PROFILE}' rather than ${ROLE_NAME}; remove it before re-running"
  ok "role already present"
fi

section "Bucket s3://${BUCKET}"

if aws_ s3api head-bucket --bucket "$BUCKET" 2>/dev/null; then
  ok "already exists"
else
  # us-east-1 is the API's default and rejects an explicit LocationConstraint naming it.
  if [[ "$REGION" == "us-east-1" ]]; then
    aws_ s3api create-bucket --bucket "$BUCKET" --region "$REGION" >/dev/null
  else
    aws_ s3api create-bucket --bucket "$BUCKET" --region "$REGION" \
      --create-bucket-configuration "LocationConstraint=${REGION}" >/dev/null
  fi
  ok "created in ${REGION}"
fi

aws_ s3api put-public-access-block --bucket "$BUCKET" \
  --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
ok "public access blocked"

aws_ s3api put-bucket-encryption --bucket "$BUCKET" \
  --server-side-encryption-configuration \
    '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
ok "default encryption enabled"

aws_ s3api put-bucket-tagging --bucket "$BUCKET" \
  --tagging 'TagSet=[{Key=Purpose,Value=firecracker-density-lab},{Key=Ephemeral,Value=true}]'
ok "tagged"

cat <<SUMMARY

  Account:  ${ACCOUNT}
  Region:   ${REGION}
  Role:     ${ROLE_NAME} (+ ${POLICY_NAME})
  Profile:  ${PROFILE_NAME}
  Bucket:   s3://${BUCKET}

  Stage the artifacts next. labsweep and labctl must be cross-compiled for linux/amd64 -- they run
  on the metal host, not here:

    cd ../lab && GOOS=linux GOARCH=amd64 go build -o /tmp/labsweep ./cmd/labsweep \\
                 && GOOS=linux GOARCH=amd64 go build -o /tmp/labctl ./cmd/labctl
    tar czf /tmp/scripts.tgz -C .. scripts
    tar czf /tmp/guest-agent-src.tgz -C .. guest-agent

    aws s3 cp /tmp/labsweep           s3://${BUCKET}/staging/ --profile ${PROFILE}
    aws s3 cp /tmp/labctl             s3://${BUCKET}/staging/ --profile ${PROFILE}
    aws s3 cp /tmp/scripts.tgz        s3://${BUCKET}/staging/ --profile ${PROFILE}
    aws s3 cp /tmp/guest-agent-src.tgz s3://${BUCKET}/staging/ --profile ${PROFILE}

  Then launch a host per SKU, provision it, and bake it so no later session pays for provisioning:

    ./launch-lab-host.sh --profile ${PROFILE} --region ${REGION} --instance-type m8a.metal-48xl
    ./launch-lab-host.sh --profile ${PROFILE} --region ${REGION} --instance-type m8i.metal-48xl

  Teardown is in src/lab/LAB-RESOURCES.md. Run it only once the datasets are committed: the bucket
  holds the only copy other than the repo.

SUMMARY
