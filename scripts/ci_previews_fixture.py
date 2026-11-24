"""Owned synthetic Git source and generated TLS for CI image-preview acceptance."""
import json
import os
from pathlib import Path
import re
import subprocess


def install_ci_previews_fixture(run, name, worker, env, directory, openssl):
    if not re.fullmatch(r"cove-browser-[a-f0-9]{12}", name) or worker != name + "-worker":
        raise RuntimeError("Owned CI preview browser fixture required")
    identity = run("docker", "info", "--format", "{{.ID}}", capture_output=True, text=True).stdout.strip()
    marker = run("docker", "exec", worker, "cat", "/var/lib/cove/.browser-owner", capture_output=True, text=True).stdout
    proxy = json.loads(run("docker", "inspect", "cove-ingress", capture_output=True, text=True).stdout)[0]
    if identity != os.environ.get("COVE_BROWSER_MANAGED_DAEMON_ID") or marker != name or proxy["Config"]["Labels"].get("cove.browser-fixture") != name:
        raise RuntimeError("CI preview fixture ownership changed")
    git = '''#!/bin/sh
set -eu
printf 'called\\n' >> /tmp/ci-preview-git-calls
case "$1" in
clone)
    found=0
    for argument in "$@"; do
        test "$argument" != https://git.fixture.invalid/ci-previews.git || found=1
        destination="$argument"
    done
    test "$found" = 1
    case "$destination" in /var/lib/cove/workloads/*/releases/*) ;; *) exit 2;; esac
    mkdir -p "$destination"
    printf '%s' ci-preview-synthetic-source > "$destination/fixture.txt"
    ;;
rev-parse) printf '%s\\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
*) exit 2 ;;
esac
'''
    run("docker", "exec", "--user", "0", worker, "sh", "-ec", "test ! -e /usr/local/bin/git; test ! -e /tmp/ci-preview-git-calls; touch /tmp/ci-preview-git-calls; chown cove:cove /tmp/ci-preview-git-calls; chmod 600 /tmp/ci-preview-git-calls", stdout=subprocess.DEVNULL)
    run("docker", "exec", "-i", "--user", "0", worker, "sh", "-ec", 'cat > /usr/local/bin/git; chmod 755 /usr/local/bin/git', input=git.encode("utf-8"), stdout=subprocess.DEVNULL)
    directory = Path(directory)
    root = Path(env["COVE_BROWSER_MANAGED_REGISTRY_ROOT"]).resolve()
    if root.parent != directory.resolve() or root.name != "registry-ca.pem":
        raise RuntimeError("CI preview TLS must use the owned managed-registry CA")
    key, csr, certificate = (directory / filename for filename in ("ci-preview.key", "ci-preview.csr", "ci-preview.pem"))
    extensions = directory / "ci-preview.ext"
    extensions.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:*.example.test\n", encoding="utf-8", newline="\n")
    quiet = {"stdout": subprocess.DEVNULL, "stderr": subprocess.DEVNULL}
    run(openssl, "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-config", str(directory / "openssl.cnf"), "-subj", "/CN=CI preview fixture", "-keyout", str(key), "-out", str(csr), **quiet)
    run(openssl, "x509", "-req", "-in", str(csr), "-CA", str(root), "-CAkey", str(directory / "registry-ca.key"), "-set_serial", "2", "-days", "2", "-extfile", str(extensions), "-out", str(certificate), **quiet)
    files = {
        "/data/ci-preview-fixture.pem": certificate.read_bytes(),
        "/data/ci-preview-fixture.key": key.read_bytes(),
        "/data/dynamic/ci-preview-fixture.yaml": json.dumps({"tls": {"stores": {"default": {"defaultCertificate": {"certFile": "/data/ci-preview-fixture.pem", "keyFile": "/data/ci-preview-fixture.key"}}}}}).encode("utf-8"),
    }
    for path, contents in files.items():
        run("docker", "exec", "-i", "cove-ingress", "sh", "-ec", 'test ! -e "$1"; umask 077; cat > "$1"', "sh", path, input=contents, stdout=subprocess.DEVNULL)
    env.update(COVE_BROWSER_CI_PREVIEWS="1", COVE_BROWSER_CI_GIT_CALLS="/tmp/ci-preview-git-calls")
