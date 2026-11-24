"""Exercise Cove's CNB pipeline on one owned offline daemon using verified cached artifacts."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
CACHE_SHA256 = "15aeac2619ed484662db7452582e847e0575e52a5dc8d2d3ccc76d23962f3fd7"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cnb-cache", type=Path, required=True)
    parser.add_argument("--receipt-directory", type=Path, required=True)
    options = parser.parse_args()
    options.receipt_directory.mkdir(parents=True, exist_ok=True)
    with options.cnb_cache.open("rb") as source:
        if hashlib.file_digest(source, "sha256").hexdigest() != CACHE_SHA256:
            raise RuntimeError("CNB cache does not match the verified release archive")
    owner = "cove-cnb-" + secrets.token_hex(6)
    label = "cove.cnb-fixture=" + owner
    env = dict(os.environ)
    for name in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
        env.pop(name, None)
    records, created = [], False
    with tempfile.TemporaryDirectory(prefix=owner) as temporary:
        folder = Path(temporary)
        config = folder / "docker"
        config.mkdir()
        endpoint = subprocess.check_output(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], env=env, text=True).strip()
        env.update(DOCKER_HOST=endpoint, DOCKER_CONFIG=str(config))

        def run(*args, data=None, timeout=180, check=True):
            result = subprocess.run(args, cwd=ROOT, env=env, input=data, capture_output=True, timeout=timeout)
            records.append({"command": list(args), "exit_code": result.returncode,
                            "stdout": result.stdout.decode("utf-8", "replace")[-16000:],
                            "stderr": result.stderr.decode("utf-8", "replace")[-8000:]})
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
            source = folder / "main.go"
            source.write_text('package main\nimport("net/http";"fmt")\nfunc main(){http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){fmt.Fprint(w,"cnb-offline-fixture")});panic(http.ListenAndServe(":8080",nil))}\n', encoding="utf-8", newline="\n")
            for package, name in (("./cmd/cove-worker", "cove-worker"), (str(source), "fixture-server"), ("./test/build-recipes", "policy-fixture")):
                result = subprocess.run(["go", "build", "-trimpath", "-o", str(folder / name), package], cwd=ROOT, env=build_env, capture_output=True)
                records.append({"command": ["go", "build", package], "exit_code": result.returncode})
                if result.returncode:
                    raise RuntimeError(result.stderr.decode())
            result = subprocess.run(["go", "test", "-c", "-o", str(folder / "worker-tests"), "./internal/worker"], cwd=ROOT, env=build_env, capture_output=True)
            records.append({"command": ["go", "test", "-c", "./internal/worker"], "exit_code": result.returncode})
            if result.returncode:
                raise RuntimeError(result.stderr.decode())
            # Privilege belongs only to this disposable daemon fixture, never a lifecycle phase.
            run("docker", "run", "-d", "--pull", "never", "--name", owner, "--label", label, "--privileged", "--network", "none", "--cpus", "2", "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind", "--tls=false", "--group=0")
            created = True
            for _ in range(60):
                if inside("docker", "info", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("owned daemon did not become ready")
            fixture_cache = folder / "fixtures.tar"
            with fixture_cache.open("wb") as output:
                result = subprocess.run(["docker", "save", "busybox:1.37", "registry:2.8.3"], env=env, stdout=output, stderr=subprocess.PIPE)
            records.append({"command": ["export cached fixtures"], "exit_code": result.returncode})
            if result.returncode:
                raise RuntimeError(result.stderr.decode())
            for cached in (fixture_cache, options.cnb_cache):
                with cached.open("rb") as source:
                    result = subprocess.run(["docker", "exec", "-i", owner, "docker", "load"], env=env, stdin=source, capture_output=True, timeout=360)
                records.append({"command": ["load verified cached images"], "exit_code": result.returncode})
                if result.returncode:
                    raise RuntimeError(result.stderr.decode())
            inside("mkdir", "-p", "/fixture/tools", "/fixture/source", "/var/lib/cove/docker-config", "/run/cove")
            write("/tmp/cove-worker", (folder / "cove-worker").read_bytes(), "755")
            write("/tmp/worker-tests", (folder / "worker-tests").read_bytes(), "755")
            write("/tmp/policy-fixture", (folder / "policy-fixture").read_bytes(), "755")
            write("/fixture/source/fixture-server", (folder / "fixture-server").read_bytes(), "755")
            write("/fixture/source/Procfile", b"web: ./fixture-server\n")
            write("/fixture/tools/git", b'#!/bin/sh\nset -eu\ncase "$1" in\nclone) for destination in "$@"; do :; done; mkdir -p "$destination"; cp -R /fixture/source/. "$destination/"; sync -d "$destination/fixture-server";;\nrev-parse) printf "%s\\n" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;;\n*) exit 2;;\nesac\n', "755")
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-registry", "--label", label, "-p", "127.0.0.1:15000:5000", "registry:2.8.3")
            inside("sh", "-ec", 'printf %s release-survives > /var/lib/cove/release-sentinel')
            inside("env", *worker_env, "sh", "-ec", '/tmp/cove-worker daemon > /tmp/worker.log 2>&1 &')
            for _ in range(60):
                if inside("test", "-S", "/run/cove/worker.sock", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("worker did not start")
            probe = rpc("probe")
            records.append({"live_probe": probe})
            print(json.dumps(probe), flush=True)
            operation, workload = secrets.token_hex(16), secrets.token_hex(16)
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-running", "--label", label, "--label", "com.docker.compose.project=cove-" + workload, "--label", "com.docker.compose.service=app", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "16m", "--cpus", ".25", "--pids-limit", "16", "busybox:1.37", "sleep", "86400")
            running = inside("docker", "inspect", "--format", "{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}", "fixture-running").stdout
            request = {"id": operation, "workload": workload, "kind": "service", "action": "build",
                       "source": {"repository": "https://git.fixture.invalid/app.git", "branch": "main", "inline_compose": True},
                       "compose": "services:\n  app:\n    image: busybox:1.37\n    build:\n      context: .\n      network: none\n",
                       "build_recipe": {"engine": "buildpacks", "service": "app", "directory": ".", "output": "", "build_command": "", "start_command": ""},
                       "build": {"repository": "fixture/cnb", "no_cache": True},
                       "environment": {"BUILD_TOKEN": "fixture-selected-cnb-value", "UNSELECTED_TOKEN": "fixture-unselected-cnb-value"},
                       "file_secrets": [{"name": "TOKEN", "variable": "BUILD_TOKEN", "build_services": ["app"]}],
                       "registry": {"server": "127.0.0.1:15000", "username": "fixture", "password": "fixture-not-a-real-credential"}}
            rpc("submit_build_recipe", request=request)
            for _ in range(600):
                status = rpc("status", id=operation)["operation"]
                if status["state"] not in ("queued", "running"):
                    logs = rpc("logs", id=operation).get("logs", "")
                    records.append({"operation": status, "logs": logs})
                    if status["state"] != "succeeded":
                        raise RuntimeError(json.dumps(status) + "\n" + logs[-12000:])
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("CNB operation timed out")
            limit = status["build_recipe"]["phase_memory_bytes"]
            available = status["build_recipe"]["admission_available_memory_bytes"]
            assert limit == min(4 << 30, ((available - (2 << 30)) // (1 << 20)) * (1 << 20))
            assert 2 << 30 <= limit <= 4 << 30
            assert inside("docker", "inspect", "--format", "{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}", "fixture-running").stdout == running, "build changed the existing runtime container"
            reference = status["build"]["images"][0]["reference"]
            inside("docker", "pull", reference)
            configuration = inside("docker", "image", "inspect", "--format", "{{json .Config}}", reference).stdout.decode()
            for value in request["environment"].values():
                assert value not in configuration + json.dumps(status) + logs, "fixture secret reached results, runtime configuration or logs"
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-app", "--label", label, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", reference)
            address = inside("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "bridge").IPAddress}}', "fixture-app").stdout.decode().strip()
            if not address or address == "invalid IP":
                logs = inside("docker", "logs", "fixture-app").stdout.decode("utf-8", "replace")
                raise RuntimeError("CNB runtime exited before obtaining its address: " + logs[-4000:])
            for _ in range(40):
                result = inside("wget", "-T", "2", "-qO-", "http://" + address + ":8080/", check=False)
                if result.returncode == 0:
                    assert result.stdout == b"cnb-offline-fixture"
                    break
                time.sleep(.25)
            else:
                raise RuntimeError("exported CNB application did not serve")
            inside("mkdir", "-p", "/state")
            inside("sh", "-ec", "COVE_RECIPE_FIXTURE=local-only /tmp/policy-fixture > /tmp/policy.log 2>&1 &")
            gateway = inside("docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").stdout.decode().strip()
            for _ in range(40):
                if inside("test", "-s", "/state/ca.pem", check=False).returncode == 0:
                    break
                time.sleep(.25)
            else:
                raise RuntimeError("owned policy sinkhole did not start")
            inside("ip", "addr", "add", "169.254.169.254/32", "dev", "lo")
            inside("sh", "-ec", "COVE_RECIPE_FIXTURE=local-only /tmp/policy-fixture metadata > /tmp/metadata.log 2>&1 &")
            control = ["docker", "run", "--rm", "--pull", "never", "--network", "bridge", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "32m", "--cpus", ".25", "--pids-limit", "16", "busybox:1.37", "wget", "-T", "2", "-qO-"]
            assert inside(*control, "http://" + gateway + ":2375/_ping").stdout == b"OK"
            assert inside(*control, "http://169.254.169.254/").stdout == b"owned-metadata-fixture"
            identity = inside("docker", "info", "--format", "{{.ID}}").stdout.decode().strip()
            inside("env", *worker_env, "COVE_TEST_CNB_DOCKER=1", "COVE_TEST_CNB_DAEMON_ID=" + identity, "COVE_TEST_CNB_GATEWAY=" + gateway, "COVE_TEST_CNB_PROXY=http://" + gateway + ":3129", "/tmp/worker-tests", "-test.v", "-test.run=TestBuildpacksDockerNetworkBoundary|TestBuildpacksCgroupHeadroom", "-test.timeout=2m", timeout=150)
            events = [json.loads(line) for line in inside("cat", "/state/proxy.jsonl").stdout.decode().splitlines()]
            assert events and all(event["method"] == "GET" and event["host"] == "1.1.1.1" for event in events), events
            records.append({"network_policy_sentinels": events, "metadata_and_docker_positive_controls": True, "external_network": False})
            assert inside("cat", "/var/lib/cove/release-sentinel").stdout == b"release-survives"
            for kind in ("container", "volume", "network"):
                arguments = ["docker", kind, "ls", "-q", "--filter", "label=cove.managed=build-recipe"]
                if kind == "container":
                    arguments.append("--all")
                assert not inside(*arguments).stdout.strip(), "CNB operation resource cleanup incomplete"
            records.append({"self_contained_build": True, "external_downloads": False, "phase_memory_bytes": limit, "accepted_at_4g_ceiling": limit == 4 << 30, "existing_runtime_unchanged": True})
            print("PASS: real Cove CNB lifecycle, verified OCI import/publication, application and cleanup", flush=True)
        finally:
            if created:
                current = json.loads(run("docker", "inspect", owner).stdout)[0]
                if current["Config"]["Labels"].get("cove.cnb-fixture") != owner:
                    raise RuntimeError("fixture ownership changed; cleanup refused")
                run("docker", "rm", "-f", "-v", owner)
            (options.receipt_directory / "buildpacks-native-receipt.json").write_text(json.dumps(records, indent=2) + "\n", encoding="utf-8", newline="\n")


if __name__ == "__main__":
    main()
