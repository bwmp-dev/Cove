"""Run browser acceptance against disposable packaged Cove and PostgreSQL containers."""
import base64
from contextlib import contextmanager
import errno
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


@contextmanager
def browser_fixture_lock():
    # Proxy/link names and worker state are shared across worktrees on the same Docker host.
    path = Path(tempfile.gettempdir()) / "cove-browser-fixture.lock"
    with path.open("a+b") as lock:
        lock.seek(0, os.SEEK_END)
        if lock.tell() == 0:
            lock.write(b"\0")
            lock.flush()
        waiting = False
        while True:
            lock.seek(0)
            try:
                if os.name == "nt":
                    import msvcrt
                    msvcrt.locking(lock.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except OSError as error:
                if error.errno not in (errno.EACCES, errno.EAGAIN, errno.EDEADLK):
                    raise
                if not waiting:
                    print("Waiting for another Cove browser fixture to finish...", flush=True)
                    waiting = True
                time.sleep(3)
        try:
            yield
        finally:
            lock.seek(0)
            if os.name == "nt":
                msvcrt.locking(lock.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(lock.fileno(), fcntl.LOCK_UN)


def run(*args, **kwargs):
    return subprocess.run(args, check=True, cwd=ROOT, **kwargs)


def output_lines(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout.split()


def worker_bind_helpers(previous):
    helpers = set()
    for container in set(output_lines("docker", "ps", "-aq")) - previous:
        details = json.loads(run("docker", "inspect", container, capture_output=True, text=True).stdout)[0]
        if any(mount.get("Type") == "bind" and mount.get("Source", "").startswith("/var/lib/cove/") for mount in details["Mounts"]):
            helpers.add(container)
    return helpers


def cleanup_fixture_resources(workloads, operations, shared_networks, links_owned, bound_helpers=()):
    for identifier in set(workloads) | set(operations) | set(shared_networks):
        if not re.fullmatch(r"[0-9a-f]{32}", identifier):
            raise RuntimeError("Unexpected fixture resource ID")
    labels = [prefix + identifier for identifier in operations for prefix in
              ("dev.cove.operation=", "dev.cove.hook=", "dev.cove.drill=", "dev.cove.candidate=")]
    labels += [prefix + identifier for identifier in workloads for prefix in
               ("com.docker.compose.project=cove-", "cove.database-tool.workload=", "cove.route-aux-owner=", "dev.cove.environment-preview=")]
    containers = set(bound_helpers)
    for label in labels:
        containers.update(output_lines("docker", "ps", "-aq", "--filter", "label=" + label))
    if links_owned:
        containers.update(output_lines("docker", "ps", "-aq", "--filter", "name=^cove-link$", "--filter", "label=cove.managed=link"))
        for label in ("cove.managed=ingress-check", "cove.managed=ingress-helper"):
            for container in output_lines("docker", "ps", "-aq", "--filter", "label=" + label):
                details = json.loads(run("docker", "inspect", container, capture_output=True, text=True).stdout)[0]
                if any(mount.get("Name") == "cove-ingress" for mount in details["Mounts"]):
                    containers.add(container)
    for container in sorted(containers):
        run("docker", "rm", "-f", "-v", container, stdout=subprocess.DEVNULL)
    network_labels = [prefix + identifier for identifier in workloads for prefix in
                      ("com.docker.compose.project=cove-", "cove.link-workload=", "cove.route-aux-owner=", "dev.cove.environment-preview=")]
    network_labels += ["cove.shared-network=" + identifier for identifier in shared_networks]
    if links_owned:
        network_labels.append("cove.managed=link-mesh")
    networks = set()
    for label in network_labels:
        networks.update(output_lines("docker", "network", "ls", "-q", "--filter", "label=" + label))
    for network in sorted(networks):
        run("docker", "network", "rm", network, stdout=subprocess.DEVNULL)
    for workload in workloads:
        for prefix in ("com.docker.compose.project=cove-", "cove.backup=", "dev.cove.restore=", "dev.cove.environment-preview="):
            for volume in output_lines("docker", "volume", "ls", "-q", "--filter", "label=" + prefix + workload):
                run("docker", "volume", "rm", volume, stdout=subprocess.DEVNULL)
        for tag in output_lines("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-release/" + workload + ":*"):
            run("docker", "image", "rm", tag, stdout=subprocess.DEVNULL)


def assert_ingress_removed():
    remaining = []
    for resource in ("container", "network", "volume"):
        args = ["docker", resource, "ls", *(["-a"] if resource == "container" else []),
                "--format", "{{.Names}}" if resource == "container" else "{{.Name}}", "--filter", "name=^cove-ingress$"]
        if "cove-ingress" in output_lines(*args):
            remaining.append(resource)
    if remaining:
        raise RuntimeError("Fixture cleanup left cove-ingress " + ", ".join(remaining))


def main():
    name = "cove-browser-" + secrets.token_hex(6)
    panel_image = name + "-panel:acceptance"
    worker_image = name + "-worker:acceptance"
    containers = []
    network_created = False
    ingress = []
    worker_state_owned = False
    links_owned = False
    resources_cleaned = False
    worker_containers_before = set()
    link_images_before = set(output_lines("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-link:*"))
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
            env.update(DATABASE_URL=f"postgres://postgres:{env['POSTGRES_PASSWORD']}@db:5432/cove?sslmode=disable", COVE_PUBLIC_URL=origin)
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
            run("go", "build", "-o", "bin/cove-worker-linux-amd64", "./cmd/cove-worker", env=env | {"GOOS": "linux", "GOARCH": "amd64"})
            run("docker", "build", "-f", "test/worker/Dockerfile", "-t", worker_image, ".", stdout=subprocess.DEVNULL)
            for resource in ("container", "network", "volume"):
                if subprocess.run(["docker", resource, "inspect", "cove-ingress"], capture_output=True).returncode == 0:
                    raise RuntimeError("Refusing to replace an existing cove-ingress " + resource)
            # Private connection tests create the worker's link gateway and networks on this Docker host.
            if subprocess.run(["docker", "container", "inspect", "cove-link"], capture_output=True).returncode == 0 or output_lines("docker", "network", "ls", "-q", "--filter", "label=cove.managed=link") or output_lines("docker", "network", "ls", "-q", "--filter", "label=cove.managed=link-mesh"):
                raise RuntimeError("Refusing to run with an existing Cove private link gateway or networks on this Docker host")
            # The worker adopts this Cove-labelled proxy for route tests. It publishes no host ports, and
            # its ACME resolver points at a closed loopback port so no public certificate authority is contacted.
            for resource in ("network", "volume"):
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
            links_owned = True
            run("docker", "start", "cove-ingress", stdout=subprocess.DEVNULL)
            # Docker helpers need the same absolute state path as this containerized worker.
            # Refuse any existing host state rather than adopting or overwriting it.
            run("docker", "run", "--rm", "--entrypoint", "sh", "-v", "/var/lib/cove:/fixture",
                worker_image, "-ec",
                'test -z "$(ls -A /fixture)" || { echo "Refusing existing /var/lib/cove state" >&2; exit 1; }; cp -a /var/lib/cove/. /fixture/; printf %s "$1" > /fixture/.browser-owner; chown -R cove:cove /fixture', "sh", name)
            worker_state_owned = True
            worker_containers_before = set(output_lines("docker", "ps", "-aq"))
            worker = name + "-worker"
            run("docker", "run", "-d", "--name", worker, "--network", name, "--network-alias", "worker",
                "-p", "127.0.0.1::22", "-v", "/var/lib/cove:/var/lib/cove", "-v", "/var/run/docker.sock:/var/run/docker.sock", worker_image, stdout=subprocess.DEVNULL)
            containers.append(worker)

            def api(method, path, body=None, credential=None, raw=False):
                request = urllib.request.Request(origin+path, method=method,
                    data=json.dumps(body).encode() if body is not None else None,
                    headers={"Authorization": "Bearer "+(credential or env["COVE_ADMIN_TOKEN"]), "Content-Type": "application/json"})
                with urllib.request.urlopen(request, context=context, timeout=60) as response:
                    return response.read().decode() if raw else json.load(response)

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
            if sys.argv[1:] == ["--managed-data"] or "e2e/databases.spec.ts" in sys.argv[1:]:
                ready = next(item for item in api("GET", "/api/nodes") if item["id"] == enrollment["id"])
                if not ready.get("capabilities", {}).get("managed_databases") or len(api("GET", "/api/database-engines")["engines"]) != 7:
                    raise RuntimeError("Managed-data acceptance requires this branch's panel and worker capabilities")
            client_binary=fixture / ("cove.exe" if os.name == "nt" else "cove")
            run("go", "build", "-o", str(client_binary), "./cmd/cove")
            ssh_port=run("docker", "port", worker, "22/tcp",capture_output=True,text=True).stdout.strip().rsplit(":",1)[1]
            env.update(COVE_BROWSER_DATABASE=name+"-db", COVE_BROWSER_WORKER=worker, COVE_BROWSER_CLIENT=str(client_binary), COVE_BROWSER_SSH_PORT=ssh_port, COVE_BROWSER_URL=origin, COVE_BROWSER_PASSWORD=env["COVE_ADMIN_PASSWORD"])
            if sys.argv[1:] == ["--managed-data"]:
                env["COVE_BROWSER_CA"] = str(fixture / "cert.pem")
                env["COVE_BROWSER_LOCK_OWNER"] = str(os.getpid())
                subprocess.run([sys.executable, str(ROOT / "scripts/test-managed-data.py")], cwd=ROOT, env=env, check=True)
            else:
                subprocess.run([shutil.which("pnpm"), "test:e2e", *sys.argv[1:]], cwd=ROOT / "frontend", env=env, check=True)
        finally:
            workloads, operation_ids, shared_networks, bound_helpers = set(), set(), set(), set()
            try:
                try:
                    if name+"-worker" in containers:
                        subprocess.run(["docker", "stop", "--time", "10", name+"-worker"], check=True, stdout=subprocess.DEVNULL)
                    if worker_state_owned:
                        owners = run("docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", "-v", "/var/lib/cove:/fixture:ro",
                            worker_image, "-ec", 'test "$(cat /fixture/.browser-owner)" = "$1" || exit 1; for dir in /fixture/workloads/*; do test ! -d "$dir" || printf "w:%s\\n" "${dir##*/}"; done; for file in /fixture/operations/*.json; do test ! -f "$file" || { id="${file##*/}"; printf "o:%s\\n" "${id%.json}"; }; done', "sh", name,
                            capture_output=True, text=True)
                        for owner in owners.stdout.splitlines():
                            (workloads if owner.startswith("w:") else operation_ids).add(owner[2:])
                        bound_helpers = worker_bind_helpers(worker_containers_before)
                    if name+"-db" in containers:
                        operation_ids.update(output_lines("docker", "exec", name+"-db", "psql", "-U", "postgres", "-d", "cove", "-Atc", "SELECT id FROM operations"))
                        workloads.update(output_lines("docker", "exec", name+"-db", "psql", "-U", "postgres", "-d", "cove", "-Atc", "SELECT id FROM workloads UNION SELECT subject FROM audit_events WHERE action='DELETE /api/workloads/{id}' AND status=200"))
                        shared_networks.update(output_lines("docker", "exec", name+"-db", "psql", "-U", "postgres", "-d", "cove", "-Atc", "SELECT id FROM shared_networks"))
                finally:
                    cleanup_fixture_resources(workloads, operation_ids, shared_networks, links_owned, bound_helpers)
                    resources_cleaned = True
            finally:
                for container in reversed(containers):
                    subprocess.run(["docker", "rm", "-f", "-v", container], check=False, stdout=subprocess.DEVNULL)
                if links_owned:
                    for image in set(output_lines("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-link:*")) - link_images_before:
                        subprocess.run(["docker", "image", "rm", image], check=False, stdout=subprocess.DEVNULL)
                for resource in reversed(ingress):
                    subprocess.run(["docker", resource, "rm", "-f", "cove-ingress"] if resource == "container" else ["docker", resource, "rm", "cove-ingress"], check=False, stdout=subprocess.DEVNULL)
                if worker_state_owned and resources_cleaned:
                    run("docker", "run", "--rm", "--entrypoint", "sh", "-v", "/var/lib/cove:/fixture",
                        worker_image, "-ec",
                        'test "$(cat /fixture/.browser-owner)" = "$1" || exit 1; find /fixture -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +', "sh", name)
                if network_created:
                    subprocess.run(["docker", "network", "rm", name], check=False, stdout=subprocess.DEVNULL)
                for image in (worker_image, panel_image):
                    subprocess.run(["docker", "image", "rm", image], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                if ingress:
                    assert_ingress_removed()


if __name__ == "__main__":
    with browser_fixture_lock():
        main()
