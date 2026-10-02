# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""The lab reference host: one EC2 metal instance from the user's custom AMI.

This stack assumes **step 1 of the quick start is already done**: the golden
host AMI has been baked with ``scripts/`` (``provision-host.sh`` +
``bake-lab-ami.sh``) and registered under the deploying account. The AMI ID is
resolved, in order of precedence:

1. ``-c amiId=ami-...`` — an explicit override (requires ``CDK_DEFAULT_REGION``
   or an ``env`` region, because a raw AMI ID is region-specific).
2. An SSM parameter, by default ``/firecracker-microvm-density/ami/<instance-type>``,
   which step 1 registers::

       aws ssm put-parameter \\
         --name /firecracker-microvm-density/ami/m8i.metal-48xl \\
         --type String --value ami-0123456789abcdef0

   The parameter path embeds the instance type because AMIs are **not portable
   between SKUs**: kernel args and Firecracker snapshots baked on m8i are wrong
   on m8a. Registering per-SKU makes the coupling structural.

There is deliberately **no fallback to a stock AMI**. A stock Amazon Linux node
has no Firecracker, no jailer, no guest kernel, and no staged artifacts; it
would deploy cleanly and then fail only at sweep time, which is the expensive
way to find out. If neither source is available the deploy fails with a
parameter-not-found error pointing back at step 1.

The instance:

- is a ``.metal`` SKU (Firecracker needs ``/dev/kvm``, unavailable on
  virtualized instances), placed in a private subnet;
- has **no inbound access at all** — no key pair, no SSH port, a security group
  with no ingress rules. It is reached exclusively over SSM Session Manager
  through the VPC endpoints the network stack provides;
- enforces **IMDSv2** (the role carries SSM and S3 grants; requiring session
  tokens shuts down the classic SSRF credential-theft path — this complements
  the nftables rule baked into the AMI that drops guest tap traffic to IMDS);
- carries the scoped instance role from the IAM stack, so ``labsweep`` can
  upload results to ``s3://<bucket>/benchmark_results/`` and nothing else;
- receives the results bucket name at boot: user data writes
  ``/etc/firecracker-lab/env`` (an ``EnvironmentFile`` for the host-agent
  systemd unit) and ``/etc/profile.d/firecracker-lab.sh`` (for interactive SSM
  sessions), both exporting ``SWEEP_S3_BUCKET``.
"""

from __future__ import annotations

from aws_cdk import CfnOutput, Stack, Tags
from aws_cdk import aws_ec2 as ec2
from aws_cdk import aws_iam as iam
from constructs import Construct

from .config import MetalSpec

# Default SSM parameter prefix under which step 1 registers baked AMI IDs.
AMI_PARAMETER_PREFIX = "/firecracker-microvm-density/ami"


class LabHostStack(Stack):
    """One metal reference host, launched from the baked golden AMI."""

    def __init__(
        self,
        scope: Construct,
        construct_id: str,
        vpc: ec2.IVpc,
        instance_role: iam.IRole,
        metal_spec: MetalSpec,
        transfer_bucket_name: str,
        ami_id: str | None = None,
        ami_parameter: str | None = None,
        **kwargs,
    ) -> None:
        super().__init__(scope, construct_id, **kwargs)

        machine_image = self._resolve_machine_image(
            metal_spec, ami_id, ami_parameter
        )

        # No ingress: the host is driven over SSM Session Manager only. Egress
        # stays open for S3 uploads and the AWS endpoints.
        security_group = ec2.SecurityGroup(
            self,
            "LabHostSg",
            vpc=vpc,
            description=(
                "Firecracker lab reference host: no inbound, SSM-only access."
            ),
            allow_all_outbound=True,
        )

        # Boot-time wiring for the sweep and the host agent. The AMI carries the
        # binaries and artifacts; the only thing the instance cannot know until
        # launch is which bucket this deployment's results go to.
        user_data = ec2.UserData.for_linux()
        user_data.add_commands(
            "mkdir -p /etc/firecracker-lab",
            # EnvironmentFile consumed by the host-agent systemd unit.
            f'echo "SWEEP_S3_BUCKET={transfer_bucket_name}" > /etc/firecracker-lab/env',
            "chmod 0644 /etc/firecracker-lab/env",
            # Interactive SSM sessions get the same variable.
            f'echo "export SWEEP_S3_BUCKET={transfer_bucket_name}" > /etc/profile.d/firecracker-lab.sh',
            "chmod 0644 /etc/profile.d/firecracker-lab.sh",
        )

        self.instance = ec2.Instance(
            self,
            "LabHost",
            vpc=vpc,
            vpc_subnets=ec2.SubnetSelection(
                subnet_type=ec2.SubnetType.PRIVATE_WITH_EGRESS
            ),
            instance_type=metal_spec.instance_type,
            machine_image=machine_image,
            role=instance_role,
            security_group=security_group,
            user_data=user_data,
            # IMDSv2 only. The instance role carries SSM + scoped S3, so v1's
            # no-token GET is exactly the SSRF surface to close.
            require_imdsv2=True,
            detailed_monitoring=False,
            block_devices=[
                ec2.BlockDevice(
                    device_name="/dev/xvda",
                    volume=ec2.BlockDeviceVolume.ebs(
                        100,
                        volume_type=ec2.EbsDeviceVolumeType.GP3,
                        encrypted=True,
                        delete_on_termination=True,
                    ),
                )
            ],
        )
        Tags.of(self.instance).add("Name", f"fc-lab-{metal_spec.name}")

        CfnOutput(self, "InstanceId", value=self.instance.instance_id)
        CfnOutput(
            self,
            "ConnectCommand",
            value=(
                "aws ssm start-session --target "
                f"{self.instance.instance_id}"
            ),
            description="Connect to the lab host from your terminal.",
        )
        CfnOutput(
            self,
            "SweepResultsBucket",
            value=transfer_bucket_name,
            description="labsweep uploads results under benchmark_results/ here.",
        )

    def _resolve_machine_image(
        self,
        metal_spec: MetalSpec,
        ami_id: str | None,
        ami_parameter: str | None,
    ) -> ec2.IMachineImage:
        """Resolve the golden AMI; never fall back to a stock image."""
        if ami_id:
            # A raw AMI ID is region-specific, so the stack must know its
            # region at synth time to bind it.
            if not self.region or "${Token" in self.region:
                raise ValueError(
                    "-c amiId requires a concrete region: set "
                    "CDK_DEFAULT_REGION (or pass env=) so the AMI ID can be "
                    "bound, or register the SSM parameter instead (see "
                    "cdk/README.md step 1)."
                )
            return ec2.MachineImage.generic_linux({self.region: ami_id})

        parameter = ami_parameter or f"{AMI_PARAMETER_PREFIX}/{metal_spec.name}"
        # Resolved by CloudFormation at deploy time. If step 1 has not
        # registered the parameter, the deploy fails fast with a
        # parameter-not-found error instead of launching a stock node that
        # cannot run a single microVM.
        return ec2.MachineImage.from_ssm_parameter(
            parameter,
            os=ec2.OperatingSystemType.LINUX,
            # The AMI already contains its provisioning; no extra user data
            # semantics come from the image type.
            cached_in_context=False,
        )
