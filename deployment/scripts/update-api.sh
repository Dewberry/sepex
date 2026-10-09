#!/usr/bin/env bash
# Changes the SEPEX API image, or re-pulls and restarts the current one, without replacing the instance.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<EOF
Usage: [SEPEX_STACK=name] [API_IMAGE=repository@sha256:digest] bash $0 [--yes]

With API_IMAGE, records it as the stack's ApiImage parameter, then pulls it and
restarts the container. Without API_IMAGE, restarts the current image (this also
reloads the database password after a rotation). Registered processes are kept.
EOF
}

parse_args "$@"
((${#POSITIONAL[@]} == 0)) || {
	usage >&2
	exit 2
}
require_commands aws
resolve_region

instance_id=$(sepex_instance_id)
current_image=$(aws ssm get-parameter --name "$(sepex_parameter api-image)" \
	--query Parameter.Value --output text)
show_target
printf 'Instance:    %s\nCurrent:     %s\n' "$instance_id" "$current_image"

if [[ -n ${API_IMAGE:-} && $API_IMAGE != "$current_image" ]]; then
	[[ $API_IMAGE =~ $DIGEST_IMAGE_PATTERN ]] ||
		die 'API_IMAGE must be a repository@sha256:<64 hex digits> reference.'
	printf 'New:         %s\n' "$API_IMAGE"
	confirm 'Update the image and restart the API?'

	# Updating the CloudFormation parameter, not just SSM, keeps later `cdk deploy` runs in sync.
	parameters=()
	for key in $(aws cloudformation describe-stacks --stack-name "$SEPEX_STACK" \
		--query 'Stacks[0].Parameters[].ParameterKey' --output text); do
		if [[ $key == ApiImage ]]; then
			parameters+=("ParameterKey=ApiImage,ParameterValue=$API_IMAGE")
		else
			parameters+=("ParameterKey=$key,UsePreviousValue=true")
		fi
	done
	aws cloudformation update-stack --stack-name "$SEPEX_STACK" --use-previous-template \
		--capabilities CAPABILITY_IAM --parameters "${parameters[@]}" >/dev/null
	printf 'Waiting for the stack update...\n'
	aws cloudformation wait stack-update-complete --stack-name "$SEPEX_STACK"
else
	confirm 'Re-pull and restart the current image?'
fi

run_on_instance "$instance_id" /opt/sepex/start-api.sh
