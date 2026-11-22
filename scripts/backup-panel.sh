#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
destination=${1:?Usage: sh scripts/backup-panel.sh /private/backup/directory}
umask 077
mkdir -p "$destination"
temporary=$(mktemp "$destination/.cove-backup.XXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM
docker compose exec -T database pg_dump -U cove -d cove --format=custom > "$temporary"
backup="$destination/cove-$(date -u +%Y%m%dT%H%M%SZ).dump"
ln "$temporary" "$backup"
printf 'Database backup saved to %s\n' "$backup"
echo 'Keep the original .env encryption key in a separate secure backup. This dump does not include worker volumes.'
