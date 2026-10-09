"""CDK entry point.

Synthesizes the SEPEX stack, or a job stack instead when `-c jobConfig=<process.yml>` is set.
Context: sepexStack (default in cdk.json), jobConfig, jobStack (default <sepexStack>-<info.id>).
"""

import os
from pathlib import Path

import aws_cdk as cdk

from stacks.job import JobStack, default_job_stack_name, load_job_config
from stacks.sepex import SepexStack

app = cdk.App()
# The CDK CLI sets these from the active AWS credentials and region.
env = cdk.Environment(
    account=os.environ.get("CDK_DEFAULT_ACCOUNT"),
    region=os.environ.get("CDK_DEFAULT_REGION"),
)
sepex_stack = app.node.get_context("sepexStack")
job_config = app.node.try_get_context("jobConfig")

if job_config:
    job = load_job_config(Path(job_config))
    JobStack(
        app,
        app.node.try_get_context("jobStack")
        or default_job_stack_name(sepex_stack, job),
        job,
        sepex_stack=sepex_stack,
        env=env,
    )
else:
    SepexStack(
        app,
        sepex_stack,
        existing_vpc_id=os.environ.get("SEPEX_VPC_ID") or None,
        env=env,
    )

app.synth()
