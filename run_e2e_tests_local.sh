#!/usr/bin/env bash
set -euo pipefail

COMPOSE="docker compose -f docker-compose.prod.yml"

cleanup() {
  echo "---- Tearing down ----"
  $COMPOSE down --volumes --remove-orphans 2>/dev/null || true
  if [ -d ".data" ]; then
# cleaning up .data/ with docker to avoid permission issues
    docker run --rm -v "$PWD/.data:/cleanup" alpine sh -c "rm -rf /cleanup/*"
  fi
}
if [ "${1:-}" != "--no-cleanup" ]; then
  trap cleanup EXIT
fi

# --- Check .env exists ---
if [ ! -f ".env" ]; then
  echo "ERROR: .env file not found. Copy example.env to .env and configure it."
  exit 1
fi

# --- Check if .data exists and is non-empty ---
if [ -d ".data" ] && [ -n "$(ls -A .data 2>/dev/null)" ]; then
  echo "WARNING: .data/ directory is not empty. E2E tests will overwrite its contents."
  read -rp "Delete .data/ and continue? [y/N] " answer
  if [[ "$answer" =~ ^[Yy]$ ]]; then
    docker run --rm -v "$PWD/.data:/cleanup" alpine sh -c "rm -rf /cleanup/*"
  else
    echo "Aborted."
    exit 1
  fi
fi

# --- Build plugin example images (same as GH Actions) ---
( cd plugin-examples && chmod +x build.sh && ./build.sh ) &

# --- Build compose stack ---
$COMPOSE build

# --- Network (ignore error if already exists) ---
docker network create sepex_net >/dev/null 2>&1 || true

# --- Run stack ---
$COMPOSE up -d

# --- Create bucket in minio ---
docker run --rm \
  --network sepex_net \
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
    $COMPOSE logs
    exit 1
  fi
  echo "Waiting for API server... attempt $attempts"
  sleep 10
done
echo "API server is ready!"

# --- Run newman tests ---
docker run --rm --network="host" \
  -v "$PWD/tests/e2e:/etc/newman" \
  postman/newman:5.3.1-alpine \
  run tests.postman_collection.json \
  --env-var "url=localhost:80" \
  --reporters cli \
  --bail \
  --color on

# --- Logs ---
echo "---- docker compose logs ----"
$COMPOSE logs || true
if [ -f .data/api/logs/api.jsonl ]; then
  echo "---- .data/api/logs/api.jsonl ----"
  cat .data/api/logs/api.jsonl || true
fi
