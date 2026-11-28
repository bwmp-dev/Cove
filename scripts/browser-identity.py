"""Identity protocol fixtures used only by the locked packaged browser harness."""
import importlib.util
import json
from pathlib import Path
import secrets
import shutil
import subprocess
import time
import urllib.request

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, check=True, cwd=ROOT, **kwargs)


def start(name, fixture, env, containers, images, openssl):
    module_spec = importlib.util.spec_from_file_location("cove_ldap_fixture", ROOT / "scripts/test-ldap.py")
    ldap = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(ldap)
    directory = fixture / "directory"
    directory.mkdir()
    password = secrets.token_hex(24)
    ldap.write_ldap_fixture(directory, password)
    image = name + "-ldap:acceptance"
    run("docker", "build", "-q", "-t", image, str(directory), stdout=subprocess.DEVNULL)
    images.append(image)
    container = name + "-ldap"
    run("docker", "run", "-d", "--name", container, "--network", name, "--network-alias", "directory",
        "--label", "cove.test=true", image, stdout=subprocess.DEVNULL)
    containers.append(container)
    for _ in range(60):
        ready = subprocess.run(["docker", "exec", "-e", "LDAPTLS_CACERT=/fixture/ca.pem", container,
                                "ldapwhoami", "-x", "-H", "ldaps://127.0.0.1:1636"], capture_output=True)
        if ready.returncode == 0:
            break
        time.sleep(1)
    else:
        raise RuntimeError("Packaged identity OpenLDAP fixture did not become ready")
    run("docker", "cp", container + ":/fixture/ca.pem", str(fixture / "directory-ca.pem"), stdout=subprocess.DEVNULL)
    details = json.loads(run("docker", "inspect", container, capture_output=True, text=True).stdout)[0]
    address = details["NetworkSettings"]["Networks"][name]["IPAddress"]
    if not address:
        raise RuntimeError("Owned directory fixture has no network address")

    run(openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
        "-keyout", str(fixture / "github-ca.key"), "-out", str(fixture / "github-ca.pem"),
        "-subj", "/CN=Cove-Disposable-GitHub-CA", "-addext", "basicConstraints=critical,CA:TRUE",
        "-addext", "keyUsage=critical,keyCertSign,cRLSign", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    run(openssl, "req", "-newkey", "rsa:2048", "-nodes", "-keyout", str(fixture / "github.key"),
        "-out", str(fixture / "github.csr"), "-subj", "/CN=api.github.com", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    (fixture / "github.ext").write_text("subjectAltName=DNS:api.github.com,DNS:github.com\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n", encoding="utf-8")
    run(openssl, "x509", "-req", "-in", str(fixture / "github.csr"), "-CA", str(fixture / "github-ca.pem"),
        "-CAkey", str(fixture / "github-ca.key"), "-CAcreateserial", "-days", "1", "-out", str(fixture / "github.pem"),
        "-extfile", str(fixture / "github.ext"), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    shutil.copyfile(ROOT / "scripts/identity-fixture.py", fixture / "provider.py")
    env.update(COVE_IDENTITY_ACCESS_TOKEN=secrets.token_hex(32), COVE_IDENTITY_FIXTURE_TOKEN=secrets.token_hex(32),
               COVE_IDENTITY_CLIENT_ID="cove-browser-identity", COVE_IDENTITY_CLIENT_SECRET=secrets.token_hex(32))
    options = [item for key in ("COVE_IDENTITY_ACCESS_TOKEN", "COVE_IDENTITY_FIXTURE_TOKEN", "COVE_IDENTITY_CLIENT_ID", "COVE_IDENTITY_CLIENT_SECRET") for item in ("-e", key)]
    provider = name + "-identity"
    run("docker", "run", "-d", "--name", provider, "--network", name, "--network-alias", "identity",
        "--label", "cove.test=true", "-p", "127.0.0.1::8080", "--mount", f"type=bind,source={fixture},target=/fixture,readonly",
        *options, "python:3.12-alpine", "python", "/fixture/provider.py", env=env, stdout=subprocess.DEVNULL)
    containers.append(provider)
    port = run("docker", "port", provider, "8080/tcp", capture_output=True, text=True).stdout.strip().rsplit(":", 1)[1]
    origin = "http://127.0.0.1:" + port
    for _ in range(30):
        try:
            with urllib.request.urlopen(origin + "/fixture/health", timeout=2) as response:
                if response.status == 200:
                    break
        except (OSError, urllib.error.URLError):
            time.sleep(1)
    else:
        raise RuntimeError("Controlled GitHub TLS provider did not become ready")
    env.update(COVE_BROWSER_EMAIL="admin@example.invalid", COVE_IDENTITY_MEMBER_ID=secrets.token_hex(16),
               COVE_IDENTITY_LDAP_HOST="directory", COVE_IDENTITY_LDAP_PORT="1636",
               COVE_IDENTITY_LDAP_CA=(fixture / "directory-ca.pem").read_text(), COVE_IDENTITY_LDAP_PASSWORD=password,
               COVE_IDENTITY_LDAP_USER="alice", COVE_IDENTITY_LDAP_EMAIL="alice@example.invalid",
               COVE_IDENTITY_FIXTURE_URL=origin, COVE_LDAP_ALLOWED_CIDRS=address + "/32",
               COVE_GITHUB_CLIENT_ID=env["COVE_IDENTITY_CLIENT_ID"], COVE_GITHUB_CLIENT_SECRET=env["COVE_IDENTITY_CLIENT_SECRET"])
    return ["-e", "COVE_LDAP_ALLOWED_CIDRS", "-e", "COVE_GITHUB_CLIENT_ID", "-e", "COVE_GITHUB_CLIENT_SECRET",
            "-e", "HTTPS_PROXY=http://identity:8080", "-e", "HTTP_PROXY=", "-e", "NO_PROXY=localhost,127.0.0.1,panel,db,worker,directory,identity",
            "-e", "SSL_CERT_FILE=/identity-trust.pem", "--mount", f"type=bind,source={fixture / 'github-ca.pem'},target=/identity-trust.pem,readonly"]


def seed(database, env):
    user = env["COVE_IDENTITY_MEMBER_ID"]
    if len(user) != 32 or any(character not in "0123456789abcdef" for character in user):
        raise RuntimeError("Unexpected owned fixture identity ID")
    sql = "UPDATE users SET email='admin@example.invalid',email_verified=true,github_id='100' WHERE id='bootstrap';\n"
    sql += f"INSERT INTO users(id,email,name,email_verified,github_id) VALUES('{user}','github-member@example.invalid','GitHub fixture member',true,'101');\n"
    run("docker", "exec", "-i", database, "psql", "-U", "postgres", "-d", "cove", "-v", "ON_ERROR_STOP=1", "-q", input=sql, text=True)
