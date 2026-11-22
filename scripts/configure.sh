#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
[ ! -e .env ] || { echo '.env already exists; refusing to replace existing credentials.' >&2; exit 1; }
domain=${1:-}
case "$domain" in ''|*[!a-zA-Z0-9.-]*|.*|-*|*.) echo 'Usage: sh scripts/configure.sh cove.example.com' >&2; exit 1;; esac
command -v openssl >/dev/null || { echo 'OpenSSL is required to generate credentials.' >&2; exit 1; }
umask 077
temporary=$(mktemp .env.XXXXXX)
trap 'rm -f "$temporary"' EXIT HUP INT TERM
printf 'COVE_DOMAIN=%s\n' "$domain" > "$temporary"
printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 32)" >> "$temporary"
printf 'COVE_ENCRYPTION_KEY=%s\n' "$(openssl rand -base64 32)" >> "$temporary"
printf 'COVE_ADMIN_TOKEN=%s\n' "$(openssl rand -hex 32)" >> "$temporary"
ln "$temporary" .env
echo 'Created .env with private credentials. Back it up securely, then run docker compose up -d --build.'
echo "Open https://$domain/register to create the first administrator account."
