# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""CDK stacks for the Firecracker microVM density lab.

Three stacks, deployed in dependency order:

- ``NetworkStack``  VPC across two AZs, private subnets, SSM/S3/CloudWatch
                    VPC endpoints. No inbound path exists anywhere.
- ``IamStack``      Instance role + profile the lab host runs under, and the
                    scoped S3 policy the density sweep uses to publish results.
- ``LabHostStack``  One EC2 ``.metal`` reference host launched from the custom
                    golden AMI baked and registered in step 1 of the quick
                    start (``scripts/``). Reached over SSM only.

The instance type is a CDK context parameter (``-c instanceType=...``) so the
same app deploys the Intel (``m8i.metal-48xl``) and AMD (``m8a.metal-48xl``)
reference hosts without code changes.

Production deployment on EKS with Karpenter metal node pools is discussed in
the repo README and intentionally not modeled here.
"""
