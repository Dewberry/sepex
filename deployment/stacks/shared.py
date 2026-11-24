"""Names and values shared by the SEPEX stack, job stacks, and scripts."""

from pathlib import Path

from aws_cdk import aws_iam

DEPLOYMENT_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = DEPLOYMENT_DIR.parent

# SEPEX names AWS Batch jobs "{API_NAME}_{jobID}"; the status callback filters on this prefix.
API_NAME = "sepex"
# Job stacks import the SEPEX VPC from SSM, which needs the subnet count at synth time.
AZ_COUNT = 2
# CloudFormation AllowedPattern for a digest-pinned image; registry ports are allowed.
DIGEST_IMAGE_PATTERN = r"[^\s@]+@sha256:[a-f0-9]{64}"


def parameter_name(sepex_stack: str, key: str) -> str:
    """SSM parameter the SEPEX stack publishes for scripts and job stacks."""
    return f"/sepex/{sepex_stack}/{key}"


def grant_account_bucket_read(
    role: aws_iam.Role, account_id: str, partition: str
) -> None:
    """Allow listing and reading S3 buckets owned by this account."""
    role.add_to_policy(
        aws_iam.PolicyStatement(
            actions=["s3:GetBucketLocation", "s3:ListBucket"],
            resources=[f"arn:{partition}:s3:::*"],
            conditions={"StringEquals": {"s3:ResourceAccount": account_id}},
        )
    )
    role.add_to_policy(
        aws_iam.PolicyStatement(
            actions=["s3:GetObject", "s3:GetObjectVersion"],
            resources=[f"arn:{partition}:s3:::*/*"],
            conditions={"StringEquals": {"s3:ResourceAccount": account_id}},
        )
    )


def grant_bucket_read_write(role: aws_iam.Role, partition: str) -> None:
    """Allow reading and writing S3 buckets when their policies permit access."""
    role.add_to_policy(
        aws_iam.PolicyStatement(
            actions=["s3:GetBucketLocation", "s3:ListBucket"],
            resources=[f"arn:{partition}:s3:::*"],
        )
    )
    role.add_to_policy(
        aws_iam.PolicyStatement(actions=["s3:ListAllMyBuckets"], resources=["*"])
    )
    role.add_to_policy(
        aws_iam.PolicyStatement(
            actions=["s3:ListBucketMultipartUploads"],
            resources=[f"arn:{partition}:s3:::*"],
        )
    )
    role.add_to_policy(
        aws_iam.PolicyStatement(
            actions=[
                "s3:AbortMultipartUpload",
                "s3:DeleteObject",
                "s3:DeleteObjectVersion",
                "s3:GetObject",
                "s3:GetObjectVersion",
                "s3:ListMultipartUploadParts",
                "s3:PutObject",
            ],
            resources=[f"arn:{partition}:s3:::*/*"],
        )
    )
