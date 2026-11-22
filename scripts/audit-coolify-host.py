"""Read-only migration inventory. Run on the source Docker host; never exports secrets."""
import json
import subprocess


def summarize(container):
    config = container.get("Config") or {}
    host = container.get("HostConfig") or {}
    labels = config.get("Labels") or {}
    mounts = container.get("Mounts") or []
    checks = []
    if any(m.get("RW") and m.get("Type") in ("bind", "volume") for m in mounts):
        checks.append("restore-test-persistent-data")
    if any(m.get("Type") == "tmpfs" for m in mounts):
        checks.append("review-ephemeral-storage-and-recovery")
    if any(m.get("Type") == "bind" for m in mounts):
        checks.append("transfer-bind-mounts-and-permissions")
    if any(m.get("Destination", "").endswith("docker.sock") for m in mounts):
        checks.append("docker-socket-host-access")
    if host.get("Privileged") or host.get("CapAdd") or host.get("Devices"):
        checks.append("review-host-privileges-and-devices")
    if host.get("NetworkMode") == "host":
        checks.append("host-network-port-conflicts")
    if host.get("NetworkMode", "").startswith("container:"):
        checks.append("preserve-shared-network-namespace")
    if host.get("PortBindings"):
        checks.append("review-published-ports-and-firewall")
    if any(key.startswith(("traefik.", "caddy")) for key in labels):
        checks.append("replace-coolify-ingress-labels")
    image = config.get("Image", "")
    if any(engine in image.lower() for engine in ("postgres", "timescale", "mariadb", "mysql", "redis", "clickhouse")):
        checks.append("preserve-database-version-and-test-restore")
    return {
        "name": container.get("Name", "").lstrip("/"),
        "project": labels.get("com.docker.compose.project", ""),
        "service": labels.get("com.docker.compose.service", ""),
        "image": image,
        "state": (container.get("State") or {}).get("Status"),
        "restart_policy": (host.get("RestartPolicy") or {}).get("Name"),
        "exit_code": (container.get("State") or {}).get("ExitCode"),
        "mounts": [{"type": m.get("Type"), "destination": m.get("Destination"),
                    "writable": bool(m.get("RW"))} for m in mounts],
        "networks": sorted(((container.get("NetworkSettings") or {}).get("Networks") or {}).keys()),
        "published_ports": host.get("PortBindings") or {},
        "checks": checks,
    }


def main():
    ids = subprocess.check_output(["docker", "ps", "-aq"], text=True).split()
    containers = []
    for offset in range(0, len(ids), 50):
        batch = json.loads(subprocess.check_output(["docker", "inspect", *ids[offset:offset + 50]]))
        containers.extend(summarize(container) for container in batch)
    print(json.dumps({"containers": containers}, indent=2))


if __name__ == "__main__":
    main()
