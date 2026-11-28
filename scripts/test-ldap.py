"""Exercise directory sign-in against disposable OpenLDAP and PostgreSQL."""
import argparse
import base64
import hashlib
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def write_ldap_fixture(directory, password):
    salt = secrets.token_bytes(8)
    hashed = "{SSHA}" + base64.b64encode(hashlib.sha1(password.encode() + salt).digest() + salt).decode()
    (directory / "Dockerfile").write_text("""FROM debian:bookworm-slim
RUN printf '#!/bin/sh\\nexit 101\\n' >/usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d && apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends slapd ldap-utils openssl ca-certificates && rm -rf /var/lib/apt/lists/*
COPY . /fixture/
CMD ["sh", "/fixture/start.sh"]
""", encoding="utf-8")
    (directory / "slapd.conf").write_text(f"""include /etc/ldap/schema/core.schema
include /etc/ldap/schema/cosine.schema
include /etc/ldap/schema/inetorgperson.schema
pidfile /fixture/slapd.pid
argsfile /fixture/slapd.args
modulepath /usr/lib/ldap
moduleload back_mdb
TLSCACertificateFile /fixture/ca.pem
TLSCertificateFile /fixture/server.pem
TLSCertificateKeyFile /fixture/server.key
TLSProtocolMin 3.3
database mdb
maxsize 10485760
suffix "dc=example,dc=invalid"
rootdn "cn=service,dc=example,dc=invalid"
rootpw {hashed}
directory /fixture/data
index objectClass eq
access to attrs=userPassword by anonymous auth by dn.exact="cn=service,dc=example,dc=invalid" manage by * none
access to * by dn.exact="cn=service,dc=example,dc=invalid" manage by * none
""", encoding="utf-8")
    entries = ["dn: dc=example,dc=invalid\nobjectClass: domain\ndc: example", "dn: ou=users,dc=example,dc=invalid\nobjectClass: organizationalUnit\nou: users", "dn: ou=groups,dc=example,dc=invalid\nobjectClass: organizationalUnit\nou: groups"]
    for user in ("alice", "bob", "collision", "factor", "locked"):
        entries.append(f"dn: uid={user},ou=users,dc=example,dc=invalid\nobjectClass: inetOrgPerson\nuid: {user}\ncn: {user.title()} Fixture\nsn: Fixture\nmail: {user}@example.invalid\nuserPassword: {hashed}")
    entries.append("dn: cn=engineering,ou=groups,dc=example,dc=invalid\nobjectClass: groupOfNames\ncn: engineering\n" + "\n".join(f"member: uid={user},ou=users,dc=example,dc=invalid" for user in ("alice", "collision", "factor", "locked")))
    (directory / "data.ldif").write_text("\n\n".join(entries) + "\n", encoding="utf-8")
    (directory / "start.sh").write_text("""#!/bin/sh
set -eu
mkdir -p /fixture/data
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout /fixture/ca.key -out /fixture/ca.pem -subj /CN=Cove-Disposable-Directory-CA >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout /fixture/server.key -out /fixture/server.csr -subj /CN=localhost >/dev/null 2>&1
printf 'subjectAltName=DNS:localhost,DNS:directory,IP:127.0.0.1\\nextendedKeyUsage=serverAuth\\n' >/fixture/extensions
openssl x509 -req -in /fixture/server.csr -CA /fixture/ca.pem -CAkey /fixture/ca.key -CAcreateserial -days 1 -out /fixture/server.pem -extfile /fixture/extensions >/dev/null 2>&1
slapadd -f /fixture/slapd.conf -l /fixture/data.ldif
exec slapd -f /fixture/slapd.conf -h 'ldap://0.0.0.0:1389 ldaps://0.0.0.0:1636' -d 0
""", encoding="utf-8", newline="\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", default="TestLDAP")
    args = parser.parse_args()
    name = "cove-ldap-test-" + secrets.token_hex(6)
    password = secrets.token_hex(24)
    image = name + ":fixture"
    created = []
    with tempfile.TemporaryDirectory(prefix=name) as temporary:
        directory = Path(temporary)
        write_ldap_fixture(directory, password)
        try:
            run("docker", "build", "-q", "-t", image, str(directory), stdout=subprocess.DEVNULL)
            for suffix, options, container_image in (
                ("-ldap", ["-p", "127.0.0.1::1389", "-p", "127.0.0.1::1636"], image),
                ("-db", ["-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB=cove_test", "-p", "127.0.0.1::5432"], "postgres:17-alpine"),
            ):
                run("docker", "run", "-d", "--name", name + suffix, "--label", "cove.test=true", *options, container_image,
                    env=os.environ | {"POSTGRES_PASSWORD": password}, stdout=subprocess.DEVNULL)
                created.append(name + suffix)
            for _ in range(60):
                ready = subprocess.run(["docker", "exec", "-e", "LDAPTLS_CACERT=/fixture/ca.pem", name + "-ldap", "ldapwhoami", "-x", "-H", "ldaps://127.0.0.1:1636"], capture_output=True)
                pg = subprocess.run(["docker", "exec", name + "-db", "pg_isready", "-U", "postgres"], capture_output=True)
                if ready.returncode == 0 and pg.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("Disposable directory/database did not become ready")
            run("docker", "cp", name + "-ldap:/fixture/ca.pem", str(directory / "ca.pem"), stdout=subprocess.DEVNULL)
            def port(suffix, container_port):
                return run("docker", "port", name + suffix, str(container_port) + "/tcp", capture_output=True, text=True).stdout.strip().rsplit(":", 1)[1]
            environment = os.environ | {
                "COVE_TEST_DATABASE_URL": f"postgres://postgres:{password}@127.0.0.1:{port('-db', 5432)}/cove_test?sslmode=disable",
                "COVE_TEST_LDAP_LDAPS_PORT": port("-ldap", 1636),
                "COVE_TEST_LDAP_STARTTLS_PORT": port("-ldap", 1389),
                "COVE_TEST_LDAP_CA": str(directory / "ca.pem"),
                "COVE_TEST_LDAP_PASSWORD": password,
                "COVE_TEST_LDAP_CONTAINER": name + "-ldap",
                "COVE_LDAP_ALLOWED_CIDRS": "127.0.0.1/32,::1/128",
            }
            run("docker", "exec", name + "-ldap", "slapd", "-VV", stderr=subprocess.STDOUT)
            run("go", "test", "./internal/control", "-run", args.run, "-v", "-count=1", "-timeout", "5m", env=environment)
        finally:
            for container in reversed(created):
                run("docker", "rm", "-f", "-v", container, stdout=subprocess.DEVNULL)
            run("docker", "image", "rm", image, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
