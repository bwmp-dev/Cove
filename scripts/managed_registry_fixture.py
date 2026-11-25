"""Local TLS and DNS setup for managed-registry acceptance on an owned nested daemon."""
import ipaddress
import os
from pathlib import Path
import re
import subprocess

REGISTRY_PIN = "docker.io/library/registry:3.1.2@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c"


class ManagedRegistryFixture:
    def __init__(self, run, fixture, name, openssl, env):
        if not re.fullmatch(r"cove-browser-[a-f0-9]{12}", name):
            raise RuntimeError("Isolated fixture name required")
        identity = run("docker", "info", "--format", "{{.ID}}", capture_output=True, text=True).stdout.strip()
        if not os.environ.get("COVE_BROWSER_MANAGED_DAEMON_ID") or identity != os.environ["COVE_BROWSER_MANAGED_DAEMON_ID"]:
            raise RuntimeError("Managed registry acceptance requires the caller's verified owned nested daemon")
        self.run, self.name, self.fixture = run, name, Path(fixture)
        self.hostname = "registry-" + name.removeprefix("cove-browser-") + ".example.test"
        self.installed = False
        root, root_key = self.fixture / "registry-ca.pem", self.fixture / "registry-ca.key"
        certificate, key = self.fixture / "registry-leaf.pem", self.fixture / "registry-leaf.key"
        quiet = {"stdout": subprocess.DEVNULL, "stderr": subprocess.DEVNULL}
        run(openssl, "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-days", "2", "-config", str(self.fixture / "openssl.cnf"), "-subj", "/CN=Cove managed registry fixture CA", "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", str(root_key), "-out", str(root), **quiet)
        csr = self.fixture / "registry-leaf.csr"
        run(openssl, "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-config", str(self.fixture / "openssl.cnf"), "-subj", "/CN=" + self.hostname, "-keyout", str(key), "-out", str(csr), **quiet)
        extensions = self.fixture / "registry-leaf.ext"
        extensions.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:" + self.hostname + "\n", encoding="utf-8", newline="\n")
        run(openssl, "x509", "-req", "-in", str(csr), "-CA", str(root), "-CAkey", str(root_key), "-set_serial", "1", "-days", "2", "-extfile", str(extensions), "-out", str(certificate), **quiet)
        env.update(COVE_BROWSER_MANAGED_REGISTRY="1", COVE_BROWSER_MANAGED_REGISTRY_HOST=self.hostname,
                   COVE_BROWSER_MANAGED_REGISTRY_CERT=str(certificate), COVE_BROWSER_MANAGED_REGISTRY_KEY=str(key),
                   COVE_BROWSER_MANAGED_REGISTRY_ROOT=str(root))

    def helper(self, image, script, *args):
        if image != self.name + "-worker:development":
            raise RuntimeError("Managed registry trust helper requires this fixture's worker image")
        return self.run("docker", "run", "--rm", "--pull", "never", "--name", self.name + "-registry-trust", "--label", "cove.browser-fixture=" + self.name,
                        "--network", "none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--cpus", "0.5", "--memory", "64m", "--pids-limit", "32",
                        "--mount", "type=bind,source=/etc/docker/certs.d,target=/trust",
                        "--mount", "type=bind,source=/etc/hosts,target=/hosts",
                        "--mount", "type=bind,source=" + str(self.fixture / "registry-ca.pem") + ",target=/ca.pem,readonly",
                        "--tmpfs", "/tmp:size=1m", "--entrypoint", "sh", image, "-ec", script, "sh", self.hostname, self.name, *args,
                        stdout=subprocess.DEVNULL)

    def install(self, image, worker):
        if worker != self.name + "-worker":
            raise RuntimeError("Managed registry fixture cannot modify another worker")
        labels = self.run("docker", "inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', "cove-ingress", capture_output=True, text=True).stdout.strip()
        if labels != self.name:
            raise RuntimeError("Proxy does not belong to managed registry fixture")
        address = self.run("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "cove-ingress").IPAddress}}', "cove-ingress", capture_output=True, text=True).stdout.strip()
        if not ipaddress.ip_address(address).is_private:
            raise RuntimeError("Fixture ingress must have a private address")
        # Register cleanup before either write, including a lost Docker acknowledgement.
        self.installed = True
        self.helper(image, 'test ! -e "/trust/$1"; ! grep -F "$1" /hosts; mkdir "/trust/$1"; printf %s "$2" > "/trust/$1/.cove-browser-owner"; cp /ca.pem "/trust/$1/ca.crt"; printf "\\n%s %s # cove.browser-fixture=%s\\n" "$3" "$1" "$2" >> /hosts', address)
        self.run("docker", "network", "connect", "cove-ingress", worker, stdout=subprocess.DEVNULL)
        self.run("docker", "exec", "--user", "0", worker, "sh", "-ec", 'printf "\\n%s %s\\n" "$1" "$2" >> /etc/hosts', "sh", address, self.hostname, stdout=subprocess.DEVNULL)

    def cleanup(self, image):
        if self.installed:
            self.helper(image, 'if test -e "/trust/$1"; then test "$(cat "/trust/$1/.cove-browser-owner")" = "$2"; rm -rf "/trust/$1"; fi; awk -v owner="cove.browser-fixture=$2" \'index($0,owner)==0\' /hosts > /tmp/hosts; cat /tmp/hosts > /hosts')
            self.installed = False
