#!/usr/bin/env bash
# Installed on the SEPEX instance as /opt/sepex/start-api.sh; (re)starts the API container. Safe to rerun.
set -euo pipefail
umask 077

set -a
# shellcheck source=/dev/null
source /opt/sepex/sepex.conf
set +a

image=$(aws ssm get-parameter --region "$SEPEX_REGION" --name "$SEPEX_IMAGE_PARAMETER" \
	--query Parameter.Value --output text)

# Rebuilt on every start so a rotated database password is picked up.
secret=$(aws secretsmanager get-secret-value --region "$SEPEX_REGION" \
	--secret-id "$SEPEX_DB_SECRET_ARN" --query SecretString --output text)
SEPEX_SECRET=$secret python3 - >/opt/sepex/api.env <<'PY'
import json
import os
from urllib.parse import quote

env = os.environ
secret = json.loads(env["SEPEX_SECRET"])
user = quote(secret["username"], safe="")
password = quote(secret["password"], safe="")
settings = {
    "API_NAME": env["SEPEX_API_NAME"],
    "API_PORT": "5050",
    "DB_SERVICE": "postgres",
    "POSTGRES_CONN_STRING": f"postgres://{user}:{password}@{env['SEPEX_DB_HOST']}:{env['SEPEX_DB_PORT']}/sepex?sslmode=require",
    "STORAGE_SERVICE": "aws-s3",
    "STORAGE_BUCKET": env["SEPEX_BUCKET"],
    "AWS_REGION": env["SEPEX_REGION"],
    "BATCH_LOG_STREAM_GROUP": env["SEPEX_BATCH_LOG_GROUP"],
    "TMP_JOB_LOGS_DIR": "/.data/tmp/job_logs",
    "PLUGINS_DIR": "/.data/plugins",
    "SEPEX_DOCKER_NETWORK": "host",
    "SKIP_GPU_VERIFICATION": "true",
    "MAX_LOCAL_GPUS": "0",
}
print("\n".join(f"{key}={value}" for key, value in settings.items()))
PY

docker pull "$image"
docker rm -f sepex >/dev/null 2>&1 || true
# Host port 80 maps to the API; the Docker socket lets the API launch job containers.
docker run -d --name sepex --restart unless-stopped -p 80:5050 \
	--env-file /opt/sepex/api.env \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v /opt/sepex/data:/.data \
	"$image"
docker image prune -f >/dev/null
echo "SEPEX API started with $image"
