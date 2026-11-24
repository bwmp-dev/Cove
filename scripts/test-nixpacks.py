"""Run Cove's Nixpacks path on an owned disconnected daemon with verified cached artifacts."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
BASE_PIN = "ghcr.io/railwayapp/nixpacks@sha256:d45c89d80e13d7ad0fd555b5130f22a866d9dd10e861f589932303ef2314c7de"
BASE_CACHE_SHA256 = "8a58b6e56faa9420aad672845a6d7385bd110d5b34013f9a5d4a5793e49c4689"
PLANNER_SHA256 = "0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-cache", type=Path, required=True)
    parser.add_argument("--planner-cache", type=Path, required=True)
    parser.add_argument("--blob-cache", type=Path, required=True)
    parser.add_argument("--receipt-directory", type=Path, required=True)
    options = parser.parse_args()
    options.receipt_directory.mkdir(parents=True, exist_ok=True)
    for path, expected in ((options.base_cache, BASE_CACHE_SHA256), (options.planner_cache, PLANNER_SHA256)):
        with path.open("rb") as source:
            if hashlib.file_digest(source, "sha256").hexdigest() != expected:
                raise RuntimeError("cached Nixpacks artifact checksum mismatch")
    for path in options.blob_cache.iterdir():
        if path.is_symlink() or not path.is_file() or not re.fullmatch("[0-9a-f]{64}", path.name) or path.stat().st_size > 200 << 20:
            raise RuntimeError("unexpected cached OCI blob")
        with path.open("rb") as source:
            if hashlib.file_digest(source, "sha256").hexdigest() != path.name:
                raise RuntimeError("cached OCI blob checksum mismatch")
    owner = "cove-nixpacks-" + secrets.token_hex(6)
    label = "cove.nixpacks-fixture=" + owner
    env = dict(os.environ)
    for name in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
        env.pop(name, None)
    records, created, private_values = [], False, []
    with tempfile.TemporaryDirectory(prefix=owner) as temporary:
        folder = Path(temporary)
        config = folder / "docker"
        config.mkdir()
        env.update(DOCKER_HOST=subprocess.check_output(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], env=env, text=True).strip(), DOCKER_CONFIG=str(config))

        def safe(data):
            text = data.decode("utf-8", "replace")
            for value in private_values:
                text = text.replace(value, "[fixture-redacted]")
            return text

        def run(*args, data=None, timeout=180, check=True):
            result = subprocess.run(args, cwd=ROOT, env=env, input=data, capture_output=True, timeout=timeout)
            records.append({"command": list(args), "exit_code": result.returncode, "stdout": safe(result.stdout)[-16000:], "stderr": safe(result.stderr)[-8000:]})
            if check and result.returncode:
                raise RuntimeError(json.dumps(records[-1]))
            return result

        def inside(*args, **kwargs):
            return run("docker", "exec", "-i", owner, *args, **kwargs)

        def write(path, content, mode="644"):
            inside("sh", "-ec", 'cat > "$1"; chmod "$2" "$1"', "sh", path, mode, data=content)

        worker_env = ["HOME=/var/lib/cove", "DOCKER_CONFIG=/var/lib/cove/docker-config", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONTEXT=", "PATH=/fixture/tools:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]

        def rpc(method, **values):
            result = inside("env", *worker_env, "/tmp/cove-worker", "rpc", data=json.dumps({"method": method, **values}).encode())
            response = json.loads(result.stdout)
            if response.get("error"):
                raise RuntimeError(response["error"])
            return response

        try:
            build_env = env | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
            for package, name in (("./cmd/cove-worker", "cove-worker"), ("./test/nixpacks", "fixture-server")):
                result = subprocess.run(["go", "build", "-trimpath", "-o", str(folder / name), package], cwd=ROOT, env=build_env, capture_output=True)
                records.append({"command": ["go", "build", package], "exit_code": result.returncode})
                if result.returncode:
                    raise RuntimeError(result.stderr.decode())
            result = subprocess.run(["go", "test", "-c", "-o", str(folder / "worker-tests"), "./internal/worker"], cwd=ROOT, env=build_env, capture_output=True)
            records.append({"command": ["go", "test", "-c", "./internal/worker"], "exit_code": result.returncode})
            if result.returncode:
                raise RuntimeError(result.stderr.decode())
            # Only the disposable daemon has fixture privileges; neither planner nor build steps receive its socket.
            run("docker", "run", "-d", "--pull", "never", "--name", owner, "--label", label, "--privileged", "--network", "none", "--cpus", "2", "--memory", "2g", "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind", "--tls=false", "--group=0")
            created = True
            for _ in range(60):
                if inside("docker", "info", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("owned daemon did not become ready")
            fixture_cache = folder / "fixtures.tar"
            with fixture_cache.open("wb") as output:
                result = subprocess.run(["docker", "save", "busybox:1.37", "registry:2.8.3", "moby/buildkit:buildx-stable-1"], env=env, stdout=output, stderr=subprocess.PIPE)
            records.append({"command": ["export cached fixtures"], "exit_code": result.returncode})
            if result.returncode:
                raise RuntimeError(result.stderr.decode())
            for cached in (fixture_cache, options.base_cache):
                with cached.open("rb") as source:
                    result = subprocess.run(["docker", "exec", "-i", owner, "docker", "load", "--platform", "linux/amd64"], env=env, stdin=source, capture_output=True, timeout=180)
                records.append({"command": ["load verified cached images"], "exit_code": result.returncode})
                if result.returncode:
                    raise RuntimeError(result.stderr.decode())
            inside("mkdir", "-p", "/fixture/tools", "/fixture/source", "/var/lib/cove/docker-config", "/var/lib/cove/buildx", "/var/lib/cove/build-artifacts/nixpacks-1.41.0/amd64", "/run/cove")
            run("docker", "cp", str(options.blob_cache), owner + ":/fixture/base")
            write("/tmp/cove-worker", (folder / "cove-worker").read_bytes(), "755")
            write("/tmp/worker-tests", (folder / "worker-tests").read_bytes(), "755")
            write("/tmp/fixture-server", (folder / "fixture-server").read_bytes(), "755")
            write("/fixture/source/fixture-server", (folder / "fixture-server").read_bytes(), "755")
            write("/fixture/.nixpacks-owner", owner.encode(), "600")
            write("/var/lib/cove/build-artifacts/nixpacks-1.41.0/amd64/nixpacks-v1.41.0-x86_64-unknown-linux-musl.tar.gz", options.planner_cache.read_bytes(), "600")
            selected, unselected = "fixture-selected-" + secrets.token_hex(16), "fixture-unselected-" + secrets.token_hex(16)
            private_values.extend((selected, unselected))
            expected_hash = hashlib.sha256(selected.encode()).hexdigest()
            write("/fixture/source/nixpacks.toml", ('providers = []\n[phases.build]\ncmds = ["./fixture-server build-secret-check ' + expected_hash + '"]\n[start]\ncmd = "./fixture-server"\n').encode())
            write("/fixture/tools/git", b'#!/bin/sh\nset -eu\ncase "$1" in\nclone) for destination in "$@"; do :; done; mkdir -p "$destination"; cp -R /fixture/source/. "$destination/";;\nrev-parse) printf "%s\\n" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;;\n*) exit 2;;\nesac\n', "755")
            inside("sh", "-ec", '/tmp/fixture-server mirror > /tmp/mirror.log 2>&1 &')
            gateway = inside("docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").stdout.decode().strip()
            for _ in range(40):
                if inside("wget", "-T", "2", "-qO-", "http://" + gateway + ":16000/v2/", check=False).returncode == 0:
                    break
                time.sleep(.25)
            else:
                raise RuntimeError("owned OCI mirror did not start")
            owner_id = secrets.token_hex(16)
            write("/var/lib/cove/build-owner.json", json.dumps(owner_id).encode(), "600")
            write("/fixture/buildkit.toml", ('[registry."ghcr.io"]\n  mirrors = ["' + gateway + ':16000"]\n[registry."' + gateway + ':16000"]\n  http = true\n').encode())
            builder_image = inside("docker", "image", "inspect", "--format", "{{.Id}}", "moby/buildkit:buildx-stable-1").stdout.decode().strip()
            inside("env", *worker_env, "BUILDX_CONFIG=/var/lib/cove/buildx", "docker", "buildx", "create", "--name", "cove-" + owner_id, "--driver", "docker-container", "--driver-opt", "default-load=true", "--driver-opt", "image=" + builder_image, "--buildkitd-config", "/fixture/buildkit.toml")
            write("/var/lib/cove/builder-ready.json", b"true", "600")
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-registry", "--label", label, "-p", "127.0.0.1:15000:5000", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "--tmpfs", "/var/lib/registry:size=256m", "registry:2.8.3")
            inside("sh", "-ec", 'printf %s release-survives > /var/lib/cove/release-sentinel')
            inside("env", *worker_env, "sh", "-ec", '/tmp/cove-worker daemon > /tmp/worker.log 2>&1 &')
            for _ in range(60):
                if inside("test", "-S", "/run/cove/worker.sock", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("worker did not start")
            probe = rpc("probe")["probe"]
            assert probe["nixpacks"] is True
            operation, workload = secrets.token_hex(16), secrets.token_hex(16)
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-running", "--label", label, "--network", "none", "--read-only", "--cap-drop", "ALL", "--memory", "16m", "--cpus", ".25", "--pids-limit", "16", "busybox:1.37", "sleep", "86400")
            running = inside("docker", "inspect", "--format", "{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}", "fixture-running").stdout
            request = {"id": operation, "workload": workload, "kind": "service", "action": "build", "source": {"repository": "https://git.fixture.invalid/app.git", "branch": "main", "inline_compose": True}, "compose": "services:\n  app:\n    image: busybox:1.37\n    build:\n      context: .\n      network: none\n", "build_recipe": {"engine": "nixpacks", "service": "app", "directory": ".", "output": "", "build_command": "", "start_command": ""}, "build": {"repository": "fixture/nixpacks", "no_cache": True}, "environment": {"BUILD_TOKEN": selected, "UNSELECTED_TOKEN": unselected}, "file_secrets": [{"name": "TOKEN", "variable": "BUILD_TOKEN", "build_services": ["app"]}], "registry": {"server": "127.0.0.1:15000", "username": "fixture", "password": "fixture-not-a-real-credential"}, "build_platform": "linux/amd64"}
            rpc("submit_build_recipe", request=request)
            for _ in range(600):
                status = rpc("status", id=operation)["operation"]
                if status["state"] not in ("queued", "running"):
                    logs = rpc("logs", id=operation).get("logs", "")
                    if any(value in logs + json.dumps(status) for value in private_values):
                        raise RuntimeError("fixture secret reached public operation data")
                    records.append({"operation": status, "logs": logs})
                    if status["state"] != "succeeded":
                        raise RuntimeError(json.dumps(status) + "\n" + logs[-12000:])
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("Nixpacks operation timed out")
            assert status["build_recipe"]["engine"] == "nixpacks" and status["build_recipe"]["planner_sha256"] == "sha256:" + PLANNER_SHA256
            assert inside("docker", "inspect", "--format", "{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}", "fixture-running").stdout == running
            reference = status["build"]["images"][0]["reference"]
            inside("docker", "pull", reference)
            configuration = inside("docker", "image", "inspect", "--format", "{{json .Config}}", reference).stdout.decode()
            assert all(value not in configuration for value in private_values)
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-app", "--label", label, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", reference)
            address = inside("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "bridge").IPAddress}}', "fixture-app").stdout.decode().strip()
            for _ in range(40):
                result = inside("wget", "-T", "2", "-qO-", "http://" + address + ":8080/", check=False)
                if result.returncode == 0:
                    assert result.stdout == b"nixpacks-offline-fixture"
                    break
                time.sleep(.25)
            else:
                raise RuntimeError("exported Nixpacks application did not serve")
            assert inside("cat", "/var/lib/cove/release-sentinel").stdout == b"release-survives"
            identity = inside("docker", "info", "--format", "{{.ID}}").stdout.decode().strip()
            boundary = inside("env", *worker_env, "COVE_TEST_NIXPACKS_DOCKER=1", "COVE_TEST_NIXPACKS_DAEMON_ID=" + identity, "COVE_TEST_NIXPACKS_OWNER=" + owner, "COVE_TEST_NIXPACKS_GATEWAY=" + gateway, "/tmp/worker-tests", "-test.v", "-test.run=TestNixpacks", "-test.timeout=5m", timeout=330)
            assert b"proxy-bypass-control" in boundary.stdout and b"PASS" in boundary.stdout
            for kind in ("container", "volume", "network"):
                arguments = ["docker", kind, "ls", "-q", "--filter", "label=cove.managed=build-recipe"]
                if kind == "container":
                    arguments.append("--all")
                assert not inside(*arguments).stdout.strip(), "Nixpacks operation resource cleanup incomplete"
            records.append({"self_contained_build": True, "external_downloads": False, "selected_secret_read_during_build": True, "unselected_secret_unavailable": True, "existing_runtime_unchanged": True, "pinned_base_fixture_mirror": True, "real_network_none_sentinel_passed": True, "proxy_bypass_control_proves_product_refusal_needed": True})
            print("PASS: real Cove Nixpacks generation, validated pinned BuildKit build, selected secret read, immutable publication, runtime and cleanup", flush=True)
        finally:
            if created:
                current = json.loads(run("docker", "inspect", owner).stdout)[0]
                if current["Config"]["Labels"].get("cove.nixpacks-fixture") != owner:
                    raise RuntimeError("fixture ownership changed; cleanup refused")
                run("docker", "rm", "-f", "-v", owner)
            (options.receipt_directory / "nixpacks-native-receipt.json").write_text(json.dumps(records, indent=2) + "\n", encoding="utf-8", newline="\n")


if __name__ == "__main__":
    main()
