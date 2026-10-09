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


if __name__ == "__main__":
    unittest.main()
