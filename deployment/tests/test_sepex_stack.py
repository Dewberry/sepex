import re
import unittest
from typing import Any
from unittest.mock import patch

import aws_cdk as cdk
from aws_cdk import aws_ec2
from aws_cdk.assertions import Template

from stacks.sepex import SepexStack
from stacks.shared import DIGEST_IMAGE_PATTERN

DIGEST = "a" * 64


def synthesize(
    existing_vpc_id: str | None = None,
    private_subnet_cidrs: str | None = None,
    nat_gateway_id: str | None = None,
) -> dict[str, Any]:
    stack = SepexStack(
        cdk.App(),
        "TestStack",
        existing_vpc_id=existing_vpc_id,
        private_subnet_cidrs=private_subnet_cidrs,
        nat_gateway_id=nat_gateway_id,
        env=cdk.Environment(account="123456789012", region="us-east-1"),
    )
    return dict(Template.from_stack(stack).to_json())


def resources_of(template: dict[str, Any], resource_type: str) -> list[dict]:
    return [
        resource
        for resource in template["Resources"].values()
        if resource["Type"] == resource_type
    ]


def policy_statements(template: dict[str, Any]) -> list[dict]:
    return [
        statement
        for policy in resources_of(template, "AWS::IAM::Policy")
        for statement in policy["Properties"]["PolicyDocument"]["Statement"]
    ]


def actions(statement: dict) -> list[str]:
    action = statement["Action"]
    return [action] if isinstance(action, str) else action


def import_vpc(scope: Any, construct_id: str, *, vpc_id: str) -> aws_ec2.IVpc:
    return aws_ec2.Vpc.from_vpc_attributes(
        scope,
        construct_id,
        vpc_id=vpc_id,
        vpc_cidr_block="172.31.0.0/16",
        availability_zones=["us-east-1a", "us-east-1b"],
    )


class SepexStackTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.template = synthesize()
        cls.user_data = str(
            resources_of(cls.template, "AWS::EC2::Instance")[0]["Properties"][
                "UserData"
            ]
        )

    def test_private_database_and_deletable_artifacts(self) -> None:
        database = resources_of(self.template, "AWS::RDS::DBInstance")[0]
        self.assertFalse(database["Properties"]["PubliclyAccessible"])
        self.assertTrue(database["Properties"]["StorageEncrypted"])
        self.assertFalse(database["Properties"]["DeletionProtection"])
        self.assertEqual(database["Properties"]["DBInstanceClass"], "db.t4g.micro")
        self.assertEqual(database["DeletionPolicy"], "Delete")
        bucket = resources_of(self.template, "AWS::S3::Bucket")[0]
        self.assertEqual(bucket["DeletionPolicy"], "Delete")
        self.assertEqual(
            bucket["Properties"]["VersioningConfiguration"]["Status"], "Enabled"
        )
        self.assertTrue(resources_of(self.template, "Custom::S3AutoDeleteObjects"))
        self.assertTrue(
            bucket["Properties"]["PublicAccessBlockConfiguration"]["BlockPublicAcls"]
        )

    def test_database_subnets_span_two_availability_zones(self) -> None:
        subnet_group = resources_of(self.template, "AWS::RDS::DBSubnetGroup")[0]
        self.assertEqual(len(subnet_group["Properties"]["SubnetIds"]), 2)

    def test_existing_vpc_owns_only_new_subnets_and_routes(self) -> None:
        with patch("stacks.sepex.aws_ec2.Vpc.from_lookup", side_effect=import_vpc):
            template = synthesize(
                "vpc-0123456789abcdef0",
                "172.31.0.0/24,172.31.1.0/24",
                "nat-0123456789abcdef0",
            )

        for resource_type in (
            "AWS::EC2::VPC",
            "AWS::EC2::NatGateway",
            "AWS::EC2::VPCEndpoint",
        ):
            self.assertEqual(resources_of(template, resource_type), [])
        subnets = resources_of(template, "AWS::EC2::Subnet")
        self.assertEqual(len(subnets), 2)
        self.assertEqual(
            {subnet["Properties"]["CidrBlock"] for subnet in subnets},
            {"172.31.0.0/24", "172.31.1.0/24"},
        )
        self.assertTrue(
            all(
                subnet.get("DeletionPolicy", "Delete") == "Delete" for subnet in subnets
            )
        )
        routes = resources_of(template, "AWS::EC2::Route")
        self.assertEqual(len(routes), 2)
        self.assertTrue(
            all(
                route["Properties"]["NatGatewayId"] == "nat-0123456789abcdef0"
                for route in routes
            )
        )
        instance = resources_of(template, "AWS::EC2::Instance")[0]
        route_ids = {
            logical_id
            for logical_id, resource in template["Resources"].items()
            if resource["Type"] == "AWS::EC2::Route"
        }
        self.assertTrue(route_ids.issubset(set(instance["DependsOn"])))
        subnet_group = resources_of(template, "AWS::RDS::DBSubnetGroup")[0]
        self.assertEqual(len(subnet_group["Properties"]["SubnetIds"]), 2)

    def test_existing_vpc_requires_explicit_network_inputs(self) -> None:
        with self.assertRaisesRegex(ValueError, "requires SEPEX_PRIVATE_SUBNET_CIDRS"):
            synthesize("vpc-0123456789abcdef0")
        with patch("stacks.sepex.aws_ec2.Vpc.from_lookup", side_effect=import_vpc):
            for cidrs, error in (
                ("172.31.0.0/24", "must contain 2 CIDRs"),
                ("172.31.0.0/24,172.31.0.0/25", "must not overlap"),
                ("172.32.0.0/24,172.31.1.0/24", "within the VPC CIDR"),
            ):
                with (
                    self.subTest(cidrs=cidrs),
                    self.assertRaisesRegex(ValueError, error),
                ):
                    synthesize("vpc-0123456789abcdef0", cidrs, "nat-0123456789abcdef0")

    def test_instance_requires_imdsv2(self) -> None:
        instance = resources_of(self.template, "AWS::EC2::Instance")[0]
        self.assertEqual(
            instance["Properties"]["MetadataOptions"],
            {"HttpTokens": "required", "HttpPutResponseHopLimit": 2},
        )

    def test_api_image_is_a_digest_pinned_parameter(self) -> None:
        pattern = self.template["Parameters"]["ApiImage"]["AllowedPattern"]
        for image in (
            f"ghcr.io/org/sepex@sha256:{DIGEST}",
            f"registry.local:5000/sepex@sha256:{DIGEST}",
        ):
            self.assertRegex(image, f"^{pattern}$")
        for image in ("ghcr.io/org/sepex:latest", "ghcr.io/org/sepex"):
            self.assertIsNone(re.fullmatch(DIGEST_IMAGE_PATTERN, image))

    def test_boot_installs_start_script_that_reads_image_from_ssm(self) -> None:
        self.assertIn("canonical/ubuntu/server/24.04", str(self.template["Parameters"]))
        self.assertIn("apt-get install -y docker.io python3 curl unzip", self.user_data)
        self.assertIn("/tmp/aws/install", self.user_data)
        self.assertIn("SEPEX_BATCH_LOG_GROUP=/aws/batch/job", self.user_data)
        self.assertIn("aws ssm get-parameter", self.user_data)
        self.assertIn("/opt/sepex/start-api.sh", self.user_data)
        self.assertNotIn(DIGEST, self.user_data)

    def test_publishes_ssm_parameters_for_scripts_and_job_stacks(self) -> None:
        names = {
            parameter["Properties"]["Name"]
            for parameter in resources_of(self.template, "AWS::SSM::Parameter")
        }
        self.assertEqual(
            names,
            {
                "/sepex/TestStack/api-image",
                "/sepex/TestStack/vpc-id",
                "/sepex/TestStack/private-subnet-ids",
            },
        )

    def test_batch_logs_are_readable(self) -> None:
        self.assertTrue(
            any(
                "logs:GetLogEvents" in actions(statement)
                and "/aws/batch/job" in str(statement["Resource"])
                for statement in policy_statements(self.template)
            )
        )

    def test_api_can_list_and_read_account_buckets(self) -> None:
        statements = policy_statements(self.template)
        list_buckets = next(
            statement
            for statement in statements
            if "s3:ListAllMyBuckets" in actions(statement)
        )
        bucket_read = next(
            statement
            for statement in statements
            if "s3:ListBucket" in actions(statement)
        )
        object_read = next(
            statement
            for statement in statements
            if "s3:GetObject" in actions(statement)
        )

        self.assertEqual(list_buckets["Resource"], "*")
        for statement, resource in (
            (bucket_read, "s3:::*"),
            (object_read, "s3:::*/*"),
        ):
            self.assertIn(resource, str(statement["Resource"]))
            self.assertEqual(
                statement["Condition"],
                {"StringEquals": {"s3:ResourceAccount": "123456789012"}},
            )

    def test_batch_permissions_are_scoped(self) -> None:
        by_action = {
            action: statement["Resource"]
            for statement in policy_statements(self.template)
            for action in actions(statement)
            if action.startswith("batch:")
        }
        self.assertEqual(
            set(by_action),
            {
                "batch:SubmitJob",
                "batch:TerminateJob",
                "batch:CancelJob",
                "batch:DescribeJobs",
                "batch:DescribeJobDefinitions",
            },
        )
        for action in ("batch:SubmitJob", "batch:TerminateJob", "batch:CancelJob"):
            self.assertNotEqual(by_action[action], "*")
        self.assertIn(":job-queue/*", str(by_action["batch:SubmitJob"]))
        self.assertIn(":job/*", str(by_action["batch:CancelJob"]))

    def test_callback_log_group_can_be_recreated(self) -> None:
        log_group = resources_of(self.template, "AWS::Logs::LogGroup")[0]
        self.assertNotIn("LogGroupName", log_group["Properties"])
        self.assertEqual(log_group["DeletionPolicy"], "Delete")
        function = resources_of(self.template, "AWS::Lambda::Function")[0]
        self.assertNotIn("FunctionName", function["Properties"])

    def test_callbacks_only_for_sepex_jobs(self) -> None:
        rule = resources_of(self.template, "AWS::Events::Rule")[0]
        self.assertEqual(
            rule["Properties"]["EventPattern"]["detail"],
            {
                "jobName": [{"prefix": "sepex_"}],
                "status": ["RUNNING", "SUCCEEDED", "FAILED"],
            },
        )

    def test_ssh_is_not_configured_by_cdk_and_port_22_stays_closed(self) -> None:
        instance = resources_of(self.template, "AWS::EC2::Instance")[0]
        self.assertNotIn("KeyName", instance["Properties"])
        ingress_rules = []
        for resource in self.template["Resources"].values():
            properties = resource.get("Properties", {})
            ingress_rules.extend(properties.get("SecurityGroupIngress", []))
            if resource["Type"] == "AWS::EC2::SecurityGroupIngress":
                ingress_rules.append(properties)
        self.assertFalse(any(rule.get("FromPort") == 22 for rule in ingress_rules))


if __name__ == "__main__":
    unittest.main()
