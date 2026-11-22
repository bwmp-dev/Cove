#!/bin/sh
set -eu
[ "$(id -u)" = 0 ] || { echo 'Run as root on the Linux panel host.' >&2; exit 1; }
command -v systemctl >/dev/null
command -v python3 >/dev/null
installation=$(cd "${1:-$(dirname "$0")/..}" && pwd)
case "$installation" in *[!a-zA-Z0-9_./-]*) echo 'Installation path must not contain spaces or shell metacharacters.' >&2; exit 1;; esac
[ -f "$installation/.env" ] && [ -f "$installation/compose.yaml" ] || { echo 'A configured Cove installation is required.' >&2; exit 1; }
install -d -m 700 /usr/local/lib/cove
install -m 700 "$(dirname "$0")/update-panel.py" /usr/local/lib/cove/update-panel.py
sh "$(dirname "$0")/install-panel-recovery.sh" "$installation"
python=$(command -v python3)
cat > /etc/systemd/system/cove-panel-update.service <<SERVICE
[Unit]
Description=Update Cove panel and workers from the selected channel
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target
[Service]
Type=oneshot
WorkingDirectory=$installation
ExecStart=$python /usr/local/lib/cove/update-panel.py --latest --installation $installation
TimeoutStartSec=45min
UMask=0077
SERVICE
cat > /etc/systemd/system/cove-panel-update.timer <<'TIMER'
[Unit]
Description=Check hourly for Cove updates
[Timer]
OnBootSec=10min
OnUnitActiveSec=1h
RandomizedDelaySec=5min
[Install]
WantedBy=timers.target
TIMER
systemctl daemon-reload
systemctl enable --now cove-panel-update.timer
echo 'Automatic updates enabled. Select beta or release in dashboard Settings. Inspect journalctl -u cove-panel-update.'
