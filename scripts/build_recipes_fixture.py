"""Synthetic Git source for packaged static-output acceptance; no Git provider is contacted."""
import json
import os
from pathlib import Path
import re
import subprocess

from build_nodes_fixture import BUILDKIT_CACHE_ID

BUILDKIT_PIN = "docker.io/moby/buildkit@sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c"


def install_build_recipes_fixture(run, name, worker, env):
    if not re.fullmatch(r"cove-browser-[a-f0-9]{12}", name) or worker != name + "-worker":
        raise RuntimeError("Owned recipe browser fixture required")
    identity = run("docker", "info", "--format", "{{.ID}}", capture_output=True, text=True).stdout.strip()
    if identity != os.environ.get("COVE_BROWSER_MANAGED_DAEMON_ID"):
        raise RuntimeError("Recipe acceptance requires its verified owned nested daemon")
    metadata = json.loads(run("docker", "inspect", worker, capture_output=True, text=True).stdout)[0]
    if metadata["Config"]["Image"] != name + "-worker:development" or name not in metadata["NetworkSettings"]["Networks"]:
        raise RuntimeError("Recipe worker ownership changed")
    marker = run("docker", "exec", worker, "cat", "/var/lib/cove/.browser-owner", capture_output=True, text=True).stdout
    if marker != name:
        raise RuntimeError("Recipe worker state ownership changed")
    image = run("docker", "image", "inspect", "moby/buildkit:buildx-stable-1", "--format", "{{.Id}}", capture_output=True, text=True).stdout.strip()
    if image != BUILDKIT_CACHE_ID:
        raise RuntimeError("Recipe builder cache differs from the approved pin")
    root = Path(env.get("COVE_BROWSER_MANAGED_REGISTRY_ROOT", "")).resolve()
    if not root.is_file() or root.name != "registry-ca.pem" or not root.parent.name.startswith(name):
        raise RuntimeError("Recipe fixture requires its own managed-registry public CA")
    certificate = root.read_bytes()
    if len(certificate) > 128 * 1024 or b"-----BEGIN CERTIFICATE-----" not in certificate or b"PRIVATE KEY" in certificate:
        raise RuntimeError("Recipe fixture public CA is invalid")
    git = """#!/bin/sh
set -eu
case "$1" in
clone)
    found=0
    for argument in "$@"; do
        test "$argument" != https://git.fixture.invalid/build-recipes.git || found=1
        destination="$argument"
    done
    test "$found" = 1
    case "$destination" in /var/lib/cove/workloads/*/releases/*) ;; *) exit 2;; esac
    mkdir -p "$destination"
    cp -R /tmp/cove-recipe-fixture/source/. "$destination/"
    ;;
rev-parse) printf '%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
*) exit 2 ;;
esac
"""
    docker = """#!/bin/sh
set -eu
if [ "$1" = buildx ] && [ "${2:-}" = create ]; then
    exec /usr/local/bin/recipe-real-docker "$@" --driver-opt image=""" + BUILDKIT_PIN + "\nfi\nexec /usr/local/bin/recipe-real-docker \"$@\"\n"
    script = ('test ! -e /usr/local/bin/recipe-real-docker; test ! -e /usr/local/bin/git; '
              'mkdir -p /tmp/cove-recipe-fixture/source/public; '
              'printf %s static-browser-fixture > /tmp/cove-recipe-fixture/source/public/index.html; '
              'printf %s fixture-private > /tmp/cove-recipe-fixture/source/public/.env; '
              'printf %s fixture-private > /tmp/cove-recipe-fixture/source/private.txt; '
              'printf "FROM unsupported.invalid/never:latest\\n" > /tmp/cove-recipe-fixture/source/Dockerfile; '
              'chmod -R a+rX /tmp/cove-recipe-fixture; '
              'mv /usr/local/bin/docker /usr/local/bin/recipe-real-docker')
    run("docker", "exec", "--user", "0", worker, "sh", "-ec", script, stdout=subprocess.DEVNULL)
    for path, content in (("/usr/local/bin/git", git), ("/usr/local/bin/docker", docker)):
        run("docker", "exec", "-i", "--user", "0", worker, "sh", "-ec", 'cat > "$1"; chmod 755 "$1"', "sh", path, input=content.encode("utf-8"), stdout=subprocess.DEVNULL)
    run("docker", "cp", str(root), worker + ":/tmp/cove-recipe-fixture/ca.pem", stdout=subprocess.DEVNULL)
    run("docker", "exec", "--user", "0", worker, "sh", "-ec", 'cat /tmp/cove-recipe-fixture/ca.pem >> /etc/ssl/certs/ca-certificates.crt', stdout=subprocess.DEVNULL)
    probe = json.loads(run("docker", "exec", "--user", "cove", worker, "/usr/local/bin/cove-worker", "probe", capture_output=True, text=True).stdout)
    if not probe.get("ready") or not probe.get("build_recipes"):
        raise RuntimeError("Recipe fixture probe is not ready: " + json.dumps({key: probe.get(key) for key in ("ready", "docker", "compose", "build_publish", "build_recipes")}))
    env["COVE_BROWSER_BUILD_RECIPES"] = "1"
