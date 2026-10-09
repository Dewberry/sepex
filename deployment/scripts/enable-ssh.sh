#!/usr/bin/env bash
# Adds a local SSH key to the SEPEX instance through Systems Manager; port 22 stays closed.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<EOF
Usage: [SEPEX_STACK=name] [SSH_KEY_FILE=path] bash $0

Creates SSH_KEY_FILE (default ~/.ssh/sepex-\$SEPEX_STACK) if needed, authorizes it
for the instance's ubuntu user, and prints an ~/.ssh/config block that tunnels SSH
through Systems Manager.
EOF
}

parse_args "$@"
((${#POSITIONAL[@]} == 0)) || {
	usage >&2
	exit 2
}
require_commands aws ssh-keygen base64 tr session-manager-plugin
resolve_region

SSH_KEY_FILE=${SSH_KEY_FILE:-"$HOME/.ssh/sepex-$SEPEX_STACK"}
public_key_file="$SSH_KEY_FILE.pub"

umask 077
mkdir -p "$(dirname "$SSH_KEY_FILE")"
if [[ ! -f $SSH_KEY_FILE ]]; then
	ssh-keygen -t ed25519 -f "$SSH_KEY_FILE" -C "sepex-$SEPEX_STACK"
elif [[ ! -f $public_key_file ]]; then
	die "Public key not found: $public_key_file. Set SSH_KEY_FILE to a key pair that includes its .pub file."
fi

instance_id=$(sepex_instance_id)
public_key_base64=$(base64 <"$public_key_file" | tr -d '\n')
remote_key_file="/tmp/sepex-ssh-key-$$"
run_on_instance "$instance_id" \
	"set -eu; umask 077" \
	"install -d -m 700 -o ubuntu -g ubuntu /home/ubuntu/.ssh" \
	"touch /home/ubuntu/.ssh/authorized_keys" \
	"printf %s $public_key_base64 | base64 -d > $remote_key_file" \
	"grep -qxFf $remote_key_file /home/ubuntu/.ssh/authorized_keys || cat $remote_key_file >> /home/ubuntu/.ssh/authorized_keys" \
	"chown ubuntu:ubuntu /home/ubuntu/.ssh/authorized_keys && chmod 600 /home/ubuntu/.ssh/authorized_keys" \
	"rm -f $remote_key_file"
printf 'SSH public key installed on %s. Private key: %s\n' "$instance_id" "$SSH_KEY_FILE"

identity_file=$SSH_KEY_FILE
if command -v cygpath >/dev/null 2>&1; then
	identity_file=$(cygpath -w "$SSH_KEY_FILE")
fi
proxy_command="aws ssm start-session --target %h --region $AWS_REGION${AWS_PROFILE:+ --profile $AWS_PROFILE} --document-name AWS-StartSSHSession --parameters portNumber=%p"
printf '\nAdd this block to ~/.ssh/config, then run: ssh %s\n\n' "$SEPEX_STACK"
printf 'Host %s\n    HostName %s\n    User ubuntu\n    ProxyCommand %s\n    IdentityFile "%s"\n' \
	"$SEPEX_STACK" "$instance_id" "$proxy_command" "$identity_file"
