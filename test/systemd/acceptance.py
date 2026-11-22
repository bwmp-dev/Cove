"""Destructive only inside the dedicated disposable Cove acceptance VM; run as root."""
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import ssl
import subprocess
import tempfile
import time
import urllib.request


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    if os.geteuid() != 0 or Path("/etc/hostname").read_text().strip() != "cove-acceptance":
        raise SystemExit("This test only runs as root on the isolated cove-acceptance VM.")
    os.umask(0o077)
    marker = Path("/etc/cove/node-id")
    if marker.exists():
        run("systemctl", "stop", "cove-worker")
        marker.unlink()
    root = Path(tempfile.mkdtemp(prefix="cove-systemd-"))
    binaries = Path(__file__).resolve().parent
    env = os.environ.copy()
    password = secrets.token_hex(24)
    token = secrets.token_hex(32)
    database = "cove_test_" + secrets.token_hex(6)
    run("sudo", "-u", "postgres", "psql", "-v", "ON_ERROR_STOP=1", input=f"CREATE ROLE {database} LOGIN PASSWORD '{password}';\nCREATE DATABASE {database} OWNER {database};\n", text=True, stdout=subprocess.DEVNULL)
    env.update(DATABASE_URL=f"postgres://{database}:{password}@127.0.0.1/{database}?sslmode=disable",
               COVE_ENCRYPTION_KEY=base64.b64encode(secrets.token_bytes(32)).decode(),
               COVE_ADMIN_TOKEN=token, COVE_ADMIN_PASSWORD=secrets.token_hex(24),
               COVE_PUBLIC_URL="https://localhost:8443", COVE_LISTEN="127.0.0.1:8080",
               COVE_LOCAL_SOCKET="/run/cove/worker.sock",
               COVE_BINARY_DIR=str(binaries))
    log = (root / "acceptance.log").open("wb")
    certificate = root / "cert.pem"
    run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
        "-keyout", str(root / "key.pem"), "-out", str(certificate), "-subj", "/CN=localhost",
        "-addext", "subjectAltName=DNS:localhost", stdout=log, stderr=log)
    (root / "routes.yaml").write_text(json.dumps({
        "http":{"routers":{"panel":{"rule":"Host(`localhost`)","entryPoints":["websecure"],"service":"panel","tls":{}}},
                "services":{"panel":{"loadBalancer":{"servers":[{"url":"http://127.0.0.1:8080"}]}}}},
        "tls":{"certificates":[{"certFile":"/fixture/cert.pem","keyFile":"/fixture/key.pem"}]}}))
    panel = subprocess.Popen([str(binaries / "cove")], env=env, stdout=log, stderr=log)
    proxy = "cove-systemd-tls-" + secrets.token_hex(6)
    try:
        run("docker", "run", "-d", "--name", proxy, "--network", "host", "-v", str(root)+":/fixture:ro",
            "traefik:v3.7.19", "--entrypoints.websecure.address=:8443", "--providers.file.filename=/fixture/routes.yaml", stdout=log, stderr=log)
        shutil.copyfile(certificate, "/usr/local/share/ca-certificates/cove-acceptance.crt")
        run("update-ca-certificates", stdout=log, stderr=log)
        context = ssl.create_default_context(cafile=str(certificate))

        def api(method, path, body=None):
            request = urllib.request.Request("https://localhost:8443"+path, method=method,
                data=json.dumps(body).encode() if body is not None else None,
                headers={"Authorization": "Bearer "+token, "Content-Type": "application/json"})
            with urllib.request.urlopen(request, context=context, timeout=60) as response:
                return json.load(response)

        for _ in range(120):
            try:
                api("GET", "/api/nodes")
                break
            except OSError:
                time.sleep(1)
        node = api("POST", "/api/nodes", {"name": "systemd-worker", "address": "127.0.0.1", "port": 22})

        def install(enrollment):
            run("sh", input=enrollment["setup_command"], text=True, stdout=log, stderr=log, timeout=1800)
            nodes = api("GET", "/api/nodes")
            if next(item for item in nodes if item["id"] == node["id"])["state"] != "ready":
                raise RuntimeError("Generated installer did not produce a ready SSH worker")

        install(node)
        local = api("POST", "/api/nodes/local")
        project = api("POST", "/api/projects", {"name": "Systemd acceptance"})["id"]
        workload = api("POST", "/api/workloads", {"project_id": project, "name": "systemd-local-service", "node_id": local["id"], "kind": "service",
            "compose": "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'sleep 600']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n"})

        def operation(action):
            queued = api("POST", "/api/workloads/"+workload["id"]+"/operations", {"action": action})
            for _ in range(120):
                current = api("GET", "/api/operations/"+queued["id"])
                if current["state"] == "succeeded": return
                if current["state"] not in ("queued", "running"): raise RuntimeError("Local worker operation failed: "+current["state"])
                time.sleep(2)
            raise RuntimeError("Local worker operation timed out")

        operation("deploy")
        container_before = subprocess.check_output(["docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload["id"]], text=True).strip()
        if not container_before: raise RuntimeError("Local deployment created no running container")
        before = subprocess.check_output(["systemctl", "show", "cove-worker", "--property=MainPID", "--value"], text=True).strip()
        install(api("POST", "/api/nodes/"+node["id"]+"/enrollment"))
        after = subprocess.check_output(["systemctl", "show", "cove-worker", "--property=MainPID", "--value"], text=True).strip()
        if before == after or after == "0": raise RuntimeError("Reinstallation did not restart the worker")
        container_after = subprocess.check_output(["docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload["id"]], text=True).strip()
        if container_before != container_after: raise RuntimeError("Worker upgrade replaced the running application")
        if Path("/var/lib/cove/draining.json").exists(): raise RuntimeError("Worker remained drained")
        installed = Path("/usr/local/bin/cove-worker")
        original_hash = hashlib.sha256(installed.read_bytes()).hexdigest()
        binary = binaries / "cove-worker-linux-amd64"
        original = binary.read_bytes()
        try:
            binary.write_bytes(b"#!/bin/sh\nexit 1\n")
            enrollment = api("POST", "/api/nodes/"+node["id"]+"/enrollment")
            result = subprocess.run(["sh"], input=enrollment["setup_command"], text=True, stdout=log, stderr=log, timeout=180)
            if result.returncode == 0: raise RuntimeError("Broken replacement unexpectedly passed readiness")
            if hashlib.sha256(installed.read_bytes()).hexdigest() != original_hash:
                raise RuntimeError("Installer did not restore the previous executable")
            run("systemctl", "is-active", "--quiet", "cove-worker")
            if Path("/var/lib/cove/draining.json").exists(): raise RuntimeError("Restored worker remained drained")
        finally:
            binary.write_bytes(original)
        operation("stop")
        run("docker", "rm", container_before, stdout=log, stderr=log)
        print("PASS: generated install, pinned SSH readiness, local socket deployment, application survival, reenrollment, daemon replacement, failed-upgrade restoration and resume", flush=True)
        print("Acceptance log: "+str(root / "acceptance.log"), flush=True)
    finally:
        run("docker", "rm", "-f", proxy, stdout=log, stderr=log)
        for process in (panel,):
            process.terminate()
            try: process.wait(timeout=30)
            except subprocess.TimeoutExpired: process.kill(); process.wait()
        log.close()
        run("sudo", "-u", "postgres", "psql", "-v", "ON_ERROR_STOP=1", input=f"DROP DATABASE {database};\nDROP ROLE {database};\n", text=True, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
