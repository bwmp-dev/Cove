#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
[ ! -e .env ] || { echo '.env already exists; refusing to replace existing credentials.' >&2; exit 1; }
domain=${1:-}
base_path=${2:-}
case "$domain" in ''|*[!a-zA-Z0-9.-]*|.*|-*|*.) echo 'Usage: sh scripts/configure.sh cove.example.com' >&2; exit 1;; esac
case "$base_path" in ''|/*) ;; *) echo 'Use an empty base path or /cove without a trailing slash.' >&2; exit 1;; esac
case "$base_path" in *[!a-zA-Z0-9_/-]*|*/|*//*) echo 'Base paths use letters, numbers, underscores and hyphens in nonempty segments.' >&2; exit 1;; esac
[ "${#base_path}" -le 200 ] || { echo 'Base path must be at most 200 characters.' >&2; exit 1; }
[ "$base_path" != /healthz ] || { echo '/healthz is reserved for the liveness check.' >&2; exit 1; }
bind_address=${3:-0.0.0.0}
case "$bind_address" in *[!0-9.]*|'') echo 'Bind address must be an IPv4 address (for example your Tailscale address).' >&2; exit 1;; esac
command -v python3 >/dev/null || { echo 'Python 3 is required to validate the bind address.' >&2; exit 1; }
python3 -c 'import ipaddress,sys; ipaddress.IPv4Address(sys.argv[1])' "$bind_address"
command -v openssl >/dev/null || { echo 'OpenSSL is required to generate credentials.' >&2; exit 1; }
umask 077
temporary=$(mktemp .env.XXXXXX)
trap 'rm -f "$temporary"' EXIT HUP INT TERM
printf 'COVE_DOMAIN=%s\n' "$domain" > "$temporary"
printf 'COVE_BASE_PATH=%s\n' "$base_path" >> "$temporary"
printf 'COVE_BIND_ADDRESS=%s\n' "$bind_address" >> "$temporary"
printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 32)" >> "$temporary"
printf 'COVE_ENCRYPTION_KEY=%s\n' "$(openssl rand -base64 32)" >> "$temporary"
printf 'COVE_ADMIN_TOKEN=%s\n' "$(openssl rand -hex 32)" >> "$temporary"
ln "$temporary" .env
echo 'Created .env with private credentials. Back it up securely, then run docker compose up -d --build.'
echo "Open https://$domain$base_path/register to create the first administrator account."
