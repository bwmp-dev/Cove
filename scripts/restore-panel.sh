#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
backup=${1:-}
[ -f "$backup" ] && [ "${2:-}" = --replace-database ] || { echo 'Usage: sh scripts/restore-panel.sh backup.dump --replace-database' >&2; exit 1; }
echo 'Stopping the control plane and replacing its database. Worker applications are unaffected.'
docker compose stop cove
trap 'docker compose start cove' EXIT
docker compose exec -T database pg_restore -U cove -d cove --clean --if-exists --single-transaction --exit-on-error < "$backup"
echo 'Database restored. Cove will verify that .env contains the original encryption key on startup.'
