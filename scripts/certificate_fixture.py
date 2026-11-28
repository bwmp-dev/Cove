"""Owned validating ACME/DNS fixtures; never changes host trust or public DNS."""
import json
import os
from pathlib import Path
import secrets
import ssl
import subprocess
import time
import urllib.request

PEBBLE = "ghcr.io/letsencrypt/pebble@sha256:0c81f05db371f436c97fb894bd6d2ab8fc9c8e2ce40f8bef8a8e15941bcd75ef"
CHALLENGE = "ghcr.io/letsencrypt/pebble-challtestsrv@sha256:0152c8cb506e2ecba2900ed47d23932e27e3e6575a7d2c8ef93c7719ee3dea4c"


def published_port(container, port):
    result = subprocess.run(["docker", "port", container, port], capture_output=True, text=True)
    if result.returncode:
        logs = subprocess.run(["docker", "logs", "--tail", "20", container], capture_output=True, text=True)
        raise RuntimeError("Owned fixture port unavailable: " + container + " " + port + "\n" + (logs.stdout + logs.stderr)[-4096:])
    return result.stdout.strip().rsplit(":", 1)[1]


def start_certificate_fixture(run, name, fixture, containers, validity=90):
    fixture = Path(fixture)
    image = name + "-certificate-provider:test"
    token = secrets.token_hex(32)
    (fixture / "pebble.json").write_text(json.dumps({"pebble": {
        "listenAddress": "0.0.0.0:14000", "managementListenAddress": "0.0.0.0:15000",
        "certificate": "/fixture/cert.pem", "privateKey": "/fixture/key.pem",
        "httpPort": 5002, "tlsPort": 5001,
        "profiles": {"default": {"description": "Cove isolated DNS-01 acceptance", "validityPeriod": validity}}
    }}), encoding="utf-8")
    run("docker", "build", "-f", "test/certificates/Dockerfile", "-t", image, "test/certificates", stdout=subprocess.DEVNULL)

    def start(suffix, *args, env=None):
        container = name + "-" + suffix
        if subprocess.run(["docker", "inspect", "--type=container", container], capture_output=True).returncode == 0:
            raise RuntimeError("Refusing an existing certificate fixture container")
        try:
            run("docker", "run", "-d", "--name", container, "--network", name,
                "--label", "cove.browser-fixture=" + name,
                "-e", "HTTP_PROXY=", "-e", "HTTPS_PROXY=", "-e", "ALL_PROXY=", "-e", "NO_PROXY=*",
                *args, env=env, stdout=subprocess.DEVNULL)
        finally:
            owner = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', container], capture_output=True, text=True)
            if owner.returncode == 0 and owner.stdout.strip() == name:
                containers.append(container)

    start("challenge", "--network-alias", "challenge", "-p", "127.0.0.1::8055", "-p", "127.0.0.1::53/udp",
          CHALLENGE, "-dnsserver=:53", "-http01=", "-https01=", "-tlsalpn01=", "-doh=")
    start("pebble", "--network-alias", "pebble", "-p", "127.0.0.1::14000", "-p", "127.0.0.1::15000",
          "-e", "PEBBLE_VA_NOSLEEP=1", "-e", "PEBBLE_AUTHZREUSE=0",
          "--mount", f"type=bind,source={fixture},target=/fixture,readonly",
          PEBBLE, "-config", "/fixture/pebble.json", "-dnsserver", "challenge:53")
    start("certificate-provider", "--network-alias", "api.cloudflare.com", "-p", "127.0.0.1::443",
          "--mount", f"type=bind,source={fixture},target=/fixture,readonly", "-e", "COVE_CERTIFICATE_FIXTURE_TOKEN",
          image, env=os.environ | {"COVE_CERTIFICATE_FIXTURE_TOKEN": token})
    trust = ssl.create_default_context(cafile=str(fixture / "cert.pem"))
    provider = "https://localhost:" + published_port(name + "-certificate-provider", "443/tcp")
    directory = "https://localhost:" + published_port(name + "-pebble", "14000/tcp") + "/dir"
    for url in (directory, provider + "/healthz"):
        for _ in range(60):
            try:
                with urllib.request.urlopen(url, context=trust, timeout=2) as response:
                    if response.status == 200:
                        break
            except (OSError, urllib.error.URLError):
                time.sleep(1)
        else:
            raise RuntimeError("Owned certificate fixture did not start")
    address = subprocess.check_output(["docker", "inspect", "--format",
                                      '{{with index .NetworkSettings.Networks "' + name + '"}}{{.IPAddress}}{{end}}',
                                      name + "-challenge"], text=True).strip()
    return {
        "image": image, "token": token, "dns": address,
        "provider": provider, "directory": directory,
        "management": "https://localhost:" + published_port(name + "-pebble", "15000/tcp"),
        "dns_management": "http://127.0.0.1:" + published_port(name + "-challenge", "8055/tcp"),
        "dns_address": "127.0.0.1:" + published_port(name + "-challenge", "53/udp"),
    }
