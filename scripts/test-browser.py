"""Run browser acceptance against disposable packaged Cove and PostgreSQL containers."""
import base64
import json
import os
from pathlib import Path
import secrets
import re
import shutil
import socket
import ssl
import sys
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, check=True, cwd=ROOT, **kwargs)


def output_lines(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout.split()


def main():
    name = "cove-browser-" + secrets.token_hex(6)
    panel_image = name + "-panel:development"
    worker_image = name + "-worker:development"
    base_path = os.environ.get("COVE_BROWSER_BASE_PATH", "")
    if not re.fullmatch(r"(?:/[A-Za-z0-9_-]+)*", base_path) or len(base_path) > 200:
        raise ValueError("Use a simple COVE_BROWSER_BASE_PATH without a trailing slash")
    containers = []
    network_created = False
    ingress = []
    worker_state_owned = False
    links_owned = False
    fixture_workloads = []
    env = os.environ.copy()
    env.update(POSTGRES_PASSWORD=secrets.token_hex(24),
               COVE_ADMIN_PASSWORD=secrets.token_hex(24),
               COVE_ADMIN_TOKEN=secrets.token_hex(32),
               COVE_ENCRYPTION_KEY=base64.b64encode(secrets.token_bytes(32)).decode())
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        fixture = Path(directory)
        openssl = shutil.which("openssl") or ("C:/Program Files/Git/usr/bin/openssl.exe" if os.name == "nt" else "openssl")
        (fixture / "openssl.cnf").write_text("[req]\ndistinguished_name=dn\n[dn]\n")
        run(openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
            "-config", str(fixture / "openssl.cnf"), "-keyout", str(fixture / "key.pem"),
            "-out", str(fixture / "cert.pem"), "-subj", "/CN=localhost",
            "-addext", "subjectAltName=DNS:localhost", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        (fixture / "routes.yaml").write_text(json.dumps({
            "http":{"routers":{"panel":{"rule":"Host(`localhost`)","entryPoints":["websecure"],"service":"panel","tls":{}}},
                    "services":{"panel":{"loadBalancer":{"servers":[{"url":"http://panel:8080"}]}}}},
            "tls":{"certificates":[{"certFile":"/fixture/cert.pem","keyFile":"/fixture/key.pem"}]}}))
        try:
            # Claim the fixed ingress name before building so parallel tracks cannot overlap.
            while True:
                occupied = any(subprocess.run(["docker", kind, "inspect", "cove-ingress"], capture_output=True).returncode == 0 for kind in ("container", "network", "volume"))
                if not occupied:
                    claim = subprocess.run(["docker", "network", "create", "--label", "cove.managed=ingress", "cove-ingress"], capture_output=True)
                    if claim.returncode == 0:
                        ingress.append("network")
                        break
                    raced = subprocess.run(["docker", "network", "inspect", "cove-ingress"], capture_output=True).returncode == 0
                    if not raced or os.environ.get("COVE_BROWSER_WAIT") != "1":
                        raise RuntimeError("Could not claim the browser ingress network: " + claim.stderr.decode(errors="replace").strip())
                elif os.environ.get("COVE_BROWSER_WAIT") != "1":
                    raise RuntimeError("Refusing to replace an existing cove-ingress resource")
                time.sleep(3)
            run("docker", "build", "-t", panel_image, ".")
            run("docker", "network", "create", name, stdout=subprocess.DEVNULL)
            network_created = True
            for suffix, image, extra in [
                ("db", "postgres:17-alpine", ["--network-alias", "db", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB=cove"]),
            ]:
                container = name + "-" + suffix
                run("docker", "run", "-d", "--name", container, "--network", name,
                    *extra, image, env=env, stdout=subprocess.DEVNULL)
                containers.append(container)
            for _ in range(60):
                ready = subprocess.run(["docker", "exec", name + "-db", "pg_isready", "-h", "127.0.0.1", "-U", "postgres"], capture_output=True)
                if ready.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("Disposable database did not start")
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            origin = f"https://localhost:{port}"
            panel_url = origin + base_path
            env.update(DATABASE_URL=f"postgres://postgres:{env['POSTGRES_PASSWORD']}@db:5432/cove?sslmode=disable", COVE_PUBLIC_URL=panel_url)
            options = [item for key in ("DATABASE_URL", "COVE_PUBLIC_URL", "COVE_ADMIN_PASSWORD", "COVE_ADMIN_TOKEN", "COVE_ENCRYPTION_KEY") for item in ("-e", key)]
            run("docker", "run", "-d", "--name", name + "-panel", "--network", name,
                "--network-alias", "panel", *options, panel_image, env=env, stdout=subprocess.DEVNULL)
            containers.append(name + "-panel")
            run("docker", "run", "-d", "--name", name + "-tls", "--network", name,
                "-p", f"127.0.0.1:{port}:443", "--mount", f"type=bind,source={fixture},target=/fixture,readonly",
                "traefik:v3.7.19", "--entrypoints.websecure.address=:443", "--providers.file.filename=/fixture/routes.yaml", stdout=subprocess.DEVNULL)
            containers.append(name + "-tls")
            context = ssl.create_default_context(cafile=str(fixture / "cert.pem"))
            for _ in range(60):
                try:
                    with urllib.request.urlopen(origin + "/healthz", context=context, timeout=2) as response:
                        if response.status == 200:
                            break
                except (OSError, urllib.error.URLError):
                    time.sleep(1)
            else:
                raise RuntimeError("Disposable panel did not start")

            def api(method, path, body=None, credential=None, raw=False):
                request = urllib.request.Request(panel_url+path, method=method,
                    data=json.dumps(body).encode() if body is not None else None,
                    headers={"Authorization": "Bearer "+(credential or env["COVE_ADMIN_TOKEN"]), "Content-Type": "application/json"})
                with urllib.request.urlopen(request, context=context, timeout=60) as response:
                    return response.read().decode() if raw else json.load(response)

            search = api("GET", "/api/search?q=cove-group19-fixture")
            timezone = api("GET", "/api/preferences/timezone")
            roles = api("GET", "/api/teams/default/roles")
            if not isinstance(search.get("items"), list) or search.get("limit") != 40 or timezone.get("schedule_timezone") != "UTC" or "read_secrets" not in roles.get("permission_catalogue", []):
                raise RuntimeError("Packaged panel does not expose the merged integration/trust contract")
            run("go", "build", "-o", "bin/cove-worker-linux-amd64", "./cmd/cove-worker", env=env | {"GOOS": "linux", "GOARCH": "amd64"})
            run("docker", "build", "-f", "test/worker/Dockerfile", "-t", worker_image, ".", stdout=subprocess.DEVNULL)
            for resource in ("container", "volume"):
                if subprocess.run(["docker", resource, "inspect", "cove-ingress"], capture_output=True).returncode == 0:
                    raise RuntimeError("Refusing to replace an existing cove-ingress " + resource)
            # Private connection tests create the worker's link gateway and networks on this Docker host.
            if subprocess.run(["docker", "container", "inspect", "cove-link"], capture_output=True).returncode == 0 or output_lines("docker", "network", "ls", "-q", "--filter", "label=cove.managed=link") or output_lines("docker", "network", "ls", "-q", "--filter", "label=cove.managed=link-mesh"):
                raise RuntimeError("Refusing to run with an existing Cove private link gateway or networks on this Docker host")
            links_owned = True
            # The worker adopts this Cove-labelled proxy for route tests. It publishes no host ports, and
            # its ACME resolver points at a closed loopback port so no public certificate authority is contacted.
            for resource in ("volume",):
                run("docker", resource, "create", "--label", "cove.managed=ingress", "cove-ingress", stdout=subprocess.DEVNULL)
                ingress.append(resource)
            run("docker", "create", "--name", "cove-ingress", "--label", "cove.managed=ingress", "--network", "cove-ingress",
                "--security-opt", "no-new-privileges:true", "-v", "cove-ingress:/data", "--entrypoint", "sh", "traefik:v3.7.19",
                "-ec", 'mkdir -p /data/dynamic; exec traefik "$@"', "traefik",
                "--entrypoints.web.address=:80", "--entrypoints.websecure.address=:443",
                "--entrypoints.traefik.address=127.0.0.1:8080", "--api.insecure=true", "--api.dashboard=false",
                "--providers.file.directory=/data/dynamic", "--providers.file.watch=true",
                "--certificatesresolvers.cove.acme.storage=/data/traefik-acme.json",
                "--certificatesresolvers.cove.acme.httpchallenge.entrypoint=web",
                "--certificatesresolvers.cove.acme.caserver=https://127.0.0.1:9/directory", stdout=subprocess.DEVNULL)
            ingress.append("container")
            run("docker", "start", "cove-ingress", stdout=subprocess.DEVNULL)
            # Docker helpers need the same absolute state path as this containerized worker.
            # Refuse any existing host state rather than adopting or overwriting it.
            run("docker", "run", "--rm", "--entrypoint", "sh", "-v", "/var/lib/cove:/fixture",
                worker_image, "-ec",
                'test -z "$(ls -A /fixture)" || { echo "Refusing existing /var/lib/cove state" >&2; exit 1; }; cp -a /var/lib/cove/. /fixture/; printf %s "$1" > /fixture/.browser-owner; chown -R cove:cove /fixture', "sh", name)
            worker_state_owned = True
            worker = name + "-worker"
            run("docker", "run", "-d", "--name", worker, "--network", name, "--network-alias", "worker",
                "-p", "127.0.0.1::22", "-v", "/var/lib/cove:/var/lib/cove", "-v", "/var/run/docker.sock:/var/run/docker.sock", worker_image, stdout=subprocess.DEVNULL)
            containers.append(worker)

            enrollment = api("POST", "/api/nodes", {"name": "ready-browser-worker", "address": "worker", "port": 22})
            credential = re.search(r"Authorization: Bearer ([^']+)", enrollment["setup_command"]).group(1)
            installer = api("GET", "/install.sh", credential=credential, raw=True)
            authorized = re.search(r"'(restrict,command=\"[^\"]+\" ssh-ed25519 [A-Za-z0-9+/=]+)'", installer).group(1)
            run("docker", "exec", "-i", worker, "sh", "-c", "cat > /var/lib/cove/.ssh/authorized_keys && chown cove:cove /var/lib/cove/.ssh/authorized_keys && chmod 600 /var/lib/cove/.ssh/authorized_keys", input=authorized+"\n", text=True)
            host_key = run("docker", "exec", worker, "cat", "/etc/ssh/ssh_host_ed25519_key.pub", capture_output=True, text=True).stdout.strip()
            api("POST", "/bootstrap/register", {"host_key": host_key}, credential=credential)
            for _ in range(30):
                if next(item for item in api("GET", "/api/nodes") if item["id"] == enrollment["id"])["state"] == "ready": break
                api("POST", "/api/nodes/"+enrollment["id"]+"/verify")
                time.sleep(1)
            else: raise RuntimeError("Browser worker did not become ready")
            client_binary=fixture / ("cove.exe" if os.name == "nt" else "cove")
            run("go", "build", "-o", str(client_binary), "./cmd/cove")
            ssh_port=run("docker", "port", worker, "22/tcp",capture_output=True,text=True).stdout.strip().rsplit(":",1)[1]
            env.update(COVE_BROWSER_DATABASE=name+"-db", COVE_BROWSER_WORKER=worker, COVE_BROWSER_CLIENT=str(client_binary), COVE_BROWSER_SSH_PORT=ssh_port, COVE_BROWSER_URL=origin, COVE_BROWSER_PASSWORD=env["COVE_ADMIN_PASSWORD"])
            subprocess.run([shutil.which("pnpm"), "test:e2e", *sys.argv[1:]], cwd=ROOT / "frontend", env=env, check=True)
        finally:
            try:
                if name+"-db" in containers:
                    if name+"-worker" in containers:
                        subprocess.run(["docker", "stop", "--time", "10", name+"-worker"], check=True, stdout=subprocess.DEVNULL)
                    operations = subprocess.run(["docker", "exec", name+"-db", "psql", "-U", "postgres", "-d", "cove", "-Atc", "SELECT id FROM operations"], capture_output=True, text=True, check=True)
                    for operation in operations.stdout.splitlines():
                        if not re.fullmatch(r"[0-9a-f]{32}", operation): raise RuntimeError("Unexpected fixture operation ID")
                        for label in ("dev.cove.operation=", "dev.cove.hook=", "dev.cove.drill=", "dev.cove.candidate="):
                            helpers = subprocess.run(["docker", "ps", "-aq", "--filter", "label="+label+operation], capture_output=True, text=True, check=True)
                            for helper in helpers.stdout.split(): subprocess.run(["docker", "rm", "-f", "-v", helper], check=True, stdout=subprocess.DEVNULL)
                    records = subprocess.run(["docker", "exec", name+"-db", "psql", "-U", "postgres", "-d", "cove", "-Atc", "SELECT id FROM workloads UNION SELECT subject FROM audit_events WHERE action='DELETE /api/workloads/{id}' AND status=200"], capture_output=True, text=True)
                    if records.returncode == 0:
                        for workload in records.stdout.splitlines():
                            if not re.fullmatch(r"[0-9a-f]{32}", workload): raise RuntimeError("Unexpected fixture workload ID")
                            fixture_workloads.append(workload)
                            for listing, removal in [(["ps", "-aq"], ["rm", "-f", "-v"]), (["network", "ls", "-q"], ["network", "rm"])]:
                                owned = subprocess.run(["docker", *listing, "--filter", "label=com.docker.compose.project=cove-"+workload], capture_output=True, text=True, check=True)
                                for resource in owned.stdout.split(): subprocess.run(["docker", *removal, resource], check=True, stdout=subprocess.DEVNULL)
                            for label in ("com.docker.compose.project=cove-"+workload, "cove.backup="+workload, "dev.cove.restore="+workload):
                                volumes = subprocess.run(["docker", "volume", "ls", "-q", "--filter", "label="+label], capture_output=True, text=True, check=True)
                                for volume in volumes.stdout.split(): subprocess.run(["docker", "volume", "rm", volume], check=True, stdout=subprocess.DEVNULL)
                            tags = subprocess.run(["docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-release/"+workload+":*"], capture_output=True, text=True, check=True)
                            for tag in tags.stdout.split(): subprocess.run(["docker", "image", "rm", tag], check=True, stdout=subprocess.DEVNULL)
                    networks = subprocess.run(["docker", "exec", name+"-db", "psql", "-U", "postgres", "-d", "cove", "-Atc", "SELECT id FROM shared_networks"], capture_output=True, text=True, check=True)
                    for network in networks.stdout.splitlines():
                        if not re.fullmatch(r"[0-9a-f]{32}", network): raise RuntimeError("Unexpected fixture network ID")
                        subprocess.run(["docker", "network", "rm", "cove-shared-"+network], check=True, stdout=subprocess.DEVNULL)
            finally:
                for container in reversed(containers):
                    subprocess.run(["docker", "rm", "-f", "-v", container], check=False, stdout=subprocess.DEVNULL)
                if links_owned:
                    gateway = subprocess.run(["docker", "inspect", "--format", '{"id":{{json .Id}},"mounts":{{json .Mounts}}}', "cove-link"], capture_output=True, text=True)
                    owned_gateway = False
                    if gateway.returncode == 0 and worker_state_owned:
                        info = json.loads(gateway.stdout)
                        owned_gateway = any(mount.get("Source") == "/var/lib/cove/links/gateway" and mount.get("Destination") == "/config" for mount in info["mounts"])
                        if owned_gateway:
                            subprocess.run(["docker", "rm", "-f", info["id"]], check=False, stdout=subprocess.DEVNULL)
                    for workload in fixture_workloads:
                        for network in output_lines("docker", "network", "ls", "-q", "--filter", "label=cove.managed=link", "--filter", "label=cove.link-workload=" + workload):
                            subprocess.run(["docker", "network", "rm", network], check=False, stdout=subprocess.DEVNULL)
                    if owned_gateway:
                        subprocess.run(["docker", "network", "rm", "cove-link-mesh"], check=False, stdout=subprocess.DEVNULL)
                if worker_state_owned:
                    run("docker", "run", "--rm", "--entrypoint", "sh", "-v", "/var/lib/cove:/fixture",
                        worker_image, "-ec",
                        'test "$(cat /fixture/.browser-owner)" = "$1" || exit 1; find /fixture -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +', "sh", name)
                if network_created:
                    subprocess.run(["docker", "network", "rm", name], check=False, stdout=subprocess.DEVNULL)
                for resource in reversed(ingress):
                    subprocess.run(["docker", resource, "rm", "-f", "cove-ingress"] if resource == "container" else ["docker", resource, "rm", "cove-ingress"], check=False, stdout=subprocess.DEVNULL)
                for image in (panel_image, worker_image):
                    subprocess.run(["docker", "image", "rm", image], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
