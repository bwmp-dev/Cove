"""Run browser acceptance against disposable packaged Cove and PostgreSQL containers."""
import base64
from contextlib import contextmanager
import errno
import hashlib
import importlib.util
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
REGISTRY_HOOK_PIN = "registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
REGISTRY_HOOK_CONFIG = "sha256:26b2eb03618e749084668eaff68cff8f81dda12d06ac641be7a6398b82a6f25b"


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



def worker_fixture_state(image, fixture):
    script = ('test "$(cat /fixture/.browser-owner)" = "$1" || exit 1; '
              'printf "OWNER "; if test -f /fixture/build-owner.json; then cat /fixture/build-owner.json; else printf null; fi; printf "\\n"; '
              'printf "BACKUP_OWNER "; if test -f /fixture/backup-helper-owner.json; then cat /fixture/backup-helper-owner.json; else printf null; fi; printf "\\n"; '
              'for p in /fixture/workloads/*; do test ! -d "$p" || printf "WORKLOAD %s\\n" "${p##*/}"; done; '
              'for p in /fixture/operations/*.json; do test ! -f "$p" || { n="${p##*/}"; printf "OPERATION %s\\n" "${n%.json}"; }; done; '
              'test ! -d /fixture/links/identity || printf "LINKS present\\n"')
    result = run("docker", "run", "--rm", "--read-only", "--cap-drop=ALL", "--network=none", "--user=cove",
                 "--mount", "type=bind,source=/var/lib/cove,target=/fixture,readonly", "--entrypoint=sh", image,
                 "-ec", script, "sh", fixture, capture_output=True, text=True)
    state = {"owner": None, "backup_owner": None, "workloads": set(), "operations": set(), "shared_networks": set(), "links": False}
    for line in result.stdout.splitlines():
        kind, value = line.split(" ", 1)
        if kind == "OWNER":
            state["owner"] = json.loads(value)
            if isinstance(state["owner"], dict):
                if set(state["owner"]) != {"cove_build_cache_protocol", "owner"} or state["owner"]["cove_build_cache_protocol"] != 1:
                    raise RuntimeError("Unknown fixture worker owner protocol")
                state["owner"] = state["owner"]["owner"]
            if state["owner"] is not None and (not isinstance(state["owner"], str) or not re.fullmatch(r"[a-f0-9]{32}", state["owner"])):
                raise RuntimeError("Invalid fixture worker owner")
        elif kind == "BACKUP_OWNER":
            owner = json.loads(value)
            state["backup_owner"] = owner.get("ID") if isinstance(owner, dict) else owner
            if state["backup_owner"] is not None and not re.fullmatch(r"[a-f0-9]{32}", state["backup_owner"]):
                raise RuntimeError("Invalid fixture backup owner")
        elif kind in ("WORKLOAD", "OPERATION"):
            if not re.fullmatch(r"[a-zA-Z0-9_-]{1,64}", value):
                raise RuntimeError("Invalid fixture worker resource ID")
            state["workloads" if kind == "WORKLOAD" else "operations"].add(value)
        elif kind == "LINKS":
            state["links"] = True
        else:
            raise RuntimeError("Unexpected fixture ownership metadata")
    return state


def inspect_cleanup_resource(kind, resource):
    if kind == "container":
        template = '{"ID":{{json .Id}},"Name":{{json .Name}},"Labels":{{json .Config.Labels}},"Mounts":{{json .Mounts}},"Networks":{{json .NetworkSettings.Networks}}}'
    else:
        template = '{"ID":{{json .Id}},"Name":{{json .Name}},"Labels":{{json .Labels}}}' if kind == "network" else '{"Name":{{json .Name}},"Labels":{{json .Labels}}}'
    result = subprocess.run(["docker", kind, "inspect", "--format", template, resource], capture_output=True, text=True)
    if result.returncode:
        if "no such" in result.stderr.lower() or "not found" in result.stderr.lower():
            return None
        raise RuntimeError("Could not inspect fixture cleanup resource: " + kind + " " + resource)
    return json.loads(result.stdout)


def owns_worker_resource(kind, resource, fixture, state, root_owned):
    labels = resource.get("Labels") or {}
    owners = [value for key, value in labels.items() if key.startswith(("cove.", "dev.cove.")) and (key.endswith("owner") or key == "cove.browser-fixture")]
    identities = {fixture}
    if state["owner"]:
        identities.add(state["owner"])
    if state.get("backup_owner"):
        identities.add(state["backup_owner"])
    if owners:
        return all(value in identities for value in owners)
    if root_owned and state["owner"]:
        builder = "buildx_buildkit_cove-" + state["owner"] + "0"
        if resource["Name"].lstrip("/") == (builder if kind == "container" else builder + "_state"):
            return kind in ("container", "volume")
    if labels.get("com.docker.compose.project") in {"cove-" + workload for workload in state["workloads"]}:
        return True
    if labels.get("cove.shared-network") in state.get("shared_networks", set()):
        return True
    if labels.get("cove.database-tool.workload") in state["workloads"]:
        return root_owned and labels.get("cove.database-tool.root") == hashlib.sha256(b"/var/lib/cove").hexdigest()
    if any(value in state["workloads"] for key, value in labels.items() if key.startswith(("cove.", "dev.cove.")) and (key.endswith("-workload") or key in ("cove.workload", "dev.cove.workload", "cove.backup", "dev.cove.restore", "dev.cove.environment-preview"))):
        return True
    if any(labels.get(key) in state["operations"] for key in ("dev.cove.operation", "dev.cove.hook", "dev.cove.drill", "dev.cove.candidate")):
        return True
    if kind == "container" and any(mount.get("Type") == "volume" and mount.get("Name") in state.get("volumes", set()) for mount in resource.get("Mounts", [])):
        return bool(labels.get("cove.managed")) or re.fullmatch(r"cove-files-[a-f0-9]{24}", resource["Name"].lstrip("/")) is not None
    if root_owned and kind == "container" and resource.get("ID") in state.get("bound", set()):
        return True
    if not labels.get("cove.managed"):
        return False
    if root_owned and kind == "container" and any(mount.get("Type") == "bind" and (mount.get("Source") == "/var/lib/cove" or mount.get("Source", "").startswith("/var/lib/cove/")) for mount in resource.get("Mounts", [])):
        return True
    # Legacy link mesh networks have no owner label, but their gateway configuration lives in the exclusively owned worker root.
    return root_owned and state["links"] and kind == "network" and resource["Name"] == "cove-link-mesh" and labels.get("cove.managed") == "link-mesh"


def cleanup_worker_resources(fixture, state, root_owned, kinds=("container", "network", "volume")):
    listings = {"container": ["ps", "-aq", "--no-trunc"], "network": ["network", "ls", "-q", "--no-trunc"], "volume": ["volume", "ls", "-q"]}
    resources = {}
    for kind, listing in reversed(list(listings.items())):
        resources[kind] = []
        candidates = set(output_lines("docker", *listing)) - state.get("previous", {}).get(kind, set())
        for resource in candidates - state.get("previous", {}).get(kind, set()):
            current = inspect_cleanup_resource(kind, resource)
            if current and root_owned and kind == "container":
                if current["Name"] == "/" + fixture + "-worker":
                    continue
                if any(mount.get("Type") == "bind" and (mount.get("Source") == "/var/lib/cove" or mount.get("Source", "").startswith("/var/lib/cove/")) for mount in current.get("Mounts", [])):
                    state.setdefault("bound", set()).add(resource)
            if current and owns_worker_resource(kind, current, fixture, state, root_owned):
                resources[kind].append((resource, current))
        if kind == "volume":
            state["volumes"] = {current["Name"] for _, current in resources[kind]}
    failures = []
    for kind in kinds:
        for resource, before in resources[kind]:
            current = inspect_cleanup_resource(kind, resource)
            if current is None:
                continue
            if current != before or not owns_worker_resource(kind, current, fixture, state, root_owned):
                # Running state/attachments may change after containers stop; immutable ID and labels must still match.
                if current.get("ID", current["Name"]) != before.get("ID", before["Name"]) or current.get("Labels") != before.get("Labels"):
                    failures.append("ownership changed: " + kind + " " + resource)
                    continue
            args = ["docker", "rm", "-f", "-v", resource] if kind == "container" else ["docker", kind, "rm", resource]
            result = subprocess.run(args, capture_output=True, text=True)
            if result.returncode and inspect_cleanup_resource(kind, resource) is not None:
                failures.append("could not remove " + kind + " " + resource)
    if failures:
        raise RuntimeError("Fixture worker cleanup failed: " + "; ".join(failures))


def main():
    certificate_dns = "--certificate-dns" in sys.argv[1:]
    cloudflare_tunnel = "--cloudflare-tunnel" in sys.argv[1:]
    registry_hooks = "--registry-webhooks" in sys.argv[1:]
    image_scans = "--image-scans" in sys.argv[1:]
    managed_registry = "--managed-registry" in sys.argv[1:]
    build_nodes = "--build-nodes" in sys.argv[1:]
    if build_nodes and not managed_registry:
        raise ValueError("Build-node acceptance requires the managed-registry TLS fixture")
    if certificate_dns and cloudflare_tunnel:
        raise ValueError("Choose one isolated certificate or Tunnel provider fixture")
    browser_args = [value for value in sys.argv[1:] if value not in ("--certificate-dns", "--cloudflare-tunnel", "--registry-webhooks", "--image-scans", "--managed-registry", "--build-nodes")]
    name = "cove-browser-" + secrets.token_hex(6)
    panel_image = name + "-panel:development"
    worker_image = name + "-worker:development"
    base_path = os.environ.get("COVE_BROWSER_BASE_PATH", "")
    if not re.fullmatch(r"(?:/[A-Za-z0-9_-]+)*", base_path) or len(base_path) > 200:
        raise ValueError("Use a simple COVE_BROWSER_BASE_PATH without a trailing slash")
    containers = []
    identity_images = []
    identity = None
    network_created = False
    ingress = []
    worker_state_owned = False
    fixture_workloads = []
    baseline = {kind: set(output_lines("docker", *listing)) for kind, listing in (
        ("container", ["ps", "-aq", "--no-trunc"]), ("network", ["network", "ls", "-q", "--no-trunc"]), ("volume", ["volume", "ls", "-q"]))}
    certificate_config = None
    managed_registry_config = None
    build_nodes_config = None
    extra_images = []
    tunnel_bridges = []
    env = os.environ.copy()
    if registry_hooks or image_scans:
        registry_image_id = run("docker", "image", "inspect", "--format", "{{.Id}}", "registry:2.8.3", capture_output=True, text=True).stdout.strip()
        # Docker Desktop reports the manifest ID; docker save/load on classic engines reports the config ID.
        if registry_image_id not in (REGISTRY_HOOK_PIN.split("@", 1)[1], REGISTRY_HOOK_CONFIG):
            raise RuntimeError("Cached registry fixture does not match the pinned manifest/config; refusing execution")
        env.update(COVE_BROWSER_REGISTRY_IMAGE=registry_image_id, COVE_BROWSER_REGISTRY_PIN=REGISTRY_HOOK_PIN)
        if registry_hooks:
            env["COVE_BROWSER_REGISTRY_HOOKS"] = "1"
    if image_scans:
        from scanner_fixture import SCANNER_PIN
        digest = SCANNER_PIN.split("@", 1)[1]
        if subprocess.run(["docker", "image", "inspect", SCANNER_PIN], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode != 0:
            scanner_image_id = run("docker", "image", "inspect", "--format", "{{.Id}}", digest, capture_output=True, text=True).stdout.strip()
            if scanner_image_id != digest:
                raise RuntimeError("The cached scanner must match the exact release digest")
        if not env.get("COVE_BROWSER_SCANNER_DATABASE"):
            raise RuntimeError("COVE_BROWSER_SCANNER_DATABASE must name the cached main database archive")
        env["COVE_BROWSER_IMAGE_SCANS"] = "1"
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
            "-addext", "subjectAltName=DNS:localhost,DNS:pebble,DNS:api.cloudflare.com" if certificate_dns or cloudflare_tunnel else "subjectAltName=DNS:localhost", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        (fixture / "routes.yaml").write_text(json.dumps({
            "http":{"routers":{"panel":{"rule":"Host(`localhost`)","entryPoints":["websecure"],"service":"panel","tls":{}}},
                    "services":{"panel":{"loadBalancer":{"servers":[{"url":"http://panel:8080"}]}}}},
            "tls":{"certificates":[{"certFile":"/fixture/cert.pem","keyFile":"/fixture/key.pem"}]}}))
        if managed_registry:
            from managed_registry_fixture import ManagedRegistryFixture, REGISTRY_PIN
            run("docker", "image", "inspect", REGISTRY_PIN, stdout=subprocess.DEVNULL)
            managed_registry_config = ManagedRegistryFixture(run, fixture, name, openssl, env)
        try:
            # Claim the fixed ingress name before building so parallel tracks cannot overlap.
            while True:
                occupied = any(subprocess.run(["docker", kind, "inspect", "cove-ingress"], capture_output=True).returncode == 0 for kind in ("container", "network", "volume"))
                if not occupied:
                    claim = subprocess.run(["docker", "network", "create", "--label", "cove.managed=ingress", "--label", "cove.browser-fixture="+name, "cove-ingress"], capture_output=True)
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
            run("docker", "network", "create", *(["--internal"] if cloudflare_tunnel else []), name, stdout=subprocess.DEVNULL)
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
            if cloudflare_tunnel:
                from tunnel_fixture import start_tunnel_fixture, TunnelFixtureBridge
                extra_images.append(name + "-tunnel-provider:test")
                certificate_config = start_tunnel_fixture(run, name, fixture, containers, tunnel_bridges)
            elif certificate_dns:
                from certificate_fixture import start_certificate_fixture
                extra_images.append(name + "-certificate-provider:test")
                certificate_config = start_certificate_fixture(run, name, fixture, containers)
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            origin = f"https://localhost:{port}"
            panel_url = origin + base_path
            env.update(DATABASE_URL=f"postgres://postgres:{env['POSTGRES_PASSWORD']}@db:5432/cove?sslmode=disable", COVE_PUBLIC_URL=panel_url)
            identity_options = []
            if "e2e/directory.spec.ts" in sys.argv[1:] or sys.argv[1:] == ["--identity"]:
                identity_spec = importlib.util.spec_from_file_location("cove_browser_identity", ROOT / "scripts/browser-identity.py")
                identity = importlib.util.module_from_spec(identity_spec)
                identity_spec.loader.exec_module(identity)
                identity_options = identity.start(name, fixture, env, containers, identity_images, openssl)
            options = [item for key in ("DATABASE_URL", "COVE_PUBLIC_URL", "COVE_ADMIN_PASSWORD", "COVE_ADMIN_TOKEN", "COVE_ENCRYPTION_KEY") for item in ("-e", key)]
            if certificate_config:
                env.update(SSL_CERT_FILE="/fixture-trust.pem")
                options += ["--mount", f"type=bind,source={fixture / 'cert.pem'},target=/fixture-trust.pem,readonly", "-e", "SSL_CERT_FILE"]
                options += ["-e", "HTTP_PROXY=", "-e", "HTTPS_PROXY=", "-e", "ALL_PROXY=", "-e", "NO_PROXY=*"]
                if certificate_dns:
                    env.update(COVE_ACME_DIRECTORY="https://pebble:14000/dir", COVE_ACME_ROOT="/fixture-trust.pem")
                    options += ["--dns", certificate_config["dns"]]
                    options += [item for key in ("COVE_ACME_DIRECTORY", "COVE_ACME_ROOT") for item in ("-e", key)]
            run("docker", "run", "-d", "--name", name + "-panel", "--network", name,
                "--network-alias", "panel", *options, *identity_options, panel_image, env=env, stdout=subprocess.DEVNULL)
            containers.append(name + "-panel")
            run("docker", "run", "-d", "--name", name + "-tls", "--network", name,
                "--label", "cove.browser-fixture="+name,
                *([] if cloudflare_tunnel else ["-p", f"127.0.0.1:{port}:443"]), "--mount", f"type=bind,source={fixture},target=/fixture,readonly",
                "traefik:v3.7.19", "--entrypoints.websecure.address=:443", "--providers.file.filename=/fixture/routes.yaml", stdout=subprocess.DEVNULL)
            containers.append(name + "-tls")
            if cloudflare_tunnel:
                tunnel_bridges.append(TunnelFixtureBridge(name, "tls", port))
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
            if "certificates:write" not in [item["name"] for item in api("GET", "/api/access-tokens/scopes")]:
                raise RuntimeError("Packaged panel lacks this merge's scoped certificate contract")
            if "directory:manage" not in [item["name"] for item in api("GET", "/api/access-tokens/scopes")]:
                raise RuntimeError("Packaged panel lacks this merge's directory scope and ownership contract")
            if "nodes:manage" not in [item["name"] for item in api("GET", "/api/access-tokens/scopes")]:
                raise RuntimeError("Packaged panel lacks this merge's instance-admin listener contract")
            events = api("GET", "/api/notifications/events")
            if "certificate.failed" not in events:
                raise RuntimeError("Packaged panel lacks this merge's certificate outcome contract")
            if not isinstance(search.get("items"), list) or search.get("limit") != 40 or timezone.get("schedule_timezone") != "UTC" or "read_secrets" not in roles.get("permission_catalogue", []):
                raise RuntimeError("Packaged panel does not expose the merged integration/trust contract")
            if "security.lockout" not in events or "adoption.succeeded" not in events:
                raise RuntimeError("Packaged panel does not expose the merged operations outcome catalogue")
            if "rollout.staged" not in events or "autoscale.queued" not in events:
                raise RuntimeError("Packaged panel does not expose the merged deployment lifecycle contract")
            if identity is not None:
                identity.seed(name + "-db", env)
            run("go", "build", "-o", "bin/cove-worker-linux-amd64", "./cmd/cove-worker", env=env | {"GOOS": "linux", "GOARCH": "amd64"})
            run("docker", "build", "-f", "test/worker/Dockerfile", "-t", worker_image, ".", stdout=subprocess.DEVNULL)
            for resource in ("container", "volume"):
                if subprocess.run(["docker", resource, "inspect", "cove-ingress"], capture_output=True).returncode == 0:
                    raise RuntimeError("Refusing to replace an existing cove-ingress " + resource)
            # Private connection tests create the worker's link gateway and networks on this Docker host.
            if subprocess.run(["docker", "container", "inspect", "cove-link"], capture_output=True).returncode == 0 or output_lines("docker", "network", "ls", "-q", "--filter", "label=cove.managed=link") or output_lines("docker", "network", "ls", "-q", "--filter", "label=cove.managed=link-mesh"):
                raise RuntimeError("Refusing to run with an existing Cove private link gateway or networks on this Docker host")
            # The worker adopts this Cove-labelled proxy for route tests. It publishes no host ports, and
            # its ACME resolver points at a closed loopback port so no public certificate authority is contacted.
            for resource in ("volume",):
                run("docker", resource, "create", "--label", "cove.managed=ingress", "--label", "cove.browser-fixture="+name, "cove-ingress", stdout=subprocess.DEVNULL)
                ingress.append(resource)
            run("docker", "create", "--name", "cove-ingress", "--label", "cove.managed=ingress", "--label", "cove.browser-fixture="+name, "--network", "cove-ingress",
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
            if image_scans:
                from scanner_fixture import seed_scanner_database
                seed_scanner_database(run, worker_image, name, env["COVE_BROWSER_SCANNER_DATABASE"])
            worker = name + "-worker"
            if cloudflare_tunnel:
                shim = fixture / "docker-shim"
                run("go", "build", "-o", str(shim), "./test/tunnels/docker-shim", env=env | {"GOOS":"linux", "GOARCH":"amd64", "CGO_ENABLED":"0"})
                run("docker", "create", "--name", worker, "--network", name, "--network-alias", "worker", "-p", "127.0.0.1::22",
                    "-e", "COVE_TUNNEL_FIXTURE_OWNER="+name, "-e", "COVE_TUNNEL_FIXTURE_IMAGE="+certificate_config["image"],
                    "-e", "COVE_TUNNEL_FIXTURE_PROVIDER="+certificate_config["address"],
                    "-v", "/var/lib/cove:/var/lib/cove", "-v", "/var/run/docker.sock:/var/run/docker.sock", worker_image, stdout=subprocess.DEVNULL)
                containers.append(worker)
                run("docker", "cp", str(shim), worker+":/tmp/tunnel-docker-shim")
                run("docker", "cp", str(fixture / "cert.pem"), worker+":/var/lib/cove/tunnel-fixture-ca.pem")
                run("docker", "start", worker, stdout=subprocess.DEVNULL)
            else:
                run("docker", "run", "-d", "--name", worker, "--network", name, "--network-alias", "worker",
                    "-p", "127.0.0.1::22", *(["-e", "COVE_IMAGE_SCANNING=1"] if image_scans else []),
                    "-v", "/var/lib/cove:/var/lib/cove", "-v", "/var/run/docker.sock:/var/run/docker.sock", worker_image, stdout=subprocess.DEVNULL)
            if worker not in containers:
                containers.append(worker)
            if managed_registry_config:
                managed_registry_config.install(worker_image, worker)

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
            if sys.argv[1:] == ["--managed-data"] or "e2e/databases.spec.ts" in sys.argv[1:] or identity is not None:
                ready = next(item for item in api("GET", "/api/nodes") if item["id"] == enrollment["id"])
                if not ready.get("capabilities", {}).get("managed_databases") or len(api("GET", "/api/database-engines")["engines"]) != 7:
                    raise RuntimeError("Managed-data acceptance requires this branch's panel and worker capabilities")
            if identity is not None:
                methods = api("GET", "/auth/sign-in-methods")
                if "ldap_providers" not in methods or not api("GET", "/api/settings/directory/teams").get("teams"):
                    raise RuntimeError("Directory acceptance requires this branch's identity APIs")
            if build_nodes:
                from build_nodes_fixture import BuildNodesFixture
                build_nodes_config = BuildNodesFixture(run, name, fixture, env, containers)
                build_nodes_config.start(worker_image, api, managed_registry_config.hostname)
            client_binary=fixture / ("cove.exe" if os.name == "nt" else "cove")
            run("go", "build", "-o", str(client_binary), "./cmd/cove")
            env["COVE_BROWSER_LOCK_OWNER"] = str(os.getpid())
            env.update(COVE_BROWSER_DATABASE=name+"-db", COVE_BROWSER_WORKER=worker, COVE_BROWSER_CLIENT=str(client_binary), COVE_BROWSER_URL=origin, COVE_BROWSER_PASSWORD=env["COVE_ADMIN_PASSWORD"])
            if cloudflare_tunnel:
                env.pop("COVE_BROWSER_SSH_PORT", None)
                env.update(COVE_BROWSER_TUNNEL_PROVIDER=certificate_config["provider"], COVE_BROWSER_TUNNEL_TOKEN=certificate_config["token"], COVE_BROWSER_TUNNEL_ROOT=str(fixture / "cert.pem"))
            else:
                env["COVE_BROWSER_SSH_PORT"]=run("docker", "port", worker, "22/tcp",capture_output=True,text=True).stdout.strip().rsplit(":",1)[1]
            if certificate_config and certificate_dns:
                env.update(COVE_BROWSER_CERTIFICATE_PROVIDER=certificate_config["provider"], COVE_BROWSER_CERTIFICATE_TOKEN=certificate_config["token"],
                           COVE_BROWSER_CERTIFICATE_ROOT=str(fixture / "cert.pem"), COVE_BROWSER_CERTIFICATE_ISSUANCE_ROOT=certificate_config["management"] + "/roots/0")
            if sys.argv[1:] in (["--managed-data"], ["--identity"]):
                env["COVE_BROWSER_CA"] = str(fixture / "cert.pem")
                env["COVE_BROWSER_LOCK_OWNER"] = str(os.getpid())
                script = "test-identity.py" if identity is not None else "test-managed-data.py"
                subprocess.run([sys.executable, str(ROOT / "scripts" / script)], cwd=ROOT, env=env, check=True)
            else:
                subprocess.run([shutil.which("pnpm"), "test:e2e", *browser_args], cwd=ROOT / "frontend", env=env, check=True)
        except subprocess.CalledProcessError:
            if managed_registry_config and worker_state_owned:
                try:
                    for operation in api("GET", "/api/operations"):
                        if operation["state"] in ("failed", "interrupted", "cancelled"):
                            diagnostic = api("GET", "/api/operations/" + operation["id"] + "/logs")
                            print("Managed registry fixture operation:", operation["id"], operation["state"], diagnostic, flush=True)
                except Exception:
                    print("Managed registry fixture operation diagnostics unavailable", flush=True)
            raise
        finally:
            cleanup_errors = []
            for bridge in reversed(tunnel_bridges):
                try:
                    bridge.stop()
                except Exception as error:
                    cleanup_errors.append("could not stop the fixture tunnel bridge: " + str(error))
            state = {"owner": None, "backup_owner": None, "workloads": set(), "operations": set(), "shared_networks": set(), "links": False}
            root_proved = False
            try:
                if name+"-panel" in containers:
                    run("docker", "stop", "--time", "10", name+"-panel", stdout=subprocess.DEVNULL)
                if name+"-worker" in containers:
                    run("docker", "stop", "--time", "10", name+"-worker", stdout=subprocess.DEVNULL)
                if worker_state_owned:
                    state = worker_fixture_state(worker_image, name)
                    state["previous"] = baseline
                    root_proved = True
                if root_proved and name+"-db" in containers:
                    for key, query in (
                        ("operations", "SELECT id FROM operations"),
                        ("workloads", "SELECT id FROM workloads UNION SELECT subject FROM audit_events WHERE action='DELETE /api/workloads/{id}' AND status=200"),
                        ("shared_networks", "SELECT id FROM shared_networks")):
                        values = output_lines("docker", "exec", name+"-db", "psql", "-U", "postgres", "-d", "cove", "-Atc", query)
                        if any(not re.fullmatch(r"[0-9a-f]{32}", value) for value in values):
                            raise RuntimeError("Unexpected fixture " + key + " identity")
                        state[key].update(values)
                try:
                    cleanup_worker_resources(name, state, root_proved, kinds=("container",))
                except Exception as error:
                    cleanup_errors.append(str(error))
            finally:
                if build_nodes_config:
                    try:
                        build_nodes_config.cleanup()
                    except Exception as error:
                        cleanup_errors.append("build worker fixture cleanup failed: " + str(error))
                if managed_registry_config:
                    try:
                        managed_registry_config.cleanup(worker_image)
                    except Exception as error:
                        cleanup_errors.append("managed registry fixture trust cleanup failed: " + str(error))
                for container in reversed(containers):
                    subprocess.run(["docker", "rm", "-f", "-v", container], check=False, stdout=subprocess.DEVNULL)
                try:
                    cleanup_worker_resources(name, state, root_proved)
                except Exception as error:
                    cleanup_errors.append(str(error))
                for resource in reversed(ingress):
                    current = inspect_cleanup_resource(resource, "cove-ingress")
                    if current is None:
                        continue
                    if (current.get("Labels") or {}).get("cove.browser-fixture") != name:
                        cleanup_errors.append("refusing to remove another fixture's cove-ingress " + resource)
                        continue
                    result = subprocess.run(["docker", resource, "rm", "-f", "cove-ingress"] if resource == "container" else ["docker", resource, "rm", "cove-ingress"], capture_output=True, text=True)
                    if result.returncode:
                        cleanup_errors.append("could not remove cove-ingress " + resource)
                if ingress:
                    remaining = [kind for kind in ("container", "network", "volume") if inspect_cleanup_resource(kind, "cove-ingress") is not None]
                    if remaining:
                        cleanup_errors.append("cove-ingress remains after fixture cleanup: " + ", ".join(remaining))
                if worker_state_owned and root_proved and not cleanup_errors:
                    run("docker", "run", "--rm", "--entrypoint", "sh", "-v", "/var/lib/cove:/fixture",
                        worker_image, "-ec",
                        'test "$(cat /fixture/.browser-owner)" = "$1" || exit 1; find /fixture -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +', "sh", name)
                for workload in state["workloads"]:
                    for tag in output_lines("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-release/"+workload+":*"):
                        subprocess.run(["docker", "image", "rm", tag], check=True, stdout=subprocess.DEVNULL)
                if network_created:
                    result = subprocess.run(["docker", "network", "rm", name], capture_output=True, text=True)
                    if result.returncode and inspect_cleanup_resource("network", name) is not None:
                        cleanup_errors.append("could not remove fixture network " + name)
                for image in (*identity_images, worker_image, panel_image, *extra_images):
                    subprocess.run(["docker", "image", "rm", image], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                if cleanup_errors:
                    raise RuntimeError("; ".join(cleanup_errors))


if __name__ == "__main__":
    with browser_fixture_lock():
        main()
