import json
import math
import re
from pathlib import Path

import jsonschema
import yaml
from aws_cdk import (
    Annotations,
    CfnOutput,
    Environment,
    Fn,
    Size,
    Stack,
    aws_batch,
    aws_ec2,
    aws_ecs,
    aws_iam,
    aws_ssm,
)
from constructs import Construct

from stacks.shared import (
    AZ_COUNT,
    REPO_ROOT,
    grant_bucket_read_write,
    parameter_name,
)

SCHEMA_PATH = REPO_ROOT / "schemas" / "aws-batch-job.schema.json"

# (vCPUs, minimum MiB, maximum MiB, MiB increment) for each Fargate CPU size.
_FARGATE_SIZINGS = (
    (0.25, 512, 2048, 512),
    (0.5, 1024, 4096, 1024),
    (1.0, 2048, 8192, 1024),
    (2.0, 4096, 16384, 1024),
    (4.0, 8192, 30720, 1024),
    (8.0, 16384, 61440, 4096),
    (16.0, 32768, 122880, 8192),
)


def _fargate_resources(cpus: float, memory_mib: int) -> tuple[float, int]:
    if cpus <= 0:
        raise ValueError("config.maxResources.cpus must be greater than 0")
    if memory_mib <= 0:
        raise ValueError("config.maxResources.memory must be greater than 0")

    for vcpus, min_memory, max_memory, memory_increment in _FARGATE_SIZINGS:
        if vcpus < cpus:
            continue
        allocated_memory = max(memory_mib, min_memory)
        allocated_memory = (
            min_memory
            + math.ceil((allocated_memory - min_memory) / memory_increment)
            * memory_increment
        )
        if allocated_memory <= max_memory:
            return vcpus, allocated_memory

    raise ValueError("config.maxResources exceeds the supported AWS Fargate size")


def load_job_config(path: Path) -> dict:
    """Read a SEPEX process YAML and validate it against the AWS Batch job schema."""
    with path.open(encoding="utf-8") as f:
        job = yaml.safe_load(f)
    jsonschema.validate(instance=job, schema=json.loads(SCHEMA_PATH.read_text()))
    return job


def default_job_stack_name(sepex_stack: str, job: dict) -> str:
    return re.sub(r"[^A-Za-z0-9-]", "-", f"{sepex_stack}-{job['info']['id']}")


class JobStack(Stack):
    """Deploy an AWS Batch job for a SEPEX process into an existing SEPEX VPC."""

    def __init__(
        self,
        scope: Construct,
        construct_id: str,
        job: dict,
        *,
        sepex_stack: str,
        env: Environment | None = None,
    ) -> None:
        super().__init__(scope, construct_id, env=env)

        # Resolved by CloudFormation at deploy time from parameters the SEPEX stack publishes.
        def published(key: str) -> str:
            return aws_ssm.StringParameter.value_for_string_parameter(
                self, parameter_name(sepex_stack, key)
            )

        vpc = aws_ec2.Vpc.from_vpc_attributes(
            self,
            "SepexVpc",
            vpc_id=published("vpc-id"),
            # Required by the API but unused by Batch, which only needs the subnet IDs.
            availability_zones=[Fn.select(i, Fn.get_azs()) for i in range(AZ_COUNT)],
            private_subnet_ids=Fn.split(",", published("private-subnet-ids"), AZ_COUNT),
        )
        # Batch never reads the subnets' route tables, so the import does not need them.
        Annotations.of(self).acknowledge_warning(
            "@aws-cdk/aws-ec2:noSubnetRouteTableId",
            "Batch only uses subnet IDs.",
        )

        host = job["host"]
        resources = job["config"]["maxResources"]
        instance_type = host.get("instanceType")
        gpu = resources.get("gpus", 0)
        if gpu and not instance_type:
            raise ValueError("config.maxResources.gpus requires host.instanceType")
        compute_env: aws_batch.IComputeEnvironment
        if instance_type:
            compute_env = aws_batch.ManagedEc2EcsComputeEnvironment(
                self,
                "Ec2ComputeEnvironment",
                vpc=vpc,
                instance_types=[aws_ec2.InstanceType(instance_type)],
                use_optimal_instance_classes=False,
                images=[
                    aws_batch.EcsMachineImage(
                        image_type=(
                            aws_batch.EcsMachineImageType.ECS_AL2023_NVIDIA
                            if gpu
                            else aws_batch.EcsMachineImageType.ECS_AL2023
                        )
                    )
                ],
                minv_cpus=0,
                maxv_cpus=host.get("maxvCpus", max(4, math.ceil(resources["cpus"]))),
                allocation_strategy=aws_batch.AllocationStrategy.BEST_FIT_PROGRESSIVE,
                vpc_subnets=aws_ec2.SubnetSelection(
                    subnet_type=aws_ec2.SubnetType.PRIVATE_WITH_EGRESS
                ),
            )
        else:
            compute_env = aws_batch.FargateComputeEnvironment(
                self,
                "FargateComputeEnvironment",
                vpc=vpc,
                vpc_subnets=aws_ec2.SubnetSelection(
                    subnet_type=aws_ec2.SubnetType.PRIVATE_WITH_EGRESS
                ),
            )

        aws_batch.JobQueue(
            self,
            "JobQueue",
            job_queue_name=host["jobQueue"],
            compute_environments=[
                aws_batch.OrderedComputeEnvironment(
                    compute_environment=compute_env, order=1
                )
            ],
        )

        cpu, memory = (
            (resources["cpus"], resources["memory"])
            if instance_type
            else _fargate_resources(resources["cpus"], resources["memory"])
        )
        job_role = aws_iam.Role(
            self,
            "JobRole",
            assumed_by=aws_iam.ServicePrincipal(
                "ecs-tasks.amazonaws.com"
            ),  # pyright: ignore[reportArgumentType]
        )
        grant_bucket_read_write(job_role, self.partition)

        container: (
            aws_batch.EcsEc2ContainerDefinition | aws_batch.EcsFargateContainerDefinition
        )
        if instance_type:
            container = aws_batch.EcsEc2ContainerDefinition(
                self,
                "containerDefn",
                image=aws_ecs.ContainerImage.from_registry(host["image"]),
                memory=Size.mebibytes(memory),
                cpu=cpu,
                gpu=gpu or None,
                command=job["command"],
                environment=host.get("environment"),
                job_role=job_role,  # pyright: ignore[reportArgumentType]
            )
        else:
            container = aws_batch.EcsFargateContainerDefinition(
                self,
                "containerDefn",
                image=aws_ecs.ContainerImage.from_registry(host["image"]),
                memory=Size.mebibytes(memory),
                cpu=cpu,
                command=job["command"],
                environment=host.get("environment"),
                job_role=job_role,  # pyright: ignore[reportArgumentType]
            )
        container.execution_role.add_managed_policy(
            aws_iam.ManagedPolicy.from_aws_managed_policy_name(
                "service-role/AmazonECSTaskExecutionRolePolicy"
            )
        )

        # Accepts a bare name, name:revision, or ARN; CloudFormation assigns the revision.
        job_definition_name = host["jobDefinition"].rsplit("/", 1)[-1].split(":", 1)[0]
        job_definition = aws_batch.EcsJobDefinition(
            self,
            "JobDefinition",
            job_definition_name=job_definition_name,
            container=container,
        )
        CfnOutput(self, "JobDefinitionArn", value=job_definition.job_definition_arn)
