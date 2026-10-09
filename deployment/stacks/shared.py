"""Names and values shared by the SEPEX stack, job stacks, and scripts."""

from pathlib import Path

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
