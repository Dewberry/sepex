#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p public/schemas
python3 -m venv /tmp/schema-venv
. /tmp/schema-venv/bin/activate
python -m pip install --upgrade pip
python -m pip install json-schema-for-humans
/tmp/schema-venv/bin/generate-schema-doc \
  --copy-css \
  --copy-js \
  public/schemas/process.json \
  public/schemas/process.html
