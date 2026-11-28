"""Exercise actual DNS-01 issuance/renewal/recovery with disposable local services."""
import os
import argparse
import json
import re
import importlib.util
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



def fixed_test_binary(package, linux=False):
    directory = Path(tempfile.gettempdir()) / "cove-test-bin" / ROOT.name
    directory.mkdir(parents=True, exist_ok=True)
    return directory / (package + ("-linux.test" if linux else ".test.exe" if os.name == "nt" else ".test"))


def late_dns():
    from certificate_fixture import PEBBLE, CHALLENGE
    owner = "cove-certificate-retry-" + secrets.token_hex(6)
    references = ["docker:29-dind", "traefik:v3.7.19", "busybox:1.37", PEBBLE, CHALLENGE]
    cached = {reference: json.loads(subprocess.check_output(["docker", "image", "inspect", reference]))[0] for reference in references}
    binary = fixed_test_binary("worker", linux=True)
    run("go", "test", "-c", "-o", str(binary), "./internal/worker", env=os.environ | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"})
    with tempfile.TemporaryDirectory(prefix=owner) as directory:
        scratch = Path(directory)
        archive = scratch / "images.tar"
        run("docker", "save", "-o", str(archive), *references[1:])
        created = False
        identity = None
        try:
            run("docker", "run", "-d", "--pull=never", "--privileged", "--network", "none", "--name", owner,
                "--label", "cove.certificate-retry=" + owner, "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind", "--tls=false", stdout=subprocess.DEVNULL)
            created = True
            identity = json.loads(subprocess.check_output(["docker", "inspect", owner]))[0]["Id"]
            for _ in range(60):
                if subprocess.run(["docker", "exec", owner, "docker", "info"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("Owned certificate retry daemon did not start")
            run("docker", "cp", str(archive), owner + ":/images.tar")
            run("docker", "exec", owner, "docker", "load", "-i", "/images.tar", stdout=subprocess.DEVNULL)
            run("docker", "cp", str(binary), owner + ":/worker.test")
            run("docker", "exec", owner, "chmod", "755", "/worker.test")
            # Save/load may preserve only image IDs, not registry digest references.
            ids = sorted(set(subprocess.check_output(["docker", "exec", owner, "docker", "image", "ls", "-aq"]).decode("utf-8").split()))
            imported = json.loads(subprocess.check_output(["docker", "exec", owner, "docker", "image", "inspect", *ids]))
            runtime = {}
            for reference in references[1:]:
                expected = cached[reference]
                # Containerd and classic image stores report different IDs and sizes
                # for the same archive. Verify runtime configuration and layer digests.
                matching = [image for image in imported if all(image[field] == expected[field] for field in ("Architecture", "Os", "Config", "RootFS"))]
                if len(matching) != 1:
                    raise RuntimeError("Cached certificate fixture contents changed: " + reference)
                runtime[reference] = matching[0]["Id"]
                print("Verified cached fixture contents:", reference, "source", expected["Id"], expected["Size"], "runtime", matching[0]["Id"], matching[0]["Size"], flush=True)
            result = subprocess.run(["docker", "exec", "-e", "COVE_TEST_CERTIFICATE_RETRY_DOCKER=1",
                                     "-e", "COVE_TEST_CERTIFICATE_RETRY_PEBBLE_IMAGE=" + runtime[PEBBLE],
                                     "-e", "COVE_TEST_CERTIFICATE_RETRY_CHALLENGE_IMAGE=" + runtime[CHALLENGE],
                                     owner, "/worker.test", "-test.v", "-test.run=^TestPublicCertificateRetryLateDNSPebble$", "-test.timeout=5m"],
                                    capture_output=True, timeout=330)
            output = (result.stdout + result.stderr).decode("utf-8")
            print(output)
            passed = re.findall(r"^--- PASS: (Test[^ /]+) ", output, re.M)
            if result.returncode or passed != ["TestPublicCertificateRetryLateDNSPebble"] or "--- SKIP:" in output or "--- FAIL:" in output:
                raise RuntimeError("Late-DNS acceptance requires exactly one passing test and zero skips")
            if subprocess.check_output(["docker", "exec", owner, "docker", "ps", "-aq"]).strip():
                raise RuntimeError("Late-DNS test leaked an inner container")
            networks = json.loads(subprocess.check_output(["docker", "exec", owner, "docker", "network", "inspect", "cove-retry-app_default"]))
            if len(networks) != 1 or networks[0]["Containers"] or networks[0]["Labels"].get("com.docker.compose.project") != "cove-retry-app":
                raise RuntimeError("Could not confirm the empty fixture application network")
            run("docker", "exec", owner, "docker", "network", "rm", networks[0]["Id"], stdout=subprocess.DEVNULL)
            remaining = subprocess.check_output(["docker", "exec", owner, "docker", "network", "ls", "--format", "{{.Name}}"]).decode("utf-8").split()
            if set(remaining) != {"bridge", "host", "none"}:
                raise RuntimeError("Late-DNS test leaked an inner network")
            if subprocess.check_output(["docker", "exec", owner, "docker", "volume", "ls", "-q"]).strip():
                raise RuntimeError("Late-DNS test leaked an inner volume")
        finally:
            if created:
                actual = json.loads(subprocess.check_output(["docker", "inspect", owner]))[0]
                if actual["Id"] != identity or actual["Config"]["Labels"].get("cove.certificate-retry") != owner:
                    raise RuntimeError("Certificate retry fixture ownership changed; cleanup refused")
                run("docker", "rm", "-f", "-v", identity, stdout=subprocess.DEVNULL)
                if subprocess.run(["docker", "inspect", identity], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                    raise RuntimeError("Owned certificate retry daemon cleanup was not confirmed")


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
            env.update(COVE_TEST_DATABASE_CONTAINER=database, COVE_TEST_CERTIFICATE_FIXTURE=name, COVE_TEST_DATABASE_URL="postgres://postgres:" + password + "@127.0.0.1:" + published_port(database, "5432/tcp") + "/cove_test?sslmode=disable",
                       COVE_TEST_CERTIFICATE_ACME_DIRECTORY=config["directory"], COVE_TEST_CERTIFICATE_ACME_ROOT=str(fixture / "cert.pem"),
                       COVE_TEST_CERTIFICATE_DNS_MANAGEMENT=config["dns_management"], COVE_TEST_CERTIFICATE_DNS_ADDRESS=config["dns_address"],
                       COVE_TEST_CERTIFICATE_PROVIDER=config["provider"], COVE_TEST_CERTIFICATE_PROVIDER_TOKEN=config["token"],
                       COVE_TEST_CERTIFICATE_ISSUANCE_ROOT=config["management"] + "/roots/0", COVE_TEST_CERTIFICATE_ACME_RENEWAL="1", COVE_TEST_CERTIFICATE_TRAEFIK="1")
            binary = fixed_test_binary("control")
            run("go", "test", "-c", "-o", str(binary), "./internal/control")
            result = subprocess.run([str(binary), "-test.v", "-test.run=^TestCertificateACME(AccountRecoversCommittedKeyPebble|DNS01Pebble|ControllerPublishesValidatesAndCancelsPebble|ConnectedProviderTLS)$", "-test.timeout=4m"], cwd=ROOT / "internal/control", env=env, capture_output=True, timeout=270)
            output = (result.stdout + result.stderr).decode("utf-8")
            print(output)
            expected = {"TestCertificateACME" + name for name in ("AccountRecoversCommittedKeyPebble", "DNS01Pebble", "ControllerPublishesValidatesAndCancelsPebble", "ConnectedProviderTLS")}
            passed = re.findall(r"^--- PASS: (Test[^ /]+) ", output, re.M)
            if result.returncode or set(passed) != expected or len(passed) != len(expected) or "--- SKIP:" in output or "--- FAIL:" in output:
                raise RuntimeError("Certificate acceptance requires all four selected tests to pass with zero skips")
        finally:
            failures = []
            owned = subprocess.run(["docker", "ps", "-a", "--format", "{{.Names}}", "--filter", "label=cove.browser-fixture=" + name], capture_output=True, text=True, check=True).stdout.split()
            for container in list(dict.fromkeys([*owned, *reversed(containers)])):
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
    spec = importlib.util.spec_from_file_location("cove_shared_browser_harness", ROOT / "scripts/test-browser.py")
    harness = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(harness)
    parser = argparse.ArgumentParser()
    parser.add_argument("--late-dns", action="store_true", help="Run the owned Linux proxy/ACME retry regression")
    arguments = parser.parse_args()
    with harness.browser_fixture_lock():
        if arguments.late_dns:
            late_dns()
        else:
            main()
