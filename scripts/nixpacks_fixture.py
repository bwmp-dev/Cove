"""Offline Nixpacks source and digest-verified OCI mirror for the owned browser fixture."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess

BASE_PIN = "ghcr.io/railwayapp/nixpacks@sha256:d45c89d80e13d7ad0fd555b5130f22a866d9dd10e861f589932303ef2314c7de"
PLANNER_SHA256 = "0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6"


def install_nixpacks_fixture(run, name, worker, env, directory):
    if not re.fullmatch(r"cove-browser-[a-f0-9]{12}", name) or worker != name + "-worker":
        raise RuntimeError("Owned Nixpacks browser fixture required")
    identity = run("docker", "info", "--format", "{{.ID}}", capture_output=True, text=True).stdout.strip()
    marker = run("docker", "exec", worker, "cat", "/var/lib/cove/.browser-owner", capture_output=True, text=True).stdout
    if identity != os.environ.get("COVE_BROWSER_MANAGED_DAEMON_ID") or marker != name:
        raise RuntimeError("Nixpacks fixture ownership changed")
    archive = Path(os.environ.get("COVE_BROWSER_NIXPACKS_PLANNER", "")).resolve()
    if not archive.is_file() or archive.stat().st_size != 3717141:
        raise RuntimeError("Nixpacks fixture requires the verified official amd64 planner archive")
    with archive.open("rb") as source:
        if hashlib.file_digest(source, "sha256").hexdigest() != PLANNER_SHA256:
            raise RuntimeError("Nixpacks fixture planner checksum mismatch")
    blobs = Path(os.environ.get("COVE_BROWSER_NIXPACKS_BLOBS", "")).resolve()
    if not blobs.is_dir() or not (blobs / BASE_PIN.split(":")[-1]).is_file():
        raise RuntimeError("Nixpacks fixture requires its verified public OCI blob cache")
    public = Path(directory) / "nixpacks-mirror"
    public.mkdir()
    target = public / "base"
    target.mkdir()
    count, total = 0, 0
    for blob in blobs.iterdir():
        count += 1
        total += blob.stat().st_size
        if blob.is_symlink() or not blob.is_file() or not re.fullmatch(r"[a-f0-9]{64}", blob.name) or count > 64 or total > 512 << 20:
            raise RuntimeError("Nixpacks fixture OCI cache contains unsafe or oversized entries")
        with blob.open("rb") as source:
            if hashlib.file_digest(source, "sha256").hexdigest() != blob.name:
                raise RuntimeError("Nixpacks fixture OCI blob checksum mismatch")
        shutil.copyfile(blob, target / blob.name)
    binary = public / "fixture-server"
    run("go", "build", "-trimpath", "-o", str(binary), "./test/nixpacks", env=dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0"))
    mirror = name + "-nixpacks-mirror"
    run("docker", "run", "--rm", "--pull", "never", "--label", "cove.browser-fixture=" + name,
        "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "32m", "--cpus", ".1", "--pids-limit", "16",
        "--mount", "type=bind,source=" + str(public) + ",target=/fixture", "--entrypoint", "sh", name + "-worker:test", "-ec", "chmod -R a+rX /fixture; chmod 755 /fixture/fixture-server", stdout=subprocess.DEVNULL)
    run("docker", "run", "-d", "--pull", "never", "--name", mirror, "--label", "cove.browser-fixture=" + name,
        "--network", "bridge", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
        "--cpus", ".25", "--memory", "64m", "--pids-limit", "32", "--mount", "type=bind,source=" + str(public) + ",target=/fixture,readonly",
        "--entrypoint", "/fixture/fixture-server", name + "-worker:test", "mirror", stdout=subprocess.DEVNULL)
    address = run("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "bridge").IPAddress}}', mirror, capture_output=True, text=True).stdout.strip()
    if not re.fullmatch(r"[0-9.]+", address):
        raise RuntimeError("Nixpacks mirror did not receive its owned bridge address")
    run("docker", "exec", "--user", "0", worker, "sh", "-ec",
        'test -e /usr/local/bin/recipe-real-docker; test ! -e /usr/local/bin/nixpacks-original-docker; '
        'mv /usr/local/bin/docker /usr/local/bin/nixpacks-original-docker; '
        'rm -rf /tmp/cove-recipe-fixture/source; mkdir -p /tmp/cove-recipe-fixture/source /tmp/cove-nixpacks-fixture /var/lib/cove/build-artifacts/nixpacks-1.41.0/amd64',
        stdout=subprocess.DEVNULL)
    run("docker", "cp", str(binary), worker + ":/tmp/cove-recipe-fixture/source/fixture-server", stdout=subprocess.DEVNULL)
    run("docker", "cp", str(archive), worker + ":/var/lib/cove/build-artifacts/nixpacks-1.41.0/amd64/nixpacks-v1.41.0-x86_64-unknown-linux-musl.tar.gz", stdout=subprocess.DEVNULL)
    source = 'providers = []\n[phases.build]\ncmds = ["printf offline-build-completed > /app/build-proof"]\n[start]\ncmd = "./fixture-server browser"\n'
    config = '[registry."ghcr.io"]\n  mirrors = ["' + address + ':16000"]\n[registry."' + address + ':16000"]\n  http = true\n'
    wrapper = '#!/bin/sh\nset -eu\nif [ "$1" = buildx ] && [ "${2:-}" = create ]; then\n  exec /usr/local/bin/nixpacks-original-docker "$@" --buildkitd-config /tmp/cove-nixpacks-fixture/buildkitd.toml\nfi\nexec /usr/local/bin/nixpacks-original-docker "$@"\n'
    for path, content, mode in (("/tmp/cove-recipe-fixture/source/nixpacks.toml", source, "644"), ("/tmp/cove-nixpacks-fixture/buildkitd.toml", config, "644"), ("/usr/local/bin/docker", wrapper, "755")):
        run("docker", "exec", "-i", "--user", "0", worker, "sh", "-ec", 'cat > "$1"; chmod "$2" "$1"', "sh", path, mode, input=content.encode(), stdout=subprocess.DEVNULL)
    run("docker", "exec", "--user", "0", worker, "sh", "-ec", 'chmod -R a+rX /tmp/cove-recipe-fixture/source /tmp/cove-nixpacks-fixture; chmod 755 /tmp/cove-recipe-fixture/source/fixture-server; chown -R cove:cove /var/lib/cove/build-artifacts', stdout=subprocess.DEVNULL)
    probe = json.loads(run("docker", "exec", "--user", "cove", worker, "/usr/local/bin/cove-worker", "probe", capture_output=True, text=True).stdout)
    if not probe.get("nixpacks") or probe.get("arch") != "amd64":
        raise RuntimeError("The owned Nixpacks fixture requires its actual supported amd64 worker")
    env["COVE_BROWSER_NIXPACKS"] = "1"
    return mirror
