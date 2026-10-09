#!/usr/bin/env bash
# Deletes the SEPEX stack or a job stack. The database and artifacts bucket are retained.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<EOF
Usage: [SEPEX_STACK=name] bash $0 [stack-name] [--yes]

Deletes stack-name (default: \$SEPEX_STACK). Delete job stacks before the SEPEX
stack, because they run in its VPC.
EOF
}

parse_args "$@"
((${#POSITIONAL[@]} <= 1)) || {
	usage >&2
	exit 2
}
target=${POSITIONAL[0]:-$SEPEX_STACK}
require_commands aws
resolve_region
stack_exists "$target" || die "Stack $target not found in $AWS_REGION."

retained=$(aws cloudformation describe-stack-resources --stack-name "$target" \
	--query "StackResources[?ResourceType=='AWS::RDS::DBInstance' || ResourceType=='AWS::S3::Bucket'].[ResourceType, PhysicalResourceId]" \
	--output text)
show_target
printf 'Delete:      %s\n' "$target"
confirm "Permanently delete stack $target?"
aws cloudformation delete-stack --stack-name "$target"
printf 'Waiting for deletion...\n'
aws cloudformation wait stack-delete-complete --stack-name "$target"
printf 'Deleted %s.\n' "$target"
if [[ -n $retained ]]; then
	printf '\nThese resources were retained and are still billed; delete them deliberately if unneeded:\n%s\n' "$retained"
fi
