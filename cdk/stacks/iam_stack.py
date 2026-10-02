# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""IAM: the instance role and profile the lab host runs under.

The role carries two grants:

- ``AmazonSSMManagedInstanceCore`` so the host is driven over SSM Session
  Manager, with no SSH key and no inbound port. This is how the lab and the host
  agent are reached.
- A scoped inline S3 policy for the density sweep, mirroring the ``labsweep``
  ``SWEEP_S3_BUCKET`` upload path and the policy documented in
  ``lab/LAB-RESOURCES.md``.

Deliberately absent: ECR pull permissions. The critical path stages every
artifact through the S3 bucket; if you convert container images to rootfs
images *on the host* with ``scripts/docker-to-rootfs.sh`` against a private
ECR registry, add ``AmazonEC2ContainerRegistryReadOnly`` back.

The S3 policy is deliberately narrow (see ``lab/LAB-RESOURCES.md`` "why it is
scoped the way it is"):

- ``s3:GetObject`` on the whole bucket, so the host can fetch staged binaries.
- ``s3:PutObject`` confined to ``benchmark_results/*``, so a sweep can publish
  results but cannot overwrite its own inputs.
- ``s3:ListBucket`` on the bucket so the AWS CLI can enumerate a prefix.
- No ``s3:DeleteObject``: results are append-only from the host's perspective.

The results bucket is **retained on stack destroy** (the CloudFormation
default for buckets): benchmark results are the product of the lab, so
``cdk destroy`` tears down the compute but never deletes the data. Empty and
remove the bucket manually when the results have been harvested (see
``cdk/README.md`` "Teardown").

Bucket hardening (security scan findings AWS-0132, AWS-0089): when this app
creates the transfer bucket it is encrypted with a customer-managed KMS key
(rotation enabled, retained on destroy alongside the bucket so retained data
stays decryptable) and server access logging is delivered to a dedicated logs
bucket. The logs bucket itself uses SSE-S3 because S3 server access logging
does not support delivery to SSE-KMS targets. When an existing bucket is
referenced by name via context, its encryption and logging are owned by
whoever created it and are not modified here.
"""

from __future__ import annotations

from typing import Optional

from aws_cdk import Aws, CfnOutput, RemovalPolicy, Stack
from aws_cdk import aws_iam as iam
from aws_cdk import aws_kms as kms
from aws_cdk import aws_s3 as s3
from constructs import Construct


class IamStack(Stack):
    """Instance role + profile for metal nodes, plus the scoped sweep S3 policy."""

    def __init__(
        self,
        scope: Construct,
        construct_id: str,
        transfer_bucket_name: Optional[str] = None,
        **kwargs,
    ) -> None:
        super().__init__(scope, construct_id, **kwargs)

        # The transfer bucket. If a name is supplied via context we reference the
        # existing bucket by ARN; otherwise we create one so the app is
        # self-contained. Either way the S3 policy is scoped to exactly this
        # bucket, never a wildcard across account buckets.
        if transfer_bucket_name:
            bucket_arn = f"arn:{Aws.PARTITION}:s3:::{transfer_bucket_name}"
            self.transfer_bucket = s3.Bucket.from_bucket_name(
                self, "TransferBucket", transfer_bucket_name
            )
            self.transfer_bucket_key = None
        else:
            # Customer-managed key for the transfer bucket (AWS-0132). Retained
            # on destroy, like the bucket, so retained results stay readable.
            self.transfer_bucket_key = kms.Key(
                self,
                "TransferBucketKey",
                description=(
                    "CMK for the Firecracker density lab transfer bucket"
                ),
                enable_key_rotation=True,
                removal_policy=RemovalPolicy.RETAIN,
            )

            # Server access logs target (AWS-0089). SSE-S3 by requirement: S3
            # access logging cannot deliver to SSE-KMS buckets. Retained on
            # destroy with the bucket it audits.
            access_logs_bucket = s3.Bucket(
                self,
                "TransferBucketAccessLogs",
                block_public_access=s3.BlockPublicAccess.BLOCK_ALL,
                encryption=s3.BucketEncryption.S3_MANAGED,
                enforce_ssl=True,
                versioned=True,
            )

            self.transfer_bucket = s3.Bucket(
                self,
                "TransferBucket",
                block_public_access=s3.BlockPublicAccess.BLOCK_ALL,
                encryption=s3.BucketEncryption.KMS,
                encryption_key=self.transfer_bucket_key,
                bucket_key_enabled=True,
                enforce_ssl=True,
                versioned=True,
                server_access_logs_bucket=access_logs_bucket,
                server_access_logs_prefix="transfer-bucket/",
            )
            bucket_arn = self.transfer_bucket.bucket_arn

        # Role assumed by the EC2 metal lab host via the instance profile.
        self.instance_role = iam.Role(
            self,
            "HostAgentInstanceRole",
            assumed_by=iam.ServicePrincipal("ec2.amazonaws.com"),
            description=(
                "Role for the Firecracker microVM density lab host: SSM access "
                "and scoped S3 for density-sweep results."
            ),
            managed_policies=[
                iam.ManagedPolicy.from_aws_managed_policy_name(
                    "AmazonSSMManagedInstanceCore"
                ),
            ],
        )

        # Scoped inline policy for the density sweep. Matches lab/LAB-RESOURCES.md.
        sweep_statements = [
            iam.PolicyStatement(
                sid="ReadStagedArtifacts",
                effect=iam.Effect.ALLOW,
                actions=["s3:GetObject"],
                resources=[f"{bucket_arn}/*"],
            ),
            iam.PolicyStatement(
                sid="WriteBenchmarkResults",
                effect=iam.Effect.ALLOW,
                actions=["s3:PutObject"],
                resources=[f"{bucket_arn}/benchmark_results/*"],
            ),
            iam.PolicyStatement(
                sid="ListBucketForCLI",
                effect=iam.Effect.ALLOW,
                actions=["s3:ListBucket"],
                resources=[bucket_arn],
            ),
        ]
        if self.transfer_bucket_key is not None:
            # The bucket is CMK-encrypted, so object reads need kms:Decrypt and
            # writes need kms:GenerateDataKey. Scoped to exactly this key.
            sweep_statements.append(
                iam.PolicyStatement(
                    sid="UseTransferBucketKey",
                    effect=iam.Effect.ALLOW,
                    actions=["kms:Decrypt", "kms:GenerateDataKey"],
                    resources=[self.transfer_bucket_key.key_arn],
                )
            )

        self.instance_role.attach_inline_policy(
            iam.Policy(
                self,
                "SweepS3Transfer",
                statements=sweep_statements,
            )
        )

        # Instance profile that attaches the role to launched instances.
        self.instance_profile = iam.CfnInstanceProfile(
            self,
            "HostAgentInstanceProfile",
            roles=[self.instance_role.role_name],
        )

        CfnOutput(self, "InstanceRoleArn", value=self.instance_role.role_arn)
        CfnOutput(
            self,
            "InstanceProfileArn",
            value=self.instance_profile.attr_arn,
        )
        CfnOutput(self, "TransferBucketArn", value=bucket_arn)
