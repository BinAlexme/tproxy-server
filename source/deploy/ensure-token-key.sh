#!/usr/bin/env bash
set -euo pipefail
umask 077

key="${1:-/etc/tproxy-server/token.key}"
if [[ "${EUID}" -ne 0 ]]; then
	echo "run this key provisioner as root" >&2
	exit 1
fi
if [[ -L "$key" ]] || { [[ -e "$key" ]] && [[ ! -f "$key" ]]; }; then
	echo "token key must be a regular file: $key" >&2
	exit 1
fi
if [[ ! -e "$key" ]]; then
	temporary="$(mktemp "${key}.XXXXXX")"
	trap 'rm -f -- "$temporary"' EXIT
	head -c 32 /dev/urandom > "$temporary"
	chown tproxy:tproxy "$temporary"
	chmod 0400 "$temporary"
	if ! ln "$temporary" "$key"; then
		echo "token key appeared during provisioning; refusing to replace it" >&2
		exit 1
	fi
fi
if [[ "$(wc -c < "$key")" -ne 32 ]]; then
	echo "token key must contain exactly 32 bytes; refusing to replace it" >&2
	exit 1
fi

# Preserve the key bytes across reinstalls, updates, and binary rollbacks.
chown tproxy:tproxy "$key"
chmod 0400 "$key"