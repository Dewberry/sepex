#!/usr/bin/env bash
# Deploys or updates the SEPEX stack's infrastructure.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<EOF
Usage: [SEPEX_STACK=name] [API_IMAGE=repository@sha256:digest] bash $0 [--yes]

API_IMAGE is required for the first deployment (or for a stack created before the
ApiImage parameter existed) and rejected otherwise; change the image of a running
stack with update-api.sh.
--yes skips all confirmation prompts (for CI).
EOF
}

parse_args "$@"
((${#POSITIONAL[@]} == 0)) || {
	usage >&2
	exit 2
}
require_commands aws
resolve_region

stack_has_api_image_parameter() {
	[[ $(aws cloudformation describe-stacks --stack-name "$SEPEX_STACK" \
		--query "Stacks[0].Parameters[?ParameterKey=='ApiImage'] | length(@)" --output text) == 1 ]]
}

deploy_args=()
if stack_exists "$SEPEX_STACK" && stack_has_api_image_parameter; then
	[[ -z ${API_IMAGE:-} ]] || die "Stack $SEPEX_STACK exists; use scripts/update-api.sh to change its image."
else
	[[ ${API_IMAGE:-} =~ $DIGEST_IMAGE_PATTERN ]] ||
		die "Set API_IMAGE to a repository@sha256:<64 hex digits> reference; stack $SEPEX_STACK has no ApiImage yet."
	deploy_args+=(--parameters "ApiImage=$API_IMAGE")
fi

show_target
confirm 'Deploy to this account and region?'
install_toolchain
bootstrap_if_needed

cdk diff -c "sepexStack=$SEPEX_STACK"
confirm 'Deploy the changes shown above?'
cdk deploy -c "sepexStack=$SEPEX_STACK" --require-approval never \
	${deploy_args[@]+"${deploy_args[@]}"}
