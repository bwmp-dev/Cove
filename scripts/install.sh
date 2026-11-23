#!/bin/sh
set -eu
umask 077

fail() { echo "$*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail 'Run as root on the Linux dashboard host.'
[ "$(uname -s)" = Linux ] || fail 'A Linux host is required.'
case "$(uname -m)" in x86_64|aarch64) ;; *) fail 'Supported architectures: amd64 and arm64.';; esac
domain=${1:-}
channel=${2:-release}
installation=${3:-/opt/cove}
case "$domain" in ''|*[!a-zA-Z0-9.-]*|.*|-*|*.) fail 'Usage: sh install.sh cove.example.com [release|beta] [/opt/cove]';; esac
case "$channel" in release|beta) ;; *) fail 'Choose release or beta.';; esac
case "$installation" in /*) ;; *) fail 'Use an absolute installation directory.';; esac
case "$installation" in /|*/../*|*/..|*[!a-zA-Z0-9_./-]*) fail 'Use a simple absolute installation directory.';; esac
[ ! -e "$installation" ] || fail 'Installation directory already exists. Existing installations must use the updater.'
command -v systemctl >/dev/null || fail 'A systemd host is required.'
[ -d /run/systemd/system ] || fail 'Systemd must be running.'
. /etc/os-release
case "$ID" in
  ubuntu|debian) manager=apt; docker_distribution=$ID ;;
  rhel) manager=dnf; docker_distribution=rhel ;;
  almalinux|rocky|centos) manager=dnf; docker_distribution=centos ;;
  *) fail 'Automatic installation supports Ubuntu, Debian, RHEL, AlmaLinux, Rocky Linux and CentOS Stream.' ;;
esac

echo 'Installing dashboard prerequisites...'
if [ "$manager" = apt ]; then
  command -v apt-get >/dev/null || fail 'APT is required.'
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y ca-certificates curl openssl python3 iproute2
else
  command -v dnf >/dev/null || fail 'DNF is required.'
  case "${VERSION_ID%%.*}" in 9|10) ;; *) fail 'Automatic RPM installation requires a supported EL 9 or 10 host.' ;; esac
  dnf install -y ca-certificates openssl python3 iproute dnf-plugins-core
  if ! command -v curl >/dev/null; then dnf install -y curl; fi
fi
if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
 if [ "$manager" = apt ]; then
  install -d -m 0755 /etc/apt/keyrings
  curl --fail --silent --show-error --proto '=https' --tlsv1.2 "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
  chmod 0644 /etc/apt/keyrings/docker.asc
  codename=${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}
  [ -n "$codename" ] || fail 'Could not determine the distribution codename.'
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' "$(dpkg --print-architecture)" "$ID" "$codename" > /etc/apt/sources.list.d/docker.list
  apt-get update
  if command -v docker >/dev/null; then
    apt-get install -y docker-compose-plugin
  else
    apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  fi
 else
  if [ ! -e /etc/yum.repos.d/docker-ce.repo ]; then
    dnf config-manager --add-repo "https://download.docker.com/linux/$docker_distribution/docker-ce.repo"
  fi
  if command -v docker >/dev/null; then
    dnf install -y docker-compose-plugin
  else
    dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  fi
 fi
fi
docker compose version >/dev/null || fail 'Docker Compose plugin is unavailable.'
if ! docker info >/dev/null 2>&1; then systemctl enable --now docker; fi
docker info >/dev/null || fail 'Docker is unavailable.'
if ss -H -ltn '( sport = :80 or sport = :443 )' | grep -q .; then
  fail 'Ports 80 and 443 are already in use. Choose a dedicated dashboard host.'
fi

image="ghcr.io/bwmp-dev/cove:$channel"
docker pull "$image" || fail "No published $channel image is available. Try beta if no stable release has been published yet."
revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")
version=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$image")
python3 - "$revision" "$version" "$channel" <<'PY'
import re, sys
revision, version, channel = sys.argv[1:]
pattern = r"v[0-9]+\.[0-9]+\.[0-9]+" if channel == "release" else r"beta-" + revision
if not re.fullmatch(r"[0-9a-f]{40}", revision) or not re.fullmatch(pattern, version):
    sys.exit("The image does not have valid release metadata.")
PY
mkdir -m 0700 "$installation"
install -d -m 0700 "$installation/deploy" "$installation/scripts"
for file in compose.yaml deploy/traefik.yaml deploy/traefik-panel.yaml scripts/configure.sh scripts/update-panel.py scripts/install-auto-update.sh scripts/install-panel-recovery.sh scripts/panel-recovery.py scripts/backup-panel.py; do
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "https://raw.githubusercontent.com/bwmp-dev/Cove/$revision/$file" -o "$installation/$file"
done
sh "$installation/scripts/configure.sh" "$domain"
printf 'COVE_VERSION=%s\n' "$version" >> "$installation/.env"
cd "$installation"
unset COVE_DOMAIN POSTGRES_PASSWORD COVE_ENCRYPTION_KEY COVE_ADMIN_TOKEN COVE_VERSION COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_PROFILES COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE
docker compose pull
docker compose up -d --no-build --wait --wait-timeout 180
echo 'Waiting for public HTTPS. DNS must point to this host and ports 80/443 must be reachable...'
python3 - "$installation/.env" "$channel" <<'PY'
import json, pathlib, sys, time, urllib.error, urllib.request
env = dict(line.split("=", 1) for line in pathlib.Path(sys.argv[1]).read_text().splitlines() if "=" in line)
origin = "https://" + env["COVE_DOMAIN"]
deadline = time.monotonic() + 180
while True:
    try:
        with urllib.request.urlopen(origin + "/healthz", timeout=5) as response:
            if response.status == 200:
                break
    except (urllib.error.URLError, TimeoutError):
        pass
    if time.monotonic() >= deadline:
        sys.exit("HTTPS is not ready. Services and credentials were preserved; check DNS and docker compose logs, then install the update timer with scripts/install-auto-update.sh.")
    time.sleep(3)
request = urllib.request.Request(origin + "/api/updates", method="PUT", data=json.dumps({"channel": sys.argv[2]}).encode(), headers={"Authorization": "Bearer " + env["COVE_ADMIN_TOKEN"], "Content-Type": "application/json"})
with urllib.request.urlopen(request, timeout=15) as response:
    if response.status != 200:
        sys.exit("Could not set the update channel.")
PY
sh "$installation/scripts/install-auto-update.sh" "$installation"
echo "Cove is ready at https://$domain/register. Register now: the first account becomes the administrator."
echo "Configure SMTP and GitHub in Settings. Back up $installation/.env securely; it contains the encryption key."
