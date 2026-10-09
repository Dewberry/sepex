#!/usr/bin/env bash
# Forwards a local port to the private SEPEX API through Systems Manager.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<EOF
Usage: [SEPEX_STACK=name] [LOCAL_PORT=8080] bash $0

Keep it running and use http://localhost:\$LOCAL_PORT; press Ctrl+C to stop.
EOF
}

parse_args "$@"
((${#POSITIONAL[@]} == 0)) || {
	usage >&2
	exit 2
}
LOCAL_PORT=${LOCAL_PORT:-8080}
[[ $LOCAL_PORT =~ ^[0-9]+$ ]] || die 'LOCAL_PORT must be a number.'
require_commands aws session-manager-plugin
resolve_region

instance_id=$(sepex_instance_id)
printf 'SEPEX API: http://localhost:%s (Ctrl+C to stop)\n' "$LOCAL_PORT"
exec aws ssm start-session --target "$instance_id" \
	--document-name AWS-StartPortForwardingSession \
	--parameters "{\"portNumber\":[\"80\"],\"localPortNumber\":[\"$LOCAL_PORT\"]}"
