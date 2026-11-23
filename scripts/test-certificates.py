"""Exercise actual DNS-01 issuance/renewal/recovery with disposable local services."""
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time

from certificate_fixture import published_port, start_certificate_fixture

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def main():
    name = "cove-certificates-" + secrets.token_hex(6)
    containers = []
    network_created = False
    image = name + "-certificate-provider:test"
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        fixture = Path(directory)
        openssl = shutil.which("openssl") or ("C:/Program Files/Git/usr/bin/openssl.exe" if os.name == "nt" else "openssl")
        (fixture / "openssl.cnf").write_text("[req]\ndistinguished_name=dn\n[dn]\n", encoding="utf-8")
        run(openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-config", str(fixture / "openssl.cnf"),
            "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,DNS:pebble,DNS:api.cloudflare.com",
            "-out", str(fixture / "cert.pem"), "-keyout", str(fixture / "key.pem"), capture_output=True)
        try:
            run("docker", "network", "create", "--label", "cove.certificate-fixture=" + name, name, stdout=subprocess.DEVNULL)
            network_created = True
            config = start_certificate_fixture(run, name, fixture, containers, validity=18)
            password = secrets.token_hex(24)
            env = os.environ | {"POSTGRES_PASSWORD": password}
            database = name + "-db"
            run("docker", "run", "-d", "--name", database, "--network", name, "--label", "cove.browser-fixture=" + name,
                "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB=cove_test", "-p", "127.0.0.1::5432", "postgres:17-alpine", env=env, stdout=subprocess.DEVNULL)
            containers.append(database)
            for _ in range(30):
                if subprocess.run(["docker", "exec", database, "pg_isready", "-h", "127.0.0.1", "-U", "postgres"], capture_output=True).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("Owned certificate database did not start")
            env.update(COVE_TEST_DATABASE_URL="postgres://postgres:" + password + "@127.0.0.1:" + published_port(database, "5432/tcp") + "/cove_test?sslmode=disable",
                       COVE_TEST_CERTIFICATE_ACME_DIRECTORY=config["directory"], COVE_TEST_CERTIFICATE_ACME_ROOT=str(fixture / "cert.pem"),
                       COVE_TEST_CERTIFICATE_DNS_MANAGEMENT=config["dns_management"], COVE_TEST_CERTIFICATE_DNS_ADDRESS=config["dns_address"],
                       COVE_TEST_CERTIFICATE_PROVIDER=config["provider"], COVE_TEST_CERTIFICATE_PROVIDER_TOKEN=config["token"],
                       COVE_TEST_CERTIFICATE_ISSUANCE_ROOT=config["management"] + "/roots/0", COVE_TEST_CERTIFICATE_ACME_RENEWAL="1", COVE_TEST_CERTIFICATE_TRAEFIK="1")
            run("go", "test", "-v", "./internal/control", "-run", "^TestCertificateACME(AccountRecoversCommittedKeyPebble|DNS01Pebble|ControllerPublishesValidatesAndCancelsPebble|ConnectedProviderTLS)$", "-count=1", "-timeout=4m", env=env)
        finally:
            failures = []
            for container in reversed(containers):
                owner = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', container], capture_output=True, text=True)
                if owner.returncode != 0 or owner.stdout.strip() != name:
                    failures.append("could not confirm ownership: " + container)
                    continue
                if subprocess.run(["docker", "rm", "-f", "-v", container], stdout=subprocess.DEVNULL).returncode:
                    failures.append("could not remove owned fixture: " + container)
            if network_created:
                if subprocess.run(["docker", "network", "rm", name], stdout=subprocess.DEVNULL).returncode:
                    failures.append("could not remove owned fixture network")
            subprocess.run(["docker", "image", "rm", image], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if failures:
                raise RuntimeError("; ".join(failures))


if __name__ == "__main__":
    main()
