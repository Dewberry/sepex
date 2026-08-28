# SEPEX development tasks. Run `just` to list everything available.

# List available recipes
default:
    @just --list

# Create the external docker network shared by the stack and spawned job containers
network:
    @docker network inspect sepex_net >/dev/null 2>&1 || docker network create sepex_net

# Build the example plugin images
build-plugins:
    cd plugin-examples && chmod +x build.sh && ./build.sh

# Build the stack images
build:
    docker compose build

# Start the dev stack (API on :5050, MinIO console on :9001)
up: network
    docker compose up -d

# Stop the stack
down:
    docker compose down

# Follow the stack logs
logs:
    docker compose logs -f

# Stop the stack and delete all local data (postgres, minio objects, api state)
wipe: down
    -docker run --rm -v {{ justfile_directory() }}/.data/:/data alpine rm -rf /data/postgres
    -docker run --rm -v {{ justfile_directory() }}/.data/:/data alpine rm -rf /data/minio
    -docker run --rm -v {{ justfile_directory() }}/.data/:/data alpine rm -rf /data/api

# Run the e2e suite against the local compose stack; leaves the stack up
test-e2e: wipe build-plugins build up
    @just _wait-for-api
    docker run --rm --network host -v "{{ justfile_directory() }}/tests/e2e:/etc/newman" postman/newman:5.3.1-alpine run tests.postman_collection.json --env-var "url=localhost:5050" --reporters cli --bail --color on

# Block until the stack answers on :5050, dumping logs if it never does
_wait-for-api:
    #!/usr/bin/env bash
    set -euo pipefail
    for attempt in $(seq 1 12); do
      if curl -fsS http://localhost:5050 >/dev/null 2>&1; then
        echo "API server is ready!"
        exit 0
      fi
      echo "Waiting for API server... attempt $attempt"
      sleep 10
    done
    echo "API not ready after 12 attempts"
    just _dump-logs
    exit 1

# Print the compose logs and the API log file
_dump-logs:
    #!/usr/bin/env bash
    echo "---- docker compose logs ----"
    docker compose logs || true
    if [ -f .data/api/logs/api.jsonl ]; then
      echo "---- .data/api/logs/api.jsonl ----"
      cat .data/api/logs/api.jsonl || true
    fi
