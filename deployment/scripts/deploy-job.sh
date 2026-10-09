#!/usr/bin/env bash
# Deploys an AWS Batch job stack (compute environment, queue, job definition) from a process YAML.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<EOF
Usage: [SEPEX_STACK=name] [JOB_STACK=name] bash $0 path/to/process.yml [--yes]

The YAML must match schemas/aws-batch-job.schema.json. JOB_STACK defaults to
<SEPEX_STACK>-<info.id>. Prints the job definition revision to pin in the YAML
before registering the process with SEPEX.
EOF
}

parse_args "$@"
((${#POSITIONAL[@]} == 1)) || {
	usage >&2
	exit 2
}
config=${POSITIONAL[0]}
[[ -f $config ]] || die "Process YAML not found: $config"
config="$(cd "$(dirname "$config")" && pwd)/$(basename "$config")"
require_commands aws
resolve_region
stack_exists "$SEPEX_STACK" || die "Deploy the SEPEX stack $SEPEX_STACK in $AWS_REGION first."

context=(-c "sepexStack=$SEPEX_STACK" -c "jobConfig=$config")
if [[ -n ${JOB_STACK:-} ]]; then
	context+=(-c "jobStack=$JOB_STACK")
fi

show_target
printf 'Process:     %s\n' "$config"
confirm 'Deploy this process to this account and region?'
install_toolchain
bootstrap_if_needed

cdk diff "${context[@]}"
confirm 'Deploy the changes shown above?'
outputs=$(mktemp)
trap 'rm -f "$outputs"' EXIT
cdk deploy "${context[@]}" --require-approval never --outputs-file "$outputs"

arn=$(uv run --project "$DEPLOYMENT_DIR" --locked --quiet python -c \
	'import json, sys; print(next(iter(json.load(open(sys.argv[1])).values()))["JobDefinitionArn"])' \
	"$outputs")
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
	printf 'job-definition-arn=%s\njob-definition=%s\n' "$arn" "${arn##*/}" >>"$GITHUB_OUTPUT"
fi
cat <<EOF

Job definition: $arn

Pin this revision in $config before registering it:

  host:
    jobDefinition: ${arn##*/}

Then register it through the tunnel (scripts/tunnel.sh):

  uv run --project deployment deployment/scripts/register_process.py $config
EOF
