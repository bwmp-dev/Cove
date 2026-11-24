from pathlib import Path
import re
import subprocess
import time

BUILDKIT_CACHE_ID = "sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c"


class BuildNodesFixture:
    def __init__(self, run, name, fixture, env, containers):
        if not re.fullmatch(r"cove-browser-[a-f0-9]{12}", name):
            raise RuntimeError("Owned browser fixture required")
        self.run, self.name, self.fixture, self.env, self.containers = run, name, Path(fixture), env, containers
        self.volume = name + "-builder-state"
        self.volume_created = False

    def start(self, image, api, hostname):
        if image != self.name + "-worker:test" or not re.fullmatch(r"registry-[a-f0-9]{12}\.example\.test", hostname):
            raise RuntimeError("Build fixture image or hostname mismatch")
        current = self.run("docker", "info", "--format", "{{.ID}}", capture_output=True, text=True).stdout.strip()
        if current != self.env.get("COVE_BROWSER_MANAGED_DAEMON_ID"):
            raise RuntimeError("Build acceptance requires its verified owned nested daemon")
        identity = self.run("docker", "image", "inspect", "moby/buildkit:buildx-stable-1", "--format", "{{.Id}}", capture_output=True, text=True).stdout.strip()
        if identity != BUILDKIT_CACHE_ID:
            raise RuntimeError("Cached BuildKit identity does not match the approved artifact")
        daemon, worker = self.name + "-build-docker", self.name + "-builder"
        self.run("docker", "run", "-d", "--pull", "never", "--name", daemon, "--label", "cove.browser-fixture=" + self.name,
                 "--privileged", "--cpus", "2", "--memory", "2g", "--network", self.name, "--network-alias", "build-docker",
                 "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind", "--tls=false", stdout=subprocess.DEVNULL)
        self.containers.append(daemon)
        for _ in range(60):
            try:
                self.run("docker", "exec", daemon, "docker", "info", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                break
            except subprocess.CalledProcessError:
                time.sleep(1)
        else:
            raise RuntimeError("Owned build daemon did not become ready")
        builder_id = self.run("docker", "exec", daemon, "docker", "info", "--format", "{{.ID}}", capture_output=True, text=True).stdout.strip()
        if builder_id == current:
            raise RuntimeError("Build and runtime Docker daemons must be distinct")
        archive = self.fixture / "builder-cache.tar"
        with archive.open("wb") as output:
            self.run("docker", "image", "save", "moby/buildkit:buildx-stable-1", "busybox:1.37", stdout=output)
        if archive.stat().st_size > 256 * 1024 * 1024:
            raise RuntimeError("Cached build fixture archive exceeds its bound")
        with archive.open("rb") as source:
            self.run("docker", "exec", "-i", daemon, "docker", "load", stdin=source, stdout=subprocess.DEVNULL)
        proxy = self.run("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "cove-ingress").IPAddress}}', "cove-ingress", capture_output=True, text=True).stdout.strip()
        self.run("docker", "network", "connect", "cove-ingress", daemon, stdout=subprocess.DEVNULL)
        self.run("docker", "exec", "-i", daemon, "sh", "-ec", 'mkdir -p "/etc/docker/certs.d/$1"; cat > "/etc/docker/certs.d/$1/ca.crt"; printf "\\n%s %s\\n" "$2" "$1" >> /etc/hosts', "sh", hostname, proxy,
                 input=(self.fixture / "registry-ca.pem").read_bytes(), stdout=subprocess.DEVNULL)
        self.volume_created = True
        self.run("docker", "volume", "create", "--label", "cove.browser-fixture=" + self.name, self.volume, stdout=subprocess.DEVNULL)
        self.run("docker", "run", "--rm", "--pull", "never", "--label", "cove.browser-fixture=" + self.name, "--network", "none", "--entrypoint", "sh",
                 "--mount", "type=volume,src=" + self.volume + ",dst=/state", image,
                 "-ec", 'cp -a /var/lib/cove/. /state/; printf %s "$1" > /state/.browser-owner; chown -R cove:cove /state', "sh", self.name, stdout=subprocess.DEVNULL)
        self.run("docker", "run", "-d", "--pull", "never", "--name", worker, "--label", "cove.browser-fixture=" + self.name,
                 "--network", self.name, "--network-alias", "builder", "--add-host", hostname + ":" + proxy,
                 "-e", "DOCKER_HOST=tcp://build-docker:2375", "-e", "SSL_CERT_FILE=/fixture-root.pem",
                 "--mount", "type=bind,source=" + str(self.fixture / "registry-ca.pem") + ",target=/fixture-root.pem,readonly",
                 "--mount", "type=volume,src=" + self.volume + ",dst=/var/lib/cove", image, stdout=subprocess.DEVNULL)
        self.containers.append(worker)
        self.run("docker", "network", "connect", "cove-ingress", worker, stdout=subprocess.DEVNULL)
        enrollment = api("POST", "/api/nodes", {"name": "ready-browser-builder", "address": "builder", "port": 22})
        token = re.search(r"Authorization: Bearer ([^']+)", enrollment["setup_command"]).group(1)
        installer = api("GET", "/install.sh", credential=token, raw=True)
        authorized = re.search(r"'(restrict,command=\"[^\"]+\" ssh-ed25519 [A-Za-z0-9+/=]+)'", installer).group(1)
        self.run("docker", "exec", "-i", worker, "sh", "-c", "cat > /var/lib/cove/.ssh/authorized_keys && chown cove:cove /var/lib/cove/.ssh/authorized_keys && chmod 600 /var/lib/cove/.ssh/authorized_keys", input=authorized + "\n", text=True)
        key = self.run("docker", "exec", worker, "cat", "/etc/ssh/ssh_host_ed25519_key.pub", capture_output=True, text=True).stdout.strip()
        api("POST", "/bootstrap/register", {"host_key": key}, credential=token)
        for _ in range(30):
            node = next(item for item in api("GET", "/api/nodes") if item["id"] == enrollment["id"])
            if node["state"] == "ready" and node.get("capabilities", {}).get("remote_builds"):
                break
            api("POST", "/api/nodes/" + enrollment["id"] + "/verify")
            time.sleep(1)
        else:
            raise RuntimeError("Browser build worker did not become ready")
        self.env.update(COVE_BROWSER_BUILD_NODES="1", COVE_BROWSER_BUILDER=worker, COVE_BROWSER_BUILD_DOCKER=daemon)

    def cleanup(self):
        for suffix in ("builder", "build-docker"):
            name = self.name + "-" + suffix
            result = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', name], capture_output=True, text=True)
            if result.returncode == 0:
                if result.stdout.strip() != self.name:
                    raise RuntimeError("Builder ownership changed; cleanup refused")
                self.run("docker", "rm", "-f", "-v", name, stdout=subprocess.DEVNULL)
        if self.volume_created:
            result = subprocess.run(["docker", "volume", "inspect", self.volume, "--format", '{{index .Labels "cove.browser-fixture"}}'], capture_output=True, text=True)
            if result.returncode == 0:
                if result.stdout.strip() != self.name:
                    raise RuntimeError("Builder state ownership changed; cleanup refused")
                self.run("docker", "volume", "rm", self.volume, stdout=subprocess.DEVNULL)
            elif self.run("docker", "volume", "ls", "-q", "--filter", "name=^" + self.volume + "$", capture_output=True, text=True).stdout.strip():
                raise RuntimeError("Builder state could not be inspected; cleanup remains pending")
            self.volume_created = False
