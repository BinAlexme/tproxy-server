#!/usr/bin/env bash
set -euo pipefail

destination=/etc/mtproxy/proxy-multi.conf
temporary="$(mktemp /etc/mtproxy/proxy-multi.conf.XXXXXX)"
trap 'rm -f "$temporary"' EXIT

curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
	--output "$temporary" https://core.telegram.org/getProxyConfig
test "$(wc -c < "$temporary")" -ge 100
grep -q '^default ' "$temporary"
grep -q '^proxy_for ' "$temporary"
chown root:mtproxy "$temporary"
chmod 0640 "$temporary"
mv -f "$temporary" "$destination"
trap - EXIT
