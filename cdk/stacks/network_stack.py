# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Network foundation: a VPC for the lab reference host.

Two AZs, each with a public subnet (for the NAT gateway) and a private subnet
(where the metal host runs). Interface and gateway VPC endpoints keep SSM
session traffic, S3 result uploads, and CloudWatch telemetry on the AWS
network so the private host does not depend on the NAT gateway for them.

The lab host has no inbound path at all: no key pair, no SSH, no load
balancer. Everything goes over SSM Session Manager through these endpoints.
"""

from __future__ import annotations

from aws_cdk import RemovalPolicy, Stack
from aws_cdk import aws_ec2 as ec2
from aws_cdk import aws_kms as kms
from aws_cdk import aws_logs as logs
from constructs import Construct


class NetworkStack(Stack):
    """VPC across two AZs with public/private subnets and VPC endpoints."""

    def __init__(self, scope: Construct, construct_id: str, **kwargs) -> None:
        super().__init__(scope, construct_id, **kwargs)

        self.vpc = ec2.Vpc(
            self,
            "Vpc",
            max_azs=2,
            nat_gateways=1,
            ip_addresses=ec2.IpAddresses.cidr("10.0.0.0/16"),
            subnet_configuration=[
                ec2.SubnetConfiguration(
                    name="public",
                    subnet_type=ec2.SubnetType.PUBLIC,
                    cidr_mask=20,
                ),
                ec2.SubnetConfiguration(
                    name="private",
                    subnet_type=ec2.SubnetType.PRIVATE_WITH_EGRESS,
                    cidr_mask=18,
                ),
            ],
        )

        # Gateway endpoint for S3: staged binaries down, sweep results up,
        # travelling over the AWS network rather than the NAT gateway.
        self.vpc.add_gateway_endpoint(
            "S3Endpoint",
            service=ec2.GatewayVpcEndpointAwsService.S3,
        )

        # Interface endpoints for the services the metal host talks to from a
        # private subnet: SSM (Session Manager — the only way onto the host)
        # and CloudWatch for logs/metrics if the operator wires them up.
        interface_services = {
            "SsmEndpoint": ec2.InterfaceVpcEndpointAwsService.SSM,
            "SsmMessagesEndpoint": ec2.InterfaceVpcEndpointAwsService.SSM_MESSAGES,
            "Ec2MessagesEndpoint": ec2.InterfaceVpcEndpointAwsService.EC2_MESSAGES,
            "LogsEndpoint": ec2.InterfaceVpcEndpointAwsService.CLOUDWATCH_LOGS,
            "MonitoringEndpoint": ec2.InterfaceVpcEndpointAwsService.CLOUDWATCH_MONITORING,
        }
        for logical_id, service in interface_services.items():
            self.vpc.add_interface_endpoint(
                logical_id,
                service=service,
                subnets=ec2.SubnetSelection(
                    subnet_type=ec2.SubnetType.PRIVATE_WITH_EGRESS
                ),
                private_dns_enabled=True,
            )

        # VPC Flow Logs (security scan finding AWS-0178). Captures accepted and
        # rejected traffic to CloudWatch Logs so a compromised or misbehaving
        # host leaves a network audit trail. One month of retention is enough
        # for a lab; the log group and its key are deleted with the stack.
        flow_log_key = kms.Key(
            self,
            "VpcFlowLogKey",
            description="CMK for the Firecracker density lab VPC flow log group",
            enable_key_rotation=True,
            removal_policy=RemovalPolicy.DESTROY,
        )
        flow_log_group = logs.LogGroup(
            self,
            "VpcFlowLogGroup",
            encryption_key=flow_log_key,
            retention=logs.RetentionDays.ONE_MONTH,
            removal_policy=RemovalPolicy.DESTROY,
        )
        self.vpc.add_flow_log(
            "FlowLog",
            destination=ec2.FlowLogDestination.to_cloud_watch_logs(flow_log_group),
            traffic_type=ec2.FlowLogTrafficType.ALL,
        )
