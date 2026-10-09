# Shared helpers for the deployment scripts. Source this file; do not run it.
# Uses the AWS default credential chain (AWS_PROFILE, environment variables, SSO, instance roles).
set -euo pipefail

DEPLOYMENT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
REPO_ROOT=$(cd "$DEPLOYMENT_DIR/.." && pwd)
SEPEX_STACK=${SEPEX_STACK:-SepexStack}
# Matches DIGEST_IMAGE_PATTERN in stacks/shared.py; registry ports (host:5000/repo) are allowed.
DIGEST_IMAGE_PATTERN='^[^[:space:]@]+@sha256:[a-f0-9]{64}$'
ASSUME_YES=false
POSITIONAL=()

die() {
	printf '%s\n' "$*" >&2
	exit 1
}

[[ $SEPEX_STACK =~ ^[A-Za-z][A-Za-z0-9-]*$ ]] || die "SEPEX_STACK must be a valid CloudFormation stack name: $SEPEX_STACK"

# Parses --yes/--help and collects other arguments into POSITIONAL. Scripts define usage() first.
parse_args() {
	while (($#)); do
		case $1 in
			-y | --yes) ASSUME_YES=true ;;
			-h | --help)
				usage
				exit 0
				;;
			-*)
				usage >&2
				exit 2
				;;
			*) POSITIONAL+=("$1") ;;
		esac
		shift
	done
}

require_commands() {
	local command
	for command in "$@"; do
		command -v "$command" >/dev/null 2>&1 || die "Required command not found: $command"
	done
}

# Uses AWS_REGION, then AWS_DEFAULT_REGION, then the active profile's region.
resolve_region() {
	AWS_REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-$(aws configure get region 2>/dev/null || true)}}
	[[ -n $AWS_REGION ]] || die 'Set AWS_REGION or configure a region for your AWS profile.'
	export AWS_REGION AWS_DEFAULT_REGION=$AWS_REGION
}

# Prints the target account and region so the user can stop before changing the wrong one.
show_target() {
	ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
	printf 'AWS profile: %s\nAccount:     %s\nRegion:      %s\nSEPEX stack: %s\n' \
		"${AWS_PROFILE:-(default credential chain)}" "$ACCOUNT_ID" "$AWS_REGION" "$SEPEX_STACK"
}

confirm() {
	[[ $ASSUME_YES == true ]] && return 0
	local answer
	read -r -p "$1 [y/N] " answer
	[[ $answer == [yY] ]] || die 'Cancelled.'
}

stack_exists() {
	aws cloudformation describe-stacks --stack-name "$1" >/dev/null 2>&1
}

stack_output() {
	local value
	value=$(aws cloudformation describe-stacks --stack-name "$1" \
		--query "Stacks[0].Outputs[?OutputKey=='$2'].OutputValue | [0]" --output text)
	[[ -n $value && $value != None ]] || die "Output $2 not found on stack $1. Is it deployed in $AWS_REGION?"
	printf '%s\n' "$value"
}

sepex_instance_id() {
	stack_output "$SEPEX_STACK" SepexInstanceId
}

# Matches parameter_name() in stacks/shared.py.
sepex_parameter() {
	printf '/sepex/%s/%s\n' "$SEPEX_STACK" "$1"
}

# Installs the locked Python dependencies and the pinned CDK CLI from package-lock.json.
install_toolchain() {
	require_commands uv npm
	uv sync --project "$DEPLOYMENT_DIR" --locked --quiet
	npm ci --prefix "$DEPLOYMENT_DIR" --no-audit --no-fund --loglevel=error
}

cdk() {
	(cd "$DEPLOYMENT_DIR" && ./node_modules/.bin/cdk "$@")
}

bootstrap_if_needed() {
	if ! aws ssm get-parameter --name /cdk-bootstrap/hnb659fds/version >/dev/null 2>&1; then
		confirm "CDK is not bootstrapped in $ACCOUNT_ID/$AWS_REGION. Bootstrap it now?"
		cdk bootstrap "aws://$ACCOUNT_ID/$AWS_REGION"
	fi
}

json_string() {
	local value=${1//\\/\\\\}
	printf '"%s"' "${value//\"/\\\"}"
}

# Runs shell commands on an instance through Systems Manager, prints the output, and fails if they fail.
run_on_instance() {
	local instance_id=$1 commands='' command command_id status
	shift
	for command in "$@"; do
		commands+="${commands:+,}$(json_string "$command")"
	done
	command_id=$(aws ssm send-command --instance-ids "$instance_id" \
		--document-name AWS-RunShellScript --parameters "{\"commands\":[$commands]}" \
		--query Command.CommandId --output text)
	while :; do
		status=$(aws ssm get-command-invocation --command-id "$command_id" --instance-id "$instance_id" \
			--query Status --output text 2>/dev/null || echo Pending)
		case $status in
			Pending | InProgress | Delayed) sleep 5 ;;
			*) break ;;
		esac
	done
	aws ssm get-command-invocation --command-id "$command_id" --instance-id "$instance_id" \
		--query '[StandardOutputContent, StandardErrorContent]' --output text
	[[ $status == Success ]] || die "Command finished with status $status on $instance_id."
}
