"""Private connection acceptance on two disposable Docker-in-Docker worker nodes.

Builds the panel and worker, runs PostgreSQL, the panel and two privileged worker nodes (each with its
own Docker daemon, SSH and Cove worker) on an isolated network, enrolls both nodes through the API and
checks allowed and denied traffic between real containers. Everything it creates is removed afterwards.
"""
import base64
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parent.parent
PASSED = []


def run(*args, **kwargs):
    kwargs.setdefault("check", True)
    return subprocess.run(args, cwd=ROOT, **kwargs)


def output(*args):
    return run(*args, capture_output=True, text=True).stdout.strip()


def check(label, condition, detail=""):
    if not condition:
        raise AssertionError(f"FAILED: {label} {detail}")
    PASSED.append(label)
    print("PASS", label, flush=True)


def wait(label, predicate, timeout=120, interval=2):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            last = predicate()
            if last:
                return last
        except (AssertionError, urllib.error.URLError, OSError, KeyError, ValueError) as error:
            last = error
        time.sleep(interval)
    raise AssertionError(f"Timed out waiting for {label}: {last}")


def main():
    name = "cove-links-" + secrets.token_hex(5)
    containers, network, node_image = [], False, name + "-node:test"
    token = secrets.token_hex(32)
    env = os.environ | {"POSTGRES_PASSWORD": secrets.token_hex(16), "COVE_ADMIN_TOKEN": token,
                        "COVE_ADMIN_PASSWORD": secrets.token_hex(16), "COVE_ENCRYPTION_KEY": base64.b64encode(secrets.token_bytes(32)).decode()}
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        fixture = Path(directory)
        binaries = fixture / "bin"
        binaries.mkdir()
        try:
            for arch in ("amd64", "arm64"):
                run("go", "build", "-trimpath", "-o", str(binaries / f"cove-worker-linux-{arch}"), "./cmd/cove-worker", env=env | {"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"})
            run("go", "build", "-trimpath", "-o", str(binaries / "cove"), "./cmd/cove", env=env | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"})
            context = fixture / "node"
            context.mkdir()
            shutil.copy(binaries / "cove-worker-linux-amd64", context / "cove-worker")
            shutil.copy(ROOT / "test/links/entrypoint.sh", context / "entrypoint.sh")
            (context / "entrypoint.sh").write_bytes((context / "entrypoint.sh").read_bytes().replace(b"\r\n", b"\n"))
            run("docker", "build", "-q", "-f", str(ROOT / "test/links/Dockerfile"), "-t", node_image, str(context), stdout=subprocess.DEVNULL)
            run("docker", "network", "create", name, stdout=subprocess.DEVNULL)
            network = True
            db = name + "-db"
            run("docker", "run", "-d", "--name", db, "--network", name, "--network-alias", "db", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB=cove", "postgres:17-alpine", env=env, stdout=subprocess.DEVNULL)
            containers.append(db)
            wait("database", lambda: run("docker", "exec", db, "pg_isready", "-h", "127.0.0.1", "-U", "postgres", check=False, capture_output=True).returncode == 0, 60, 1)
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            panel = name + "-panel"
            panel_env = env | {"DATABASE_URL": f"postgres://postgres:{env['POSTGRES_PASSWORD']}@db:5432/cove?sslmode=disable", "COVE_PUBLIC_URL": "https://cove.invalid", "COVE_LISTEN": "0.0.0.0:8080", "COVE_BINARY_DIR": "/app/bin"}
            options = [item for key in ("DATABASE_URL", "COVE_PUBLIC_URL", "COVE_LISTEN", "COVE_BINARY_DIR", "COVE_ADMIN_TOKEN", "COVE_ADMIN_PASSWORD", "COVE_ENCRYPTION_KEY") for item in ("-e", key)]
            run("docker", "run", "-d", "--name", panel, "--network", name, "-p", f"127.0.0.1:{port}:8080", "--mount", f"type=bind,source={binaries},target=/app/bin,readonly", *options, "alpine:3.23", "/app/bin/cove", env=panel_env, stdout=subprocess.DEVNULL)
            containers.append(panel)
            origin = f"http://127.0.0.1:{port}"

            def api(method, path, body=None, credential=None, raw=False, status=None):
                request = urllib.request.Request(origin + path, method=method, data=json.dumps(body).encode() if body is not None else None,
                                                 headers={"Authorization": "Bearer " + (credential or token), "Content-Type": "application/json"})
                try:
                    with urllib.request.urlopen(request, timeout=300) as response:
                        data = response.read().decode()
                        if status is not None and response.status != status:
                            raise AssertionError(f"{method} {path}: {response.status}")
                        return data if raw else json.loads(data)
                except urllib.error.HTTPError as error:
                    if status == error.code:
                        return error.read().decode()
                    raise AssertionError(f"{method} {path}: {error.code} {error.read().decode()}")

            wait("panel", lambda: api("GET", "/healthz"), 60, 1)
            run("docker", "save", "-o", str(fixture / "busybox.tar"), "busybox:1.37")
            nodes = {}
            for alias in ("node1", "node2"):
                container = f"{name}-{alias}"
                run("docker", "run", "-d", "--privileged", "--name", container, "--network", name, "--network-alias", alias, node_image, stdout=subprocess.DEVNULL)
                containers.append(container)
                wait(alias + " worker socket", lambda: run("docker", "exec", container, "test", "-S", "/run/cove/worker.sock", check=False).returncode == 0, 120, 1)
                with open(fixture / "busybox.tar", "rb") as image:
                    run("docker", "exec", "-i", container, "docker", "load", stdin=image, stdout=subprocess.DEVNULL)
                enrollment = api("POST", "/api/nodes", {"name": alias, "address": alias, "port": 22})
                credential = re.search(r"Authorization: Bearer ([^']+)", enrollment["setup_command"]).group(1)
                installer = api("GET", "/install.sh", credential=credential, raw=True)
                authorized = re.search(r"'(restrict,command=\"[^\"]+\" ssh-ed25519 [A-Za-z0-9+/=]+)'", installer).group(1)
                run("docker", "exec", "-i", container, "sh", "-c", "cat > /var/lib/cove/.ssh/authorized_keys && chown cove:cove /var/lib/cove/.ssh/authorized_keys && chmod 600 /var/lib/cove/.ssh/authorized_keys", input=authorized + "\n", text=True)
                host_key = output("docker", "exec", container, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
                api("POST", "/bootstrap/register", {"host_key": host_key}, credential=credential)
                node_id = enrollment["id"]
                ready = lambda: next(n for n in api("GET", "/api/nodes") if n["id"] == node_id)
                wait(alias + " ready", lambda: ready()["state"] == "ready" or api("POST", f"/api/nodes/{node_id}/verify") and False, 60, 2)
                check(alias + " reports private link support", json.loads(json.dumps(ready()["capabilities"])).get("private_links") is True)
                # Nested containers cannot resolve the fixture network's names, so peers use the node's IP address.
                address = output("docker", "inspect", "--format", "{{(index .NetworkSettings.Networks \"" + name + "\").IPAddress}}", container)
                settings = api("PUT", f"/api/nodes/{node_id}/private-links", {"link_address": address, "link_port": 47800})
                check(alias + " link address saved", settings["link_address"] == address)
                nodes[alias] = {"id": node_id, "container": container}

            project = api("POST", "/api/projects", {"name": "Links"})["id"]
            production = api("POST", f"/api/projects/{project}/environments", {"name": "production"})["id"]
            staging = api("POST", f"/api/projects/{project}/environments", {"name": "staging"})["id"]
            placements = {"A": ("node1", production), "DB": ("node2", production), "D": ("node2", production), "B": ("node1", staging), "C": ("node1", staging), "E": ("node1", staging)}
            resources = {}

            def compose(label, ports=""):
                command = f"mkdir -p /www && echo {label} > /www/index.html && httpd -p 9090 -h /www && (nc -lk -p 7000 -e cat &) && exec httpd -f -p 8080 -h /www"
                return "services:\n  app:\n    image: busybox:1.37\n    command: [\"sh\", \"-c\", " + json.dumps(command) + "]\n" + ports

            def operate(resource, action, body=None):
                operation = api("POST", f"/api/workloads/{resources[resource]['id']}/operations", {"action": action, **(body or {})})["id"]
                result = wait(f"{resource} {action}", lambda: (lambda o: o if o["state"] in ("succeeded", "failed", "interrupted", "cancelled") else None)(api("GET", f"/api/operations/{operation}")), 300, 2)
                if result["state"] != "succeeded":
                    logs = api("GET", f"/api/operations/{operation}/logs")
                    raise AssertionError(f"{resource} {action} {result['state']}: {logs.get('logs', '')[-2000:]}")
                return operation

            for label, (alias, environment) in placements.items():
                ports = "    ports: [\"18080:8080\"]\n" if label == "C" else ""
                workload = api("POST", "/api/workloads", {"name": label, "node_id": nodes[alias]["id"], "compose": compose(label, ports), "environment": {}, "project_id": project, "environment_id": environment, "kind": "service"})["id"]
                resources[label] = {"id": workload, "node": nodes[alias]["container"], "container": f"cove-{workload}-app-1"}
            releases = {label: operate(label, "deploy") for label in resources}

            def sh(resource, command, timeout=20):
                r = resources[resource]
                return run("docker", "exec", r["node"], "docker", "exec", r["container"], "sh", "-c", command, check=False, capture_output=True, text=True, timeout=timeout)

            def fetch(resource, host, port_number):
                result = sh(resource, f"wget -qO- -T 3 http://{host}:{port_number}/")
                return result.stdout.strip() if result.returncode == 0 else None

            def allowed(label, resource, host, port_number, expected):
                got = wait(label, lambda: fetch(resource, host, port_number) == expected, 60, 2)
                check(label, got)

            def denied(label, resource, host, port_number):
                check(label, fetch(resource, host, port_number) is None)

            def connect(source, target, port_number, alias):
                return api("POST", "/api/private-connections", {"source_id": resources[source]["id"], "source_service": "app", "target_id": resources[target]["id"], "target_service": "app", "port": port_number, "alias": alias})["id"]

            def graph():
                return api("GET", f"/api/projects/{project}/connections")

            def states():
                return {c["id"]: c for c in graph()["connections"]}

            def settled(ids, state="applied"):
                return wait("connections " + state, lambda: all(states().get(i, {}).get("state") == state for i in ids), 180, 2)

            def gone(connection):
                return wait("connection removal", lambda: connection not in states(), 180, 2)

            def ip(resource_node, container, prefix):
                networks = json.loads(output("docker", "exec", resource_node, "docker", "inspect", "--format", "{{json .NetworkSettings.Networks}}", container))
                return [v["IPAddress"] for k, v in networks.items() if k.startswith(prefix)]

            connections = {"A-DB": connect("A", "DB", 8080, "db.internal"), "B-DB": connect("B", "DB", 8080, "db.internal"), "B-C": connect("B", "C", 8080, "c.internal"), "D-C": connect("D", "C", 8080, "c.internal"), "A-DB-live": connect("A", "DB", 7000, "db.internal")}
            settled(connections.values())
            check("all connections reach the applied state", True)
            allowed("A -> DB across nodes", "A", "db.internal", 8080, "DB")
            allowed("B -> DB across nodes and environments", "B", "db.internal", 8080, "DB")
            allowed("B -> C on the same node", "B", "c.internal", 8080, "C")
            allowed("D -> C across nodes", "D", "c.internal", 8080, "C")
            denied("C cannot reach DB", "C", "db.internal", 8080)
            denied("D cannot reach DB", "D", "db.internal", 8080)
            denied("A cannot reach C (no transitive grant through DB or B)", "A", "c.internal", 8080)
            denied("A cannot use an ungranted DB port", "A", "db.internal", 9090)
            gateway2 = ip(nodes["node2"]["container"], "cove-link", "cove-link-t-")
            check("DB's node gateway is on DB's target network", len(gateway2) == 1)
            denied("DB cannot initiate through its gateway (no reverse grant)", "DB", gateway2[0], 8080)
            denied("DB cannot reach the live-test port through its gateway", "DB", gateway2[0], 7000)
            gateway1_c = ip(nodes["node1"]["container"], "cove-link", "cove-link-t-")
            denied("C cannot initiate through its gateway", "C", gateway1_c[0], 8080)
            def networks_of(node, container):
                return json.loads(output("docker", "exec", node, "docker", "inspect", "--format", "{{json .NetworkSettings.Networks}}", container))
            b_default = ip(nodes["node1"]["container"], resources["B"]["container"], "cove-" + resources["B"]["id"])
            check("B has its own Compose network", len(b_default) == 1)
            denied("A cannot reach B on the same node", "A", b_default[0], 8080)
            a_networks = set(networks_of(resources["A"]["node"], resources["A"]["container"]))
            foreign = {k: v["IPAddress"] for k, v in networks_of(nodes["node1"]["container"], "cove-link").items() if k.startswith("cove-link-s-") and k not in a_networks}
            check("node1's gateway serves other sources on separate networks", len(foreign) >= 1, str(foreign))
            for network_name, gateway_address in foreign.items():
                denied("A cannot use another source's endpoint on " + network_name, "A", gateway_address, 8080)
            c_default = ip(nodes["node1"]["container"], resources["C"]["container"], "cove-" + resources["C"]["id"])
            denied("B cannot reach C directly, only through its grant", "B", c_default[0], 9090)
            denied("unlinked E cannot reach DB", "E", "db.internal", 8080)
            e_networks = networks_of(resources["E"]["node"], resources["E"]["container"])
            check("unlinked E keeps only its own Compose network", list(e_networks) == [f"cove-{resources['E']['id']}_default"], str(list(e_networks)))
            published = run("docker", "exec", resources["C"]["node"], "wget", "-qO-", "-T", "3", "http://127.0.0.1:18080/", check=False, capture_output=True, text=True)
            check("C's published host port still works", published.stdout.strip() == "C")

            check("live-test port echoes before revocation", sh("A", "echo hello | nc -w 3 db.internal 7000").stdout.strip() == "hello")
            # Hold a connection open, then revoke: both ends of the proxied connection must close.
            run("docker", "exec", resources["A"]["node"], "docker", "exec", "-d", resources["A"]["container"], "sh", "-c", "sleep 100000 | nc db.internal 7000")
            established = lambda resource: "ESTABLISHED" in sh(resource, "netstat -tn | grep ':7000 ' || true").stdout
            wait("held connection", lambda: established("A") and established("DB"), 30, 1)
            check("a held connection is established through both gateways", True)
            api("DELETE", f"/api/private-connections/{connections['A-DB-live']}")
            gone(connections["A-DB-live"])
            wait("held connection closed at the source", lambda: not established("A"), 30, 1)
            wait("held connection closed at the target", lambda: not established("DB"), 30, 1)
            check("revocation closes the existing connection at both ends", True)
            check("revocation refuses new connections", sh("A", "echo x | nc -w 3 db.internal 7000").stdout.strip() == "")
            allowed("unrelated connection survives revocation", "A", "db.internal", 8080, "DB")

            api("PUT", f"/api/private-connections/{connections['B-C']}", {"source_service": "app", "target_service": "app", "port": 9090, "alias": "c.internal"})
            settled([connections["B-C"]])
            allowed("edited connection uses its new port", "B", "c.internal", 9090, "C")
            denied("edited connection no longer uses its old port", "B", "c.internal", 8080)

            operate("DB", "deploy")
            allowed("A -> DB after redeploying the target", "A", "db.internal", 8080, "DB")
            operate("A", "deploy")
            allowed("A -> DB after redeploying the source", "A", "db.internal", 8080, "DB")
            operate("DB", "rollback", {"release": releases["DB"]})
            allowed("A -> DB after rolling the target back to a release from before the connection", "A", "db.internal", 8080, "DB")

            node1 = nodes["node1"]["container"]
            run("docker", "exec", node1, "docker", "restart", "cove-link", stdout=subprocess.DEVNULL)
            allowed("B -> C after the gateway restarts", "B", "c.internal", 9090, "C")
            run("docker", "exec", node1, "docker", "rm", "-f", "cove-link", stdout=subprocess.DEVNULL)
            allowed("A -> DB after the gateway is recreated by the worker", "A", "db.internal", 8080, "DB")

            def restart_worker(container, stop_only=False):
                run("docker", "exec", container, "pkill", "-f", "cove-worker daemon", check=False)
                wait("worker stopped", lambda: run("docker", "exec", container, "pgrep", "-f", "cove-worker daemon", check=False, capture_output=True).returncode != 0, 30, 1)
                if not stop_only:
                    run("docker", "exec", "-d", container, "su", "-s", "/bin/sh", "cove", "-c", "exec /usr/local/bin/cove-worker daemon >>/var/lib/cove/worker.log 2>&1")

            node2 = nodes["node2"]["container"]
            restart_worker(node2, stop_only=True)
            allowed("links keep working while the worker daemon is down", "A", "db.internal", 8080, "DB")
            created_while_down = connect("D", "DB", 8080, "db.internal")
            time.sleep(8)
            pending_state = states()[created_while_down]
            check("a change for an unreachable worker stays pending", pending_state["state"] == "pending", json.dumps(pending_state))
            api("DELETE", f"/api/private-connections/{connections['D-C']}")
            wait("target-side revocation", lambda: "revoked on the target" in states()[connections["D-C"]]["detail"], 120, 2)
            denied("revocation applied on the target node alone blocks traffic", "D", "c.internal", 8080)
            run("docker", "exec", "-d", node2, "su", "-s", "/bin/sh", "cove", "-c", "exec /usr/local/bin/cove-worker daemon >>/var/lib/cove/worker.log 2>&1")
            wait("node2 worker socket", lambda: run("docker", "exec", node2, "test", "-S", "/run/cove/worker.sock", check=False).returncode == 0, 60, 1)
            api("POST", f"/api/private-connections/{created_while_down}/retry")
            settled([created_while_down])
            gone(connections["D-C"])
            allowed("a connection created while the worker was down applies after it returns", "D", "db.internal", 8080, "DB")
            api("DELETE", f"/api/private-connections/{created_while_down}")
            gone(created_while_down)
            denied("removed connection is revoked", "D", "db.internal", 8080)

            operate("A", "stop")
            time.sleep(40)
            state = output("docker", "exec", resources["A"]["node"], "docker", "inspect", "--format", "{{.State.Running}}", resources["A"]["container"])
            check("link reconciliation keeps a manually stopped source stopped", state == "false")
            operate("A", "start")
            allowed("A -> DB after starting the source again", "A", "db.internal", 8080, "DB")

            shared = api("POST", "/api/networks", {"name": "legacy", "node_id": nodes["node1"]["id"]})["id"]
            for label, alias in (("A", "a-shared"), ("B", "b-shared")):
                api("PUT", f"/api/workloads/{resources[label]['id']}/networks", {"networks": [{"network_id": shared, "service": "app", "alias": alias}]})
                operate(label, "deploy")
            allowed("an existing shared network still gives broader access", "A", "b-shared", 9090, "B")
            members = graph()["shared_networks"]
            check("the graph reports the shared network as broader access", any(set(n["resources"]) >= {resources["A"]["id"], resources["B"]["id"]} for n in members), str(members))

            for connection in (connections["A-DB"], connections["B-DB"], connections["B-C"]):
                api("DELETE", f"/api/private-connections/{connection}")
            for connection in (connections["A-DB"], connections["B-DB"], connections["B-C"]):
                gone(connection)
            denied("A -> DB after removing every connection", "A", "db.internal", 8080)
            wait("gateways removed when no links remain", lambda: all(run("docker", "exec", n["container"], "docker", "inspect", "cove-link", check=False, capture_output=True).returncode != 0 for n in nodes.values()), 120, 3)
            check("gateways are removed when no links remain", True)
            print(f"\n{len(PASSED)} private link acceptance checks passed.")
        finally:
            if os.environ.get("COVE_LINKS_KEEP") == "1":
                print("Keeping fixture", name, "for inspection", file=sys.stderr)
                containers = []
                network = False
                node_image = ""
            for container in reversed(containers):
                subprocess.run(["docker", "rm", "-f", "-v", container], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if network:
                subprocess.run(["docker", "network", "rm", name], check=False, stdout=subprocess.DEVNULL)
            if node_image:
                subprocess.run(["docker", "image", "rm", node_image], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    try:
        main()
    except AssertionError as error:
        print(error, file=sys.stderr)
        sys.exit(1)
