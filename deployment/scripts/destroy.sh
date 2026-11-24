#!/usr/bin/env bash
# Deletes the SEPEX stack or a job stack.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<EOF
Usage: [SEPEX_STACK=name] bash $0 [stack-name] [--yes]

Deletes stack-name (default: \$SEPEX_STACK). Delete job stacks first.
Deleting the SEPEX stack permanently deletes its database, files, and API disk.
Export data before proceeding; see deployment/README.md#tear-down.
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

show_target
printf 'Delete:      %s\n' "$target"
if [[ $target == "$SEPEX_STACK" ]]; then
	printf '\nWARNING: This permanently deletes the SEPEX database (no final snapshot),\n'
	printf 'all versions of files in its S3 bucket, and the API instance disk/plugins.\n'
	printf 'Export data first using deployment/README.md#tear-down. Delete job stacks first.\n'
	printf 'An imported VPC and its existing NAT stay; a SEPEX-created VPC and NAT are deleted.\n\n'
	confirm "Data exported and ready to permanently delete $target?"
	provider=$(aws cloudformation describe-stack-resources --stack-name "$target" \
		--query "StackResources[?ResourceType=='AWS::Lambda::Function' && starts_with(LogicalResourceId, 'CustomS3AutoDeleteObjects')].PhysicalResourceId | [0]" \
		--output text)
else
	confirm "Permanently delete job stack $target?"
fi
aws cloudformation delete-stack --stack-name "$target"
printf 'Waiting for deletion...\n'
aws cloudformation wait stack-delete-complete --stack-name "$target"
if [[ -n ${provider:-} && $provider != None ]]; then
	log_group="/aws/lambda/$provider"
	existing=$(aws logs describe-log-groups --log-group-name-prefix "$log_group" \
		--query "logGroups[?logGroupName=='$log_group'].logGroupName | [0]" --output text)
	if [[ $existing == "$log_group" ]]; then
		aws logs delete-log-group --log-group-name "$log_group"
	fi
fi
printf 'Deleted %s.\n' "$target"
