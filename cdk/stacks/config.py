# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Shared configuration and context parsing.

Pinned versions here mirror ``scripts/lib/common.sh`` in the repo root. They are
duplicated deliberately: ``cdk/`` is published as part of a standalone
aws-samples tree and must not read from sibling components at synth time. When
``scripts/lib/common.sh`` moves a pin, update the constant here to match.
"""

from __future__ import annotations

from dataclasses import dataclass

from aws_cdk import aws_ec2 as ec2

# --- Pinned artifact versions (kept in sync with scripts/lib/common.sh) -------

# Firecracker binary version. Snapshots only resume on the version that produced
# them, so changing this invalidates every previously captured snapshot.
FIRECRACKER_VERSION = "v1.16.1"

# Guest kernel version. Supported from Firecracker v1.16.1 through 2028-06-01.
GUEST_KERNEL_VERSION = "6.18"

# --- Supported instance types -------------------------------------------------
#
# Both M8 metal variants are 192 vCPU. They differ in SMT: m8i is 96 physical
# cores x 2 threads, m8a is 192 physical cores x 1 thread. SMT tuning applies to
# m8i only. See the repo README "Instance types" section.
#
# aws-cdk-lib exposes the 48xl metal size as InstanceSize.XLARGE48METAL (not
# METAL48XL, which does not exist in the enum). Verified against aws-cdk-lib
# 2.255.0: InstanceType.of(InstanceClass.M8I, InstanceSize.XLARGE48METAL) renders
# "m8i.metal-48xl".

_INSTANCE_CLASSES = {
    "m8i.metal-48xl": ec2.InstanceClass.M8I,
    "m8a.metal-48xl": ec2.InstanceClass.M8A,
}

DEFAULT_INSTANCE_TYPE = "m8i.metal-48xl"


@dataclass(frozen=True)
class MetalSpec:
    """Static facts about a supported metal SKU, used to size infrastructure."""

    name: str
    instance_type: ec2.InstanceType
    vcpus: int
    physical_cores: int
    threads_per_core: int

    @property
    def has_smt(self) -> bool:
        return self.threads_per_core > 1


_SPECS = {
    "m8i.metal-48xl": dict(vcpus=192, physical_cores=96, threads_per_core=2),
    "m8a.metal-48xl": dict(vcpus=192, physical_cores=192, threads_per_core=1),
}


def resolve_metal_spec(instance_type_str: str) -> MetalSpec:
    """Map a context instance-type string to a validated ``MetalSpec``.

    Raises ``ValueError`` for an unsupported SKU rather than silently building a
    non-metal instance type, since Firecracker requires ``/dev/kvm`` and that
    needs a ``.metal`` instance.
    """
    key = instance_type_str.strip().lower()
    if key not in _INSTANCE_CLASSES:
        supported = ", ".join(sorted(_INSTANCE_CLASSES))
        raise ValueError(
            f"unsupported instanceType {instance_type_str!r}; "
            f"supported values are: {supported}"
        )

    instance_type = ec2.InstanceType.of(
        _INSTANCE_CLASSES[key], ec2.InstanceSize.XLARGE48METAL
    )
    spec = _SPECS[key]
    return MetalSpec(name=key, instance_type=instance_type, **spec)
