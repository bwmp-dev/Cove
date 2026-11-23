"""Own local Tunnel provider/edge; uses generated credentials and no public DNS."""
import os
from pathlib import Path
import secrets
import socket
import socketserver
import ssl
import subprocess
import threading
import time
import urllib.request
import re


class TunnelFixtureBridge(socketserver.ThreadingTCPServer):
    daemon_threads = True
    block_on_close = False

    def __init__(self, name, role, port):
        if not re.fullmatch(r"cove-browser-[a-f0-9]{12}", name) or role not in ("tls", "tunnel-provider"):
            raise ValueError("Unexpected isolated TLS bridge identity")
        container = name + "-" + role
        owner = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', container], capture_output=True, text=True, check=True).stdout.strip()
        if owner != name:
            raise RuntimeError("Refusing a TLS bridge to a foreign fixture")
        self.command = ["docker", "exec", "-i", container, *(["/provider", "fixture-stream"] if role == "tunnel-provider" else ["nc", "127.0.0.1", "443"])]
        self.children = set()
        self.children_lock = threading.Lock()
        super().__init__(("127.0.0.1", port), TunnelBridgeRequest)
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def stop(self):
        self.shutdown()
        self.server_close()
        with self.children_lock:
            children = list(self.children)
        for process in children:
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        self.thread.join(timeout=3)


class TunnelBridgeRequest(socketserver.BaseRequestHandler):
    def handle(self):
        process = subprocess.Popen(self.server.command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0,
                                   creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        with self.server.children_lock:
            self.server.children.add(process)

        def feed():
            try:
                while data := self.request.recv(65536):
                    process.stdin.write(data)
            except (OSError, ValueError):
                pass
            finally:
                process.stdin.close()

        threading.Thread(target=feed, daemon=True).start()
        try:
            while data := process.stdout.read(65536):
                self.request.sendall(data)
        except OSError:
            pass
        finally:
            try:
                self.request.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            process.stdout.close()
            with self.server.children_lock:
                self.server.children.discard(process)


def start_tunnel_fixture(run, name, fixture, containers, bridges):
    fixture = Path(fixture)
    image = name + "-tunnel-provider:test"
    token = secrets.token_hex(32)
    run("docker", "build", "-f", "test/tunnels/Dockerfile", "-t", image, "test/tunnels", stdout=subprocess.DEVNULL)
    container = name + "-tunnel-provider"
    try:
        run("docker", "run", "-d", "--name", container, "--network", name, "--network-alias", "api.cloudflare.com",
            "--label", "cove.browser-fixture=" + name, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
            "--mount", f"type=bind,source={fixture},target=/fixture,readonly",
            "-e", "COVE_TUNNEL_FIXTURE_TOKEN", image, env=os.environ | {"COVE_TUNNEL_FIXTURE_TOKEN": token}, stdout=subprocess.DEVNULL)
    finally:
        actual = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', container], capture_output=True, text=True)
        if actual.returncode == 0 and actual.stdout.strip() == name:
            containers.append(container)
    bridge = TunnelFixtureBridge(name, "tunnel-provider", 0)
    bridges.append(bridge)
    provider = "https://localhost:" + str(bridge.server_address[1])
    trust = ssl.create_default_context(cafile=str(fixture / "cert.pem"))
    for _ in range(60):
        try:
            with urllib.request.urlopen(provider + "/healthz", context=trust, timeout=2) as response:
                if response.status == 200:
                    break
        except (OSError, urllib.error.URLError):
            time.sleep(1)
    else:
        raise RuntimeError("Owned fake Tunnel provider did not start")
    address = run("docker", "inspect", "--format", '{{with index .NetworkSettings.Networks "'+name+'"}}{{.IPAddress}}{{end}}', container, capture_output=True, text=True).stdout.strip()
    return {"image": image, "token": token, "provider": provider, "address": "https://" + address}
