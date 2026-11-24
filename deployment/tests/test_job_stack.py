import copy
import json
import unittest
from typing import Any

import aws_cdk as cdk
import jsonschema
from aws_cdk.assertions import Template

from stacks.job import (
    SCHEMA_PATH,
    JobStack,
    _fargate_resources,
    default_job_stack_name,
    load_job_config,
)
from stacks.shared import REPO_ROOT

EXAMPLE = REPO_ROOT / "plugin-examples" / "python" / "pyecho" / "pyecho-cloud.yml"


def synthesize(job: dict[str, Any]) -> dict[str, Any]:
    stack = JobStack(
        cdk.App(),
        "TestJob",
        job,
        sepex_stack="TestSepex",
        env=cdk.Environment(account="123456789012", region="us-east-1"),
    )
    return dict(Template.from_stack(stack).to_json())


def actions(statement: dict[str, Any]) -> list[str]:
    action = statement["Action"]
    return [action] if isinstance(action, str) else action


class FargateResourcesTest(unittest.TestCase):
    def test_minimum_cpu_and_memory_are_selected(self) -> None:
        self.assertEqual(_fargate_resources(0.1, 256), (0.25, 512))

    def test_cpu_and_memory_requests_are_rounded_to_supported_sizes(self) -> None:
        self.assertEqual(_fargate_resources(1, 256), (1.0, 2048))
        self.assertEqual(_fargate_resources(0.5, 1500), (0.5, 2048))

    def test_memory_request_can_promote_cpu_size(self) -> None:
        self.assertEqual(_fargate_resources(0.25, 4096), (0.5, 4096))

    def test_non_positive_requests_are_rejected(self) -> None:
        with self.assertRaisesRegex(ValueError, "greater than 0"):
            _fargate_resources(0, 256)
        with self.assertRaisesRegex(ValueError, "memory must be greater than 0"):
            _fargate_resources(1, 0)


class JobConfigTest(unittest.TestCase):
    def setUp(self) -> None:
        self.job = load_job_config(EXAMPLE)

    def validate_image(self, image: str) -> None:
        job = copy.deepcopy(self.job)
        job["host"]["image"] = image
        jsonschema.validate(job, json.loads(SCHEMA_PATH.read_text()))

    def test_example_is_valid(self) -> None:
        self.assertEqual(self.job["host"]["type"], "aws-batch")

    def test_entrypoint_only_jobs_allow_empty_commands(self) -> None:
        job = copy.deepcopy(self.job)
        job["command"] = []
        definition = next(
            resource["Properties"]
            for resource in synthesize(job)["Resources"].values()
            if resource["Type"] == "AWS::Batch::JobDefinition"
        )
        container = definition["ContainerProperties"]
        self.assertEqual(container.get("Command", []), [])
        self.assertEqual(container["Image"], job["host"]["image"])
        self.assertNotIn(
            "GPU",
            {requirement["Type"] for requirement in container["ResourceRequirements"]},
        )

    def test_host_environment_sets_job_definition_environment(self) -> None:
        job = copy.deepcopy(self.job)
        job["host"]["environment"] = {"AWS_REQUEST_PAYER": "requester"}
        jsonschema.validate(job, json.loads(SCHEMA_PATH.read_text()))
        definition = next(
            resource["Properties"]
            for resource in synthesize(job)["Resources"].values()
            if resource["Type"] == "AWS::Batch::JobDefinition"
        )
        self.assertEqual(
            definition["ContainerProperties"]["Environment"],
            [{"Name": "AWS_REQUEST_PAYER", "Value": "requester"}],
        )
        job["host"]["environment"] = {"AWS_REQUEST_PAYER": 1}
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(job, json.loads(SCHEMA_PATH.read_text()))

    def test_image_needs_tag_or_digest(self) -> None:
        for image in (
            "docker.io/org/app:1.0",
            "registry.local:5000/org/app:1.0",
            "123456789012.dkr.ecr.us-east-1.amazonaws.com/app@sha256:" + "a" * 64,
        ):
            self.validate_image(image)
        for image in ("org/app", "registry.local:5000/org/app"):
            with self.assertRaises(jsonschema.ValidationError):
                self.validate_image(image)

    def test_default_stack_name(self) -> None:
        self.assertEqual(
            default_job_stack_name("SepexStack", self.job), "SepexStack-pyecho-cloud"
        )

    def test_ec2_gpu_job(self) -> None:
        job = copy.deepcopy(self.job)
        job["host"]["instanceType"] = "g4dn.xlarge"
        job["config"]["maxResources"] = {"cpus": 4, "memory": 15000, "gpus": 1}
        jsonschema.validate(job, json.loads(SCHEMA_PATH.read_text()))
        template = synthesize(job)
        compute = next(
            resource["Properties"]["ComputeResources"]
            for resource in template["Resources"].values()
            if resource["Type"] == "AWS::Batch::ComputeEnvironment"
        )
        self.assertEqual(compute["Type"], "EC2")
        self.assertEqual(compute["InstanceTypes"], ["g4dn.xlarge"])
        self.assertEqual(compute["MinvCpus"], 0)
        self.assertEqual(compute["MaxvCpus"], 4)
        self.assertEqual(
            compute["Ec2Configuration"], [{"ImageType": "ECS_AL2023_NVIDIA"}]
        )
        definition = next(
            resource["Properties"]
            for resource in template["Resources"].values()
            if resource["Type"] == "AWS::Batch::JobDefinition"
        )
        self.assertEqual(definition["PlatformCapabilities"], ["EC2"])
        requirements = {
            requirement["Type"]: requirement["Value"]
            for requirement in definition["ContainerProperties"]["ResourceRequirements"]
        }
        self.assertEqual(requirements, {"VCPU": "4", "MEMORY": "15000", "GPU": "1"})

    def test_ec2_fleet_capacity_does_not_change_job_resources(self) -> None:
        self.job["host"].update(instanceType="g4dn.xlarge", maxvCpus=40)
        self.job["config"]["maxResources"] = {"cpus": 4, "memory": 15000, "gpus": 1}
        jsonschema.validate(self.job, json.loads(SCHEMA_PATH.read_text()))
        template = synthesize(self.job)
        compute = next(
            resource["Properties"]["ComputeResources"]
            for resource in template["Resources"].values()
            if resource["Type"] == "AWS::Batch::ComputeEnvironment"
        )
        self.assertEqual(compute["MaxvCpus"], 40)
        self.assertEqual(compute["MinvCpus"], 0)
        definition = next(
            resource["Properties"]
            for resource in template["Resources"].values()
            if resource["Type"] == "AWS::Batch::JobDefinition"
        )
        requirements = {
            requirement["Type"]: requirement["Value"]
            for requirement in definition["ContainerProperties"]["ResourceRequirements"]
        }
        self.assertEqual(requirements, {"VCPU": "4", "MEMORY": "15000", "GPU": "1"})

    def test_fleet_capacity_requires_ec2(self) -> None:
        self.job["host"]["maxvCpus"] = 40
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(self.job, json.loads(SCHEMA_PATH.read_text()))

    def test_fleet_capacity_requires_positive_integer(self) -> None:
        self.job["host"]["instanceType"] = "g4dn.xlarge"
        self.job["config"]["maxResources"]["cpus"] = 4
        for capacity in (0, -1, 1.5, "40", True):
            with self.subTest(capacity=capacity):
                self.job["host"]["maxvCpus"] = capacity
                with self.assertRaises(jsonschema.ValidationError):
                    jsonschema.validate(self.job, json.loads(SCHEMA_PATH.read_text()))

    def test_gpu_requires_ec2(self) -> None:
        self.job["config"]["maxResources"]["gpus"] = 1
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(self.job, json.loads(SCHEMA_PATH.read_text()))
        with self.assertRaisesRegex(ValueError, "requires host.instanceType"):
            synthesize(self.job)

    def test_ec2_without_gpu_uses_standard_image(self) -> None:
        self.job["host"]["instanceType"] = "c6i.xlarge"
        self.job["config"]["maxResources"] = {"cpus": 1, "memory": 1000}
        jsonschema.validate(self.job, json.loads(SCHEMA_PATH.read_text()))
        template = synthesize(self.job)
        compute = next(
            resource["Properties"]["ComputeResources"]
            for resource in template["Resources"].values()
            if resource["Type"] == "AWS::Batch::ComputeEnvironment"
        )
        self.assertEqual(compute["Ec2Configuration"], [{"ImageType": "ECS_AL2023"}])
        definition = next(
            resource["Properties"]
            for resource in template["Resources"].values()
            if resource["Type"] == "AWS::Batch::JobDefinition"
        )
        requirements = {
            requirement["Type"]: requirement["Value"]
            for requirement in definition["ContainerProperties"]["ResourceRequirements"]
        }
        self.assertEqual(requirements, {"VCPU": "1", "MEMORY": "1000"})

    def test_ec2_requires_whole_cpu_and_positive_gpu_requests(self) -> None:
        self.job["host"]["instanceType"] = "g4dn.xlarge"
        for resources in (
            {"cpus": 0.5, "memory": 2048},
            {"cpus": 1, "memory": 2048, "gpus": 0},
            {"cpus": 1, "memory": 2048, "gpus": 0.5},
        ):
            with self.subTest(resources=resources):
                self.job["config"]["maxResources"] = resources
                with self.assertRaises(jsonschema.ValidationError):
                    jsonschema.validate(self.job, json.loads(SCHEMA_PATH.read_text()))


class JobStackTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.template = synthesize(load_job_config(EXAMPLE))

    def test_vpc_comes_from_sepex_ssm_parameters(self) -> None:
        defaults = {
            parameter.get("Default")
            for parameter in self.template["Parameters"].values()
        }
        self.assertLessEqual(
            {
                "/sepex/TestSepex/vpc-id",
                "/sepex/TestSepex/private-subnet-ids",
            },
            defaults,
        )
        compute = next(
            resource
            for resource in self.template["Resources"].values()
            if resource["Type"] == "AWS::Batch::ComputeEnvironment"
        )
        self.assertIn(
            "Fn::Split", str(compute["Properties"]["ComputeResources"]["Subnets"])
        )

    def test_job_definition_uses_config(self) -> None:
        definition = next(
            resource["Properties"]
            for resource in self.template["Resources"].values()
            if resource["Type"] == "AWS::Batch::JobDefinition"
        )
        self.assertEqual(definition["JobDefinitionName"], "pyecho-cloud")
        container = definition["ContainerProperties"]
        self.assertEqual(container["Command"], ["python", "main.py"])
        requirements = {
            requirement["Type"]: requirement["Value"]
            for requirement in container["ResourceRequirements"]
        }
        self.assertEqual(float(requirements["VCPU"]), 0.25)
        self.assertEqual(requirements["MEMORY"], "512")

    def test_job_role_can_read_and_write_any_bucket(self) -> None:
        definition = next(
            resource["Properties"]
            for resource in self.template["Resources"].values()
            if resource["Type"] == "AWS::Batch::JobDefinition"
        )
        container = definition["ContainerProperties"]
        role_id = container["JobRoleArn"]["Fn::GetAtt"][0]
        role = self.template["Resources"][role_id]
        self.assertIn(
            "ecs-tasks.amazonaws.com",
            str(role["Properties"]["AssumeRolePolicyDocument"]),
        )

        statements = [
            statement
            for resource in self.template["Resources"].values()
            if resource["Type"] == "AWS::IAM::Policy"
            for statement in resource["Properties"]["PolicyDocument"]["Statement"]
        ]
        self.assertTrue(
            any("s3:ListAllMyBuckets" in actions(statement) for statement in statements)
        )
        object_write = next(
            statement
            for statement in statements
            if "s3:PutObject" in actions(statement)
        )
        self.assertIn("s3:::*/*", str(object_write["Resource"]))
        s3_statements = [
            statement
            for statement in statements
            if any(action.startswith("s3:") for action in actions(statement))
            and "s3:ListAllMyBuckets" not in actions(statement)
        ]
        self.assertTrue(s3_statements)
        self.assertTrue(
            all(
                "Condition" not in statement for statement in s3_statements
            )
        )
        self.assertTrue(
            {"s3:GetObject", "s3:ListBucket", "s3:PutObject"}
            <= {action for statement in s3_statements for action in actions(statement)}
        )
        self.assertNotIn("artifacts-bucket", str(self.template))


if __name__ == "__main__":
    unittest.main()
