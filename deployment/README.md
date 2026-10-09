# Deploy SEPEX on AWS

> Skeleton: each bullet is a point to turn into prose. Commands are final.

## Summary

This Python [AWS CDK](https://docs.aws.amazon.com/cdk/v2/guide/home.html) app deploys SEPEX and its AWS Batch workloads. The SEPEX stack provides the private network, API server, database, and artifact storage; each process can have its own job stack with a Batch queue and job definition in that network.

Use the scripts in [scripts/](scripts/) to deploy, update, connect to, and remove stacks without invoking CDK directly. The API has no public endpoint, so access it through a Systems Manager tunnel; anyone who can reach the tunnel can use the API.


## What gets deployed

- **SEPEX stack** ([stacks/sepex.py](stacks/sepex.py)):
  - By default, a VPC across 2 AZs with public, private-with-egress and isolated subnets, one shared NAT gateway, and an S3 gateway endpoint. An existing VPC can be selected instead.
  - EC2 `t3.small` running Ubuntu 24.04 and the API in Docker, on a 50 GiB encrypted disk. It has no public IP and requires IMDSv2.
  - RDS PostgreSQL 16 (`t4g.micro`, encrypted, 7-day backups, deletion protection). It is **retained** when the stack is deleted.
  - Versioned, private, encrypted S3 bucket. It is **retained** when the stack is deleted.
  - Lambda and EventBridge rule that report Batch job status changes back to the API.
  - SSM parameters under `/sepex/<stack>/`: `api-image`, `vpc-id`, `private-subnet-ids`.
- **Job stack** ([stacks/job.py](stacks/job.py)):
  - Fargate compute environment, job queue (`host.jobQueue`), and job definition (`host.jobDefinition`).
  - Reads the VPC from the SEPEX stack's SSM parameters, so it needs no account-specific config.
- **Cost notes:** EC2, RDS, and the NAT gateway continue to accrue hourly charges while deployed. Fargate, S3, data transfer, and logs depend on usage.

## Prerequisites

- Tools on your machine:
  - bash
  - [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
  - [Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html)
  - [uv](https://docs.astral.sh/uv/getting-started/installation/)
  - [Node.js](https://nodejs.org/) 22+ with npm
- You do **not** install the CDK CLI globally. It is pinned in [package.json](package.json), and the scripts install it locally with `npm ci`.
- AWS credentials:
  - The scripts use the default credential chain (`AWS_PROFILE`, environment variables, SSO, and so on).
  - For SSO, run `aws sso login --profile <name>` and `export AWS_PROFILE=<name>`.
- Region: set `AWS_REGION`, or configure a region on your profile.
- Permissions:
  - The first deployment bootstraps CDK in the account and region. Bootstrapping needs admin-level rights. `deploy.sh` offers to do it for you.
  - After that, deploying needs permission to assume the CDK bootstrap roles, plus the CloudFormation, SSM, and STS calls the scripts make. See [Action role requirements](#action-role-requirements).
- A SEPEX API image reference pinned by digest, `repository@sha256:<digest>`:
  - Find it with `docker buildx imagetools inspect ghcr.io/dewberry/sepex:<tag>`.

## Deploy SEPEX

```sh
API_IMAGE=ghcr.io/dewberry/sepex@sha256:<digest> bash deployment/scripts/deploy.sh
```

- What it does:
  1. Shows the target account and region.
  2. Bootstraps CDK if needed.
  3. Shows `cdk diff`.
  4. Asks for confirmation.
  5. Deploys.
- `API_IMAGE` is only needed the first time. Afterwards, `deploy.sh` updates the infrastructure only. Use `update-api.sh` to change the image.
- To deploy a second environment, set `SEPEX_STACK=<name>` (default `SepexStack`). Use the same `SEPEX_STACK` with every script.
- To reuse an existing VPC, set `SEPEX_VPC_ID` on every infrastructure deployment:

  ```sh
  SEPEX_VPC_ID=vpc-0123456789abcdef0 API_IMAGE=ghcr.io/dewberry/sepex@sha256:<digest> bash deployment/scripts/deploy.sh
  ```

  - CDK looks up the VPC and uses its private-with-egress subnets; this stack creates no VPC, NAT gateway, or S3 endpoint in this mode.
  - The VPC must have private-with-egress subnets in at least two AZs and routes/endpoints that let the API and Batch jobs reach their image registries and required AWS services. Existing routes and endpoints are not changed.
  - The lookup runs during CDK synthesis and needs credentials with EC2 describe permissions. Keep `SEPEX_VPC_ID` set for later infrastructure deployments, or CDK will synthesize the managed-VPC configuration instead.
- **Warning:** infrastructure changes that alter the server's boot script replace the EC2 instance. Its local disk, including registered plugins in `/opt/sepex/data`, is lost. The `cdk diff` output shows this as a replacement of `ApiInstance`.

## Connect to the API

```sh
bash deployment/scripts/tunnel.sh        # keep running
# Navigate to http://localhost:8080/
```

- `LOCAL_PORT=<port>` changes the local port.
- (Optional) SSH:

  ```sh
  bash deployment/scripts/enable-ssh.sh
  ```

  - Creates `~/.ssh/sepex-<stack>` if it is missing, authorizes it on the instance, and prints an `~/.ssh/config` block that tunnels SSH through SSM.
  - WSL users with a Windows SSH client (for example VS Code Remote-SSH) must copy the key and config block to the Windows side, e.g. `%USERPROFILE%\.ssh\`.

## Update or restart the API

```sh
API_IMAGE=ghcr.io/dewberry/sepex@sha256:<new-digest> bash deployment/scripts/update-api.sh
bash deployment/scripts/update-api.sh    # restart the current image
```

- With `API_IMAGE`:
  1. Records the new image in the stack (the `ApiImage` parameter and the SSM parameter).
  2. Runs `/opt/sepex/start-api.sh` on the instance, which pulls the image and recreates the container.
- Without it: restarts the current image. This also picks up a rotated database password.
- In both cases the instance is not replaced and registered processes are kept.

## Add a process (job stack)

1. Build and push the process image. Reference it as `repository:tag` or `repository@sha256:<digest>`; a digest is preferred.
2. Write the process YAML:
   - Start from [process_templates/aws_batch.yaml](../process_templates/aws_batch.yaml).
   - It must match [schemas/aws-batch-job.schema.json](../schemas/aws-batch-job.schema.json): `info.id`, `host.jobDefinition`, `host.jobQueue`, `host.image`, `config.maxResources`, and `command`.
   - Example: [plugin-examples/python/pyecho/pyecho-cloud.yml](../plugin-examples/python/pyecho/pyecho-cloud.yml).
3. Deploy it:

   ```sh
   bash deployment/scripts/deploy-job.sh path/to/process.yml
   ```

   - The stack name defaults to `<SEPEX_STACK>-<info.id>`. Override it with `JOB_STACK=<name>`.
   - CPU and memory are rounded up to the nearest valid Fargate size.
4. **Pin the job definition revision by hand.**
   - The revision number only exists after the deploy, so the YAML cannot contain it beforehand.
   - Edit `host.jobDefinition` to the `name:revision` that `deploy-job.sh` prints, e.g. `pyecho-cloud:3`.
   - Without a revision, every redeploy silently changes what the process runs.
5. Register the process and smoke-test it (with `tunnel.sh` running):

   ```sh
   uv run --project deployment deployment/scripts/register_process.py path/to/process.yml
   uv run --project deployment deployment/scripts/register_process.py test path/to/process.yml --inputs '{"text": "hello"}'
   ```

   - Use `--update` to replace a process that is already registered.
   - Use `--url` or `SEPEX_URL` to target an API other than `http://localhost:8080`.

## Deploy a process from GitHub Actions

- The root [action.yml](../action.yml) runs `deploy-job.sh --yes` for a process YAML in the calling repository.
- Outputs:
  - `job-definition-arn`
  - `job-definition` (`name:revision`, ready to paste into the YAML)

```yaml
jobs:
  deploy-batch-job:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write # required for OIDC
    steps:
      - uses: actions/checkout@v4
      - uses: Dewberry/sepex@<release-tag-or-commit-sha>
        id: deploy
        with:
          config-path: plugins/my-process.yml
          aws-region: us-east-1
          aws-role-arn: arn:aws:iam::<account-id>:role/<deploy-role>
          sepex-stack: SepexStack # optional, default SepexStack
          stack-name: "" # optional, default <sepex-stack>-<info.id>
      - run: echo "Pin ${{ steps.deploy.outputs.job-definition }} in the process YAML"
```

- Pin the action to a release tag or commit SHA, not `main`.
- The SEPEX stack must already exist, and CDK must already be bootstrapped in the account and region.

### Action role requirements

- **Trust policy:** GitHub OIDC (`token.actions.githubusercontent.com`), with `sub` scoped to your repository and branch or environment:

  ```json
  {
    "Effect": "Allow",
    "Principal": { "Federated": "arn:aws:iam::<account-id>:oidc-provider/token.actions.githubusercontent.com" },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": { "token.actions.githubusercontent.com:aud": "sts.amazonaws.com" },
      "StringLike": { "token.actions.githubusercontent.com:sub": "repo:<org>/<repo>:ref:refs/heads/main" }
    }
  }
  ```

- **Permissions policy:**

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": "sts:AssumeRole",
        "Resource": "arn:aws:iam::<account-id>:role/cdk-hnb659fds-*-<account-id>-<region>"
      },
      {
        "Effect": "Allow",
        "Action": ["cloudformation:DescribeStacks", "sts:GetCallerIdentity"],
        "Resource": "*"
      },
      {
        "Effect": "Allow",
        "Action": "ssm:GetParameter",
        "Resource": [
          "arn:aws:ssm:<region>:<account-id>:parameter/cdk-bootstrap/hnb659fds/version",
          "arn:aws:ssm:<region>:<account-id>:parameter/sepex/<sepex-stack>/*"
        ]
      }
    ]
  }
  ```

- CloudFormation does the actual deployment through the CDK bootstrap roles (deploy, file-publishing, lookup), so the action role itself needs no Batch or IAM permissions.

## Tear down

```sh
bash deployment/scripts/destroy.sh SepexStack-pyecho-cloud   # each job stack first
bash deployment/scripts/destroy.sh                           # then the SEPEX stack
```

- Delete job stacks first, because they use the SEPEX VPC.
- The RDS database and S3 bucket are retained. `destroy.sh` lists them at the end.
  - To delete the database, turn off its deletion protection, then delete it.
  - To delete the bucket, empty all object versions, then delete it.
- The CDK bootstrap stack (`CDKToolkit`) is shared by all CDK apps in the account, so it is left in place.

## Configuration reference

| Name | Used by | Default | Meaning |
| --- | --- | --- | --- |
| `SEPEX_STACK` | all scripts | `SepexStack` | SEPEX stack name |
| `SEPEX_VPC_ID` | `deploy.sh` | create a VPC | Existing VPC to reuse; keep set on subsequent infrastructure deploys |
| `API_IMAGE` | `deploy.sh` (first deploy), `update-api.sh` | none | API image as `repository@sha256:<digest>` |
| `AWS_PROFILE` | all scripts | default credential chain | AWS credentials |
| `AWS_REGION` | all scripts | profile region | Target region |
| `LOCAL_PORT` | `tunnel.sh` | `8080` | Local port for the API |
| `SSH_KEY_FILE` | `enable-ssh.sh` | `~/.ssh/sepex-<stack>` | SSH key to create or authorize |
| `JOB_STACK` | `deploy-job.sh` | `<SEPEX_STACK>-<info.id>` | Job stack name |
| `SEPEX_URL` | `register_process.py` | `http://localhost:8080` | API base URL |
| `--yes` | all scripts that make changes | off | Skip confirmations (for CI) |

- CDK context, set by the scripts:
  - `sepexStack`
  - `jobConfig` (the path to a process YAML; selects the job stack)
  - `jobStack`
- On the instance:
  - `/opt/sepex/sepex.conf` holds stack settings.
  - `/opt/sepex/start-api.sh` (re)starts the API.
  - `/opt/sepex/api.env` is generated and holds the secrets.

## Troubleshooting

- "Stack exists; use update-api.sh": `API_IMAGE` was set on `deploy.sh` for a stack that already exists.
- `deploy-job.sh` fails to read `/sepex/<stack>/…`: the SEPEX stack is not deployed, or you are using the wrong `SEPEX_STACK` or region.
- The tunnel will not start: install the Session Manager plugin, and check that the instance shows as online in Systems Manager Fleet Manager.
- API logs: run `sudo docker logs sepex` on the instance (over SSH), or use `aws ssm start-session --target <instance-id>`.
- Batch job logs: CloudWatch log group `/aws/batch/job`.
- TODO: add more cases as they come up.

## Cost Estimate

Illustrative on-demand estimate for **us-east-1**, running 730 hours/month. Prices are approximate, exclude tax, and should be checked in the [AWS Pricing Calculator](https://calculator.aws/) for your region and usage.

| Resource | Approximate monthly cost | Assumptions |
| --- | ---: | --- |
| NAT gateway and its public IPv4 address | $36.50 + $0.045/GB processed | One gateway; $0.045/hour plus $0.005/hour for its public IPv4 address |
| API EC2 instance | $15 | Linux `t3.small`, on-demand |
| API EC2 disk | $4 | 50 GiB gp3 EBS |
| PostgreSQL database and storage | $14.50 | Single-AZ `db.t4g.micro`, 20 GiB general-purpose storage |
| **Always-on baseline** | **about $70/month** | Before NAT data processing and other usage charges |

This estimate excludes Fargate runtime, internet data transfer, S3 storage and requests, CloudWatch logs, Secrets Manager, and backup storage beyond the included allowance. The `db.t4g.micro` has 1 GiB of memory and is intended for light workloads; monitor database memory and CPU, and scale up if needed. For scale, a Fargate job using 0.25 vCPU and 0.5 GiB costs about **$0.012 per running hour** in us-east-1, before data transfer. Prices vary by region and can change; the calculator is the source of truth for a deployment estimate.

Reusing an existing VPC avoids this stack's approximately **$36.50/month** NAT gateway and public IPv4 hourly charges if the shared VPC already provides egress. NAT data-processing charges still apply to traffic and may be billed to the VPC owner; include that shared cost when estimating the environment.


## Development

```sh
cd deployment
uv sync
uv run python -m unittest discover -s tests -t .
uv run pyright
uv run ruff check . && uv run ruff format --check .
./node_modules/.bin/cdk synth -c sepexStack=Dev                                  # after npm ci
./node_modules/.bin/cdk synth -c jobConfig=$PWD/../plugin-examples/python/pyecho/pyecho-cloud.yml
```

- Layout:
  - `app.py`: CDK entry point.
  - `stacks/`: stack definitions and the instance's `start-api.sh`.
  - `functions/`: Lambda code.
  - `scripts/`: user-facing scripts.
  - `tests/`: unit tests.
- TODO: accept a job role in the process config so jobs can call AWS APIs (see [stacks/job.py](stacks/job.py)).
