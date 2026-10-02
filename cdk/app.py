#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""CDK app for the Firecracker microVM density lab.

Provisions the lab infrastructure for **step 2 of the quick start**: a VPC with
SSM/S3 endpoints, the scoped IAM role and results bucket, and one EC2 metal
reference host launched from the custom golden AMI that **step 1** baked and
registered (see the repo README "Quick start" and ``cdk/README.md``).

The instance type is a CDK context parameter so the same app deploys the Intel
and AMD reference hosts:

    cdk deploy --all -c instanceType=m8a.metal-48xl

The golden AMI is resolved from the SSM parameter
``/firecracker-microvm-density/ami/<instance-type>`` registered in step 1, or
overridden explicitly with ``-c amiId=ami-...``. There is no fallback to a
stock AMI: a node without the baked Firecracker artifacts cannot run the lab.

Account and region come from the standard CDK environment variables
(``CDK_DEFAULT_ACCOUNT`` / ``CDK_DEFAULT_REGION``); nothing is hardcoded.

Production deployment on EKS (Karpenter metal node pools, host agent as a
DaemonSet) is discussed in the repo README; it is intentionally not modeled
here.
"""

from __future__ import annotations

import os

import aws_cdk as cdk

from stacks.config import DEFAULT_INSTANCE_TYPE, resolve_metal_spec
from stacks.iam_stack import IamStack
from stacks.lab_host_stack import LabHostStack
from stacks.network_stack import NetworkStack

app = cdk.App()

# --- Context parameters -------------------------------------------------------

instance_type_str = (
    app.node.try_get_context("instanceType") or DEFAULT_INSTANCE_TYPE
)
metal_spec = resolve_metal_spec(instance_type_str)

# Optional: reference an existing transfer bucket by name. Empty -> the app
# creates one. Passed as "" in cdk.json context by default.
transfer_bucket_name = app.node.try_get_context("transferBucketName") or None

# Golden AMI resolution (see lab_host_stack.py): explicit ID wins, otherwise
# the SSM parameter registered by the step-1 bake.
ami_id = app.node.try_get_context("amiId") or None
ami_parameter = app.node.try_get_context("amiParameter") or None

# Account/region from the CDK environment. No hardcoded account IDs. Treat an empty
# string the same as unset so the app stays environment-agnostic (a bare ``cdk synth``)
# instead of building a malformed ``aws:///<region>`` specification.
env = cdk.Environment(
    account=os.environ.get("CDK_DEFAULT_ACCOUNT") or None,
    region=os.environ.get("CDK_DEFAULT_REGION") or None,
)

# --- Stacks -------------------------------------------------------------------

network = NetworkStack(app, "FcDensity-Network", env=env)

iam_stack = IamStack(
    app,
    "FcDensity-Iam",
    transfer_bucket_name=transfer_bucket_name,
    env=env,
)

lab_host = LabHostStack(
    app,
    "FcDensity-LabHost",
    vpc=network.vpc,
    instance_role=iam_stack.instance_role,
    metal_spec=metal_spec,
    transfer_bucket_name=iam_stack.transfer_bucket.bucket_name,
    ami_id=ami_id,
    ami_parameter=ami_parameter,
    env=env,
)
lab_host.add_dependency(network)
lab_host.add_dependency(iam_stack)

# Tag everything for discovery and cost attribution.
cdk.Tags.of(app).add("Project", "firecracker-microvm-density")
cdk.Tags.of(app).add("InstanceType", metal_spec.name)

app.synth()
