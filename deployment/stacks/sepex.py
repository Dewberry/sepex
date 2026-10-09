from aws_cdk import (
    CfnOutput,
    CfnParameter,
    Duration,
    Environment,
    Fn,
    RemovalPolicy,
    Stack,
    aws_ec2,
    aws_events,
    aws_events_targets,
    aws_iam,
    aws_lambda,
    aws_logs,
    aws_rds,
    aws_s3,
    aws_ssm,
)
from constructs import Construct

from stacks.shared import (
    API_NAME,
    AZ_COUNT,
    DEPLOYMENT_DIR,
    DIGEST_IMAGE_PATTERN,
    parameter_name,
)

BATCH_LOG_GROUP = "/aws/batch/job"


class SepexStack(Stack):
    """Deploy the private SEPEX API and the AWS services it uses.

    EC2 runs the API container. It stores records in PostgreSQL, stores files in
    S3, and submits jobs to AWS Batch. EventBridge sends Batch status changes to
    a Lambda function, which reports them to the API. Systems Manager provides
    remote administration without making the API or server public.
    """

    def __init__(
        self,
        scope: Construct,
        construct_id: str,
        *,
        existing_vpc_id: str | None = None,
        env: Environment | None = None,
    ) -> None:
        super().__init__(scope, construct_id, env=env)

        # A parameter rather than context so later deploys and destroys can omit it.
        api_image = CfnParameter(
            self,
            "ApiImage",
            description="SEPEX API image as repository@sha256:digest",
            allowed_pattern=DIGEST_IMAGE_PATTERN,
        )

        # Reuse a supplied VPC, or create the default network including its NAT gateway.
        if existing_vpc_id:
            vpc = aws_ec2.Vpc.from_lookup(self, "Vpc", vpc_id=existing_vpc_id)
        else:
            vpc = aws_ec2.Vpc(
                self,
                "Vpc",
                max_azs=AZ_COUNT,
                subnet_configuration=[
                    aws_ec2.SubnetConfiguration(
                        # NAT lives here and gives private servers outbound internet access.
                        name="public",
                        subnet_type=aws_ec2.SubnetType.PUBLIC,
                    ),
                    aws_ec2.SubnetConfiguration(
                        # The API, callback, and Batch jobs use this subnet; they have no public IP.
                        name="api",
                        subnet_type=aws_ec2.SubnetType.PRIVATE_WITH_EGRESS,
                    ),
                    aws_ec2.SubnetConfiguration(
                        # The database has no internet route.
                        name="database",
                        subnet_type=aws_ec2.SubnetType.PRIVATE_ISOLATED,
                    ),
                ],
            )
            # Send S3 file transfers directly to S3 instead of through the NAT gateway.
            vpc.add_gateway_endpoint(
                "S3Endpoint", service=aws_ec2.GatewayVpcEndpointAwsService.S3
            )
        private_subnets = aws_ec2.SubnetSelection(
            subnet_type=aws_ec2.SubnetType.PRIVATE_WITH_EGRESS,
            one_per_az=True,
        )
        selected_private_subnets = vpc.select_subnets(
            subnet_type=aws_ec2.SubnetType.PRIVATE_WITH_EGRESS,
            one_per_az=True,
        )
        if len(selected_private_subnets.subnet_ids) < AZ_COUNT:
            raise ValueError(
                f"VPC must have private-with-egress subnets in at least {AZ_COUNT} AZs"
            )
        database_subnets = aws_ec2.SubnetSelection(
            subnet_type=(
                aws_ec2.SubnetType.PRIVATE_WITH_EGRESS
                if existing_vpc_id
                else aws_ec2.SubnetType.PRIVATE_ISOLATED
            )
        )

        # Security groups are AWS firewalls. This one starts with no incoming rules.
        api_sg = aws_ec2.SecurityGroup(self, "ApiSecurityGroup", vpc=vpc)

        db_sg = aws_ec2.SecurityGroup(self, "DatabaseSecurityGroup", vpc=vpc)
        # Allow only the API server to reach PostgreSQL on port 5432.
        db_sg.add_ingress_rule(api_sg, aws_ec2.Port.tcp(5432), "SEPEX API only")

        # AWS runs PostgreSQL and creates its password in Secrets Manager for the API to fetch.
        db = aws_rds.DatabaseInstance(
            self,
            "Database",
            engine=aws_rds.DatabaseInstanceEngine.postgres(
                version=aws_rds.PostgresEngineVersion.VER_16
            ),
            vpc=vpc,
            vpc_subnets=database_subnets,
            security_groups=[db_sg],
            credentials=aws_rds.Credentials.from_generated_secret("sepex_app"),
            database_name="sepex",
            instance_type=aws_ec2.InstanceType.of(
                aws_ec2.InstanceClass.T4G, aws_ec2.InstanceSize.MICRO
            ),
            allocated_storage=20,
            # Protect database contents and keep the database if this stack is deleted.
            storage_encrypted=True,
            backup_retention=Duration.days(7),
            deletion_protection=True,
            removal_policy=RemovalPolicy.RETAIN,
            publicly_accessible=False,
        )
        if db.secret is None:
            raise ValueError("Database credentials must be stored in Secrets Manager")

        # Store job files privately and encrypted; keep old versions and the bucket on deletion.
        bucket = aws_s3.Bucket(
            self,
            "Artifacts",
            block_public_access=aws_s3.BlockPublicAccess.BLOCK_ALL,
            enforce_ssl=True,
            encryption=aws_s3.BucketEncryption.S3_MANAGED,
            versioned=True,
            removal_policy=RemovalPolicy.RETAIN,
        )

        # SSM parameters let scripts and job stacks find this stack's resources by name.
        image_parameter = aws_ssm.StringParameter(
            self,
            "ApiImageParameter",
            parameter_name=parameter_name(self.stack_name, "api-image"),
            string_value=api_image.value_as_string,
        )
        for construct_name, key, value in (
            ("VpcIdParameter", "vpc-id", vpc.vpc_id),
            (
                "PrivateSubnetIdsParameter",
                "private-subnet-ids",
                Fn.join(",", selected_private_subnets.subnet_ids[:AZ_COUNT]),
            ),
        ):
            aws_ssm.StringParameter(
                self,
                construct_name,
                parameter_name=parameter_name(self.stack_name, key),
                string_value=value,
            )

        # This role is the server's AWS identity, not a user login. AWS supplies
        # short-lived credentials for it; the permissions below let the API use
        # Systems Manager, S3, Secrets Manager (DB password), and AWS Batch.
        # jsii stubs rename some interface method parameters, which pyright reports as protocol mismatches.
        role = aws_iam.Role(
            self,
            "ApiRole",
            assumed_by=aws_iam.ServicePrincipal("ec2.amazonaws.com"),  # pyright: ignore[reportArgumentType]
        )
        role.add_managed_policy(
            aws_iam.ManagedPolicy.from_aws_managed_policy_name(
                "AmazonSSMManagedInstanceCore"
            )
        )
        bucket.grant_read_write(role)
        db.secret.grant_read(role)
        image_parameter.grant_read(role)
        aws_logs.LogGroup.from_log_group_name(
            self, "BatchJobLogs", BATCH_LOG_GROUP
        ).grant_read(role)
        batch_arn = f"arn:{self.partition}:batch:{self.region}:{self.account}"
        role.add_to_policy(
            aws_iam.PolicyStatement(
                actions=["batch:SubmitJob"],
                resources=[
                    f"{batch_arn}:job-queue/*",
                    f"{batch_arn}:job-definition/*",
                ],
            )
        )
        role.add_to_policy(
            aws_iam.PolicyStatement(
                actions=["batch:TerminateJob", "batch:CancelJob"],
                resources=[f"{batch_arn}:job/*"],
            )
        )
        role.add_to_policy(
            aws_iam.PolicyStatement(
                # These Describe actions do not support resource-level permissions.
                actions=["batch:DescribeJobs", "batch:DescribeJobDefinitions"],
                resources=["*"],
            )
        )

        # EC2 is the virtual machine running Docker and the API, with no public IP.
        instance = aws_ec2.Instance(
            self,
            "ApiInstance",
            vpc=vpc,
            vpc_subnets=private_subnets,
            security_group=api_sg,
            role=role,  # pyright: ignore[reportArgumentType]
            instance_type=aws_ec2.InstanceType.of(
                aws_ec2.InstanceClass.T3, aws_ec2.InstanceSize.SMALL
            ),
            # Changing the boot script replaces the instance and its local data.
            user_data_causes_replacement=True,
            # Canonical's current Ubuntu 24.04 image includes the Systems Manager agent.
            machine_image=aws_ec2.MachineImage.from_ssm_parameter(
                "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id",
                os=aws_ec2.OperatingSystemType.LINUX,
            ),
            # Require token-protected access when software on the server requests AWS credentials.
            http_tokens=aws_ec2.HttpTokens.REQUIRED,
            http_put_response_hop_limit=2,
            # This encrypted disk holds the OS and /opt/sepex/data; back it up before replacing EC2.
            block_devices=[
                aws_ec2.BlockDevice(
                    device_name="/dev/xvda",
                    volume=aws_ec2.BlockDeviceVolume.ebs(
                        50, encrypted=True, volume_type=aws_ec2.EbsDeviceVolumeType.GP3
                    ),
                )
            ],
        )
        # start-api.sh reads these on every (re)start; the image and password are fetched at runtime.
        api_config = {
            "SEPEX_REGION": self.region,
            "SEPEX_API_NAME": API_NAME,
            "SEPEX_IMAGE_PARAMETER": image_parameter.parameter_name,
            "SEPEX_DB_SECRET_ARN": db.secret.secret_arn,
            "SEPEX_DB_HOST": db.db_instance_endpoint_address,
            "SEPEX_DB_PORT": db.db_instance_endpoint_port,
            "SEPEX_BUCKET": bucket.bucket_name,
            "SEPEX_BATCH_LOG_GROUP": BATCH_LOG_GROUP,
        }
        start_script = (DEPLOYMENT_DIR / "stacks" / "start-api.sh").read_text()
        # EC2 runs this script only on first boot; it installs Docker and starts the API container.
        instance.user_data.add_commands(
            # Stop setup if any command fails, so a partial installation is not treated as ready.
            "set -euo pipefail",
            "apt-get update",
            "DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io python3 curl unzip",
            "curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip",
            "unzip -q /tmp/awscliv2.zip -d /tmp",
            "/tmp/aws/install",
            "rm -rf /tmp/aws /tmp/awscliv2.zip",
            "systemctl enable --now docker",
            "install -d -m 700 /opt/sepex /opt/sepex/data /opt/sepex/data/plugins",
            "cat > /opt/sepex/sepex.conf <<'SEPEX_CONF'\n"
            + "".join(f"{key}={value}\n" for key, value in api_config.items())
            + "SEPEX_CONF",
            f"cat > /opt/sepex/start-api.sh <<'SEPEX_SCRIPT'\n{start_script}SEPEX_SCRIPT",
            "chmod 700 /opt/sepex/start-api.sh",
            "/opt/sepex/start-api.sh",
        )

        # Allow the callback, and no other network client, to call the API on port 80.
        callback_sg = aws_ec2.SecurityGroup(self, "CallbackSecurityGroup", vpc=vpc)
        api_sg.add_ingress_rule(
            callback_sg, aws_ec2.Port.tcp(80), "Batch callback only"
        )
        # Lambda runs in the same private network and calls the API's private IP.
        callback = aws_lambda.Function(
            self,
            "BatchCallback",
            runtime=aws_lambda.Runtime.PYTHON_3_12,
            handler="lambda_function.lambda_handler",
            code=aws_lambda.Code.from_asset(
                str(DEPLOYMENT_DIR / "functions" / "batch_callback"),
                exclude=["__pycache__"],
            ),
            vpc=vpc,
            vpc_subnets=private_subnets,
            security_groups=[callback_sg],
            environment={
                "SEPEX_URL": f"http://{instance.instance_private_ip}",
                "API_NAME": API_NAME,
            },
            timeout=Duration.seconds(15),
            # Generated name and DESTROY, so a destroyed stack can be redeployed without a name clash.
            log_group=aws_logs.LogGroup(
                self,
                "CallbackLogs",
                retention=aws_logs.RetentionDays.ONE_MONTH,
                removal_policy=RemovalPolicy.DESTROY,
            ),
        )
        # Send start/success/failure events for SEPEX jobs to Lambda; it updates the API.
        aws_events.Rule(
            self,
            "BatchStatusRule",
            event_pattern=aws_events.EventPattern(
                source=["aws.batch"],
                detail_type=["Batch Job State Change"],
                detail={
                    "status": ["RUNNING", "SUCCEEDED", "FAILED"],
                    "jobName": [{"prefix": f"{API_NAME}_"}],
                },
            ),
            targets=[aws_events_targets.LambdaFunction(callback)],  # pyright: ignore[reportArgumentType]
        )

        # CloudFormation prints these after deployment; scripts read SepexInstanceId.
        CfnOutput(
            self, "SepexApiEndpoint", value=f"http://{instance.instance_private_ip}"
        )
        CfnOutput(self, "SepexInstanceId", value=instance.instance_id)
        CfnOutput(self, "SepexDatabaseEndpoint", value=db.db_instance_endpoint_address)
        CfnOutput(self, "SepexArtifactsBucket", value=bucket.bucket_name)
        CfnOutput(self, "VpcId", value=vpc.vpc_id)
