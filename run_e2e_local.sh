#!/usr/bin/env bash
set -euo pipefail

# --- Create env file (same as GH Actions) ---
cat > .env <<'EOF'
API_NAME='github-testing-sepex'
STORAGE_SERVICE='minio'
STORAGE_BUCKET='sepex-storage'
STORAGE_METADATA_PREFIX='metadata'
STORAGE_RESULTS_PREFIX='results'
STORAGE_LOGS_PREFIX='logs'

PLUGINS_LOAD_DIR='plugins'
PLUGINS_DIR='/.data/plugins'
TMP_JOB_LOGS_DIR='/.data/tmp/logs'

MAX_LOCAL_CPUS=4
MAX_LOCAL_MEMORY=4096

MINIO_ACCESS_KEY_ID=user
MINIO_SECRET_ACCESS_KEY=password
MINIO_S3_ENDPOINT=http://minio:9000
MINIO_S3_REGION='us-east-1'
MINIO_ROOT_USER=user
MINIO_ROOT_PASSWORD=password

DB_SERVICE='postgres'
POSTGRES_CONN_STRING='postgres://user:password@postgres:5432/db?sslmode=disable'
POSTGRES_PASSWORD='password'
POSTGRES_USER='user'
POSTGRES_DB='db'
PG_LOG_CHECKPOINTS='off'

# Optional (only needed if your tests hit aws-batch paths)
# AWS_ACCESS_KEY_ID=...
# AWS_SECRET_ACCESS_KEY=...
# AWS_REGION=...

BATCH_LOG_STREAM_GROUP='/aws/batch/job'

PYWRITE_MINIO_ACCESS_KEY_ID='user'
PYWRITE_MINIO_SECRET_ACCESS_KEY='password'
PYWRITE_MINIO_S3_ENDPOINT='http://minio:9000'
PYWRITE_MINIO_S3_REGION='us-east-1'
PYWRITE_MINIO_S3_BUCKET='sepex-storage'
EOF

# --- Build plugin example images (same as GH Actions) ---
( cd plugin-examples && chmod +x build.sh && ./build.sh ) &

# --- Build compose stack ---
docker compose -f docker-compose.prod.yml build

# --- Network (ignore error if already exists) ---
docker network create process_api_net >/dev/null 2>&1 || true

# --- Run stack ---
docker compose -f docker-compose.prod.yml up -d

# --- Create bucket in minio ---
docker run \
  --network process_api_net \
  -e MINIO_ROOT_USER=user \
  -e MINIO_ROOT_PASSWORD=password \
  -e STORAGE_BUCKET=sepex-storage \
  --entrypoint /bin/sh \
  minio/mc:RELEASE.2023-08-18T21-57-55Z \
  -c "mc alias set myminio http://minio:9000 \$MINIO_ROOT_USER \$MINIO_ROOT_PASSWORD && mc mb -p myminio/\${STORAGE_BUCKET} || true"

# --- Wait for API ---
attempts=0
max_attempts=12
until curl -fsS http://localhost:80 >/dev/null 2>&1; do
  attempts=$((attempts+1))
  if [ "$attempts" -ge "$max_attempts" ]; then
    echo "API not ready after $max_attempts attempts"
    docker compose logs
    exit 1
  fi
  echo "Waiting for API server... attempt $attempts"
  sleep 10
done
echo "API server is ready!"

# --- Run newman tests (use host networking like GH Actions) ---
docker run --network="host" \
  -v "$PWD/tests/e2e:/etc/newman" \
  postman/newman:5.3.1-alpine \
  run tests.postman_collection.json \
  --env-var "url=localhost:80" \
  --reporters cli \
  --bail \
  --color on

# --- Logs (helpful locally too) ---
echo "---- docker compose logs ----"
docker compose logs || true
if [ -f .data/api/logs/api.jsonl ]; then
  echo "---- .data/api/logs/api.jsonl ----"
  cat .data/api/logs/api.jsonl || true
fi
