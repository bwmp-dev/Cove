# Cove

A self-hosted control panel for deploying Compose applications to Linux workers over SSH.

## Setup

On a Linux host with Docker Compose:

```sh
sh scripts/configure.sh cove.example.com
docker compose up -d --build
```

Configure DNS for the panel hostname and make ports 80 and 443 reachable. The initial administrator password is stored in `.env`; keep that file private and back it up securely.

## Documentation

- [Development, configuration and updates](docs/DEVELOPMENT.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Interface design](docs/DESIGN.md)
- [Frontend development](frontend/README.md)
