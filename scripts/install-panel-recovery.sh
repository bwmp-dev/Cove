#!/bin/sh
set -eu
[ "$(id -u)" = 0 ] || { echo 'Run as root on the Linux panel host.' >&2; exit 1; }
command -v systemctl >/dev/null
command -v python3 >/dev/null
command -v docker >/dev/null
installation=$(cd "${1:-$(dirname "$0")/..}" && pwd)
case "$installation" in *[!a-zA-Z0-9_./-]*) echo 'Installation path must not contain spaces or shell metacharacters.' >&2; exit 1;; esac
[ -f "$installation/.env" ] && [ -f "$installation/compose.yaml" ] || { echo 'A configured Cove installation is required.' >&2; exit 1; }
install -d -m 700 /usr/local/lib/cove
install -m 700 "$(dirname "$0")/backup-panel.py" /usr/local/lib/cove/backup-panel.py
install -m 700 "$(dirname "$0")/panel-recovery.py" /usr/local/lib/cove/panel-recovery.py
python=$(command -v python3)
"$python" /usr/local/lib/cove/panel-recovery.py check --installation "$installation"
cat > /etc/systemd/system/cove-panel-recovery.service <<SERVICE
[Unit]
Description=Run Cove panel backups and journaled recovery on the host
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target
[Service]
Type=oneshot
WorkingDirectory=$installation
ExecStart=$python /usr/local/lib/cove/panel-recovery.py tick --installation $installation
TimeoutStartSec=45min
UMask=0077
SERVICE
cat > /etc/systemd/system/cove-panel-recovery.timer <<'TIMER'
[Unit]
Description=Check the Cove panel backup and recovery queue
[Timer]
OnBootSec=2min
OnUnitActiveSec=1min
[Install]
WantedBy=timers.target
TIMER
systemctl daemon-reload
systemctl enable --now cove-panel-recovery.timer
echo 'Panel host recovery timer installed. Configure backups in Settings and preserve the private repository password independently.'
