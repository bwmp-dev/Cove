# Cove

A self-hosted control panel for deploying Compose applications to Linux workers over SSH.

## Setup

On a fresh Ubuntu or Debian host with systemd (amd64 or arm64), point a hostname at the server and make ports 80 and 443 reachable:

```sh
curl -fsSLo cove-install.sh https://raw.githubusercontent.com/bwmp-dev/Cove/main/scripts/install.sh
sudo sh cove-install.sh cove.example.com beta
```

Use `release` instead of `beta` for published stable builds. The installer installs Docker Compose and prerequisites, starts PostgreSQL, Cove and HTTPS ingress, generates private database credentials and encryption keys, and enables automatic updates. It refuses to overwrite an existing installation.

Open the displayed URL and register immediately. The first account becomes the instance administrator; further registration stays closed until enabled in Settings. Configure SMTP and GitHub sign-in in **Settings**, and repository access in **Integrations**. Keep `/opt/cove/.env` private and back it up securely: the encryption key is required to recover saved secrets.

For an existing checkout with Docker Compose, run `sh scripts/configure.sh cove.example.com` followed by `docker compose up -d --build`. No administrator password or integration credentials are needed in Compose.

## Documentation

- [Development, configuration and updates](docs/DEVELOPMENT.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Interface design](docs/DESIGN.md)
- [Frontend development](frontend/README.md)
