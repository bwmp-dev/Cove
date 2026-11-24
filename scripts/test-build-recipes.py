"""Real recipe builds using cached approved artifacts on one owned, offline Docker daemon."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import secrets
import subprocess
import tarfile
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
BUILDKIT = "docker.io/moby/buildkit@sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c"
FRONTEND = "ghcr.io/railwayapp/railpack-frontend@sha256:1f0bb3015946a77d33555756abdceb31885025763c63363ccfb27d83ad919ff9"


def verify_railpack_cache(path):
    if path.stat().st_size > 600 * 1024 * 1024:
        raise RuntimeError("approved Railpack cache exceeds its 600 MiB archive bound")
    expected = {FRONTEND.rsplit(":", 1)[-1], "99f681a26282f655ef23d0bcb7ac826116d5bd1b1ac6983be6a4937354e6ceb1", "5283b6b4d8f8eb8cb6f24964a0721e517165242f772faeb5f8600327f53f77fa"}
    seen = set()
    with tarfile.open(path) as archive:
        for count, member in enumerate(archive, 1):
            if count > 128 or member.size > 512 * 1024 * 1024 or member.issym() or member.islnk():
                raise RuntimeError("approved Railpack archive structure exceeds its bounds")
            if not member.isfile() or not member.name.startswith("blobs/sha256/"):
                continue
            digest = member.name.rsplit("/", 1)[-1]
            if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
                raise RuntimeError("invalid cached Railpack blob path")
            actual = hashlib.file_digest(archive.extractfile(member), "sha256").hexdigest()
            if digest != actual:
                raise RuntimeError("cached Railpack blob failed SHA-256 verification")
            seen.add(digest)
    if not expected <= seen:
        raise RuntimeError("cache is missing an approved release image index")
    with path.open("rb") as source:
        digest = hashlib.file_digest(source, "sha256").hexdigest()
    return {"verified_cache_sha256": digest, "bytes": path.stat().st_size, "verified_blobs": len(seen)}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--railpack-cache", type=Path, required=True)
    parser.add_argument("--receipt-directory", type=Path, required=True)
    options = parser.parse_args()
    options.receipt_directory.mkdir(parents=True, exist_ok=True)
    owner = "cove-recipes-" + secrets.token_hex(6)
    label = "cove.recipe-fixture=" + owner
    env = dict(os.environ)
    for key in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
        env.pop(key, None)
    records, created = [], False
    with tempfile.TemporaryDirectory(prefix=owner) as temporary:
        folder = Path(temporary)
        config = folder / "docker"
        config.mkdir()
        host = subprocess.check_output(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], env=env, text=True).strip()
        env.update(DOCKER_HOST=host, DOCKER_CONFIG=str(config))

        def run(*args, data=None, timeout=180, check=True):
            result = subprocess.run(args, cwd=ROOT, env=env, input=data, capture_output=True, timeout=timeout)
            records.append({"command": list(args), "exit_code": result.returncode,
                            "stdout": result.stdout.decode("utf-8", "replace")[-12000:],
                            "stderr": result.stderr.decode("utf-8", "replace")[-12000:]})
            if check and result.returncode:
                raise RuntimeError(json.dumps(records[-1]))
            return result

        def inside(*args, **kwargs):
            return run("docker", "exec", "-i", owner, *args, **kwargs)

        def write(path, content, mode="644"):
            inside("sh", "-ec", 'cat > "$1"; chmod "$2" "$1"', "sh", path, mode, data=content)

        worker_env = ["HOME=/var/lib/cove", "DOCKER_CONFIG=/var/lib/cove/docker-config", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONTEXT=", "PATH=/fixture/tools:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]

        def worker(*args, **kwargs):
            return inside("env", *worker_env, *args, **kwargs)

        def rpc(method, **values):
            response = json.loads(worker("/tmp/cove-worker", "rpc", data=json.dumps({"method": method, **values}).encode()).stdout)
            if response.get("error"):
                raise RuntimeError(response["error"])
            return response

        def build(engine, source, network="default", expected="succeeded", proxies=None, selected_secret=False):
            inside("sh", "-ec", 'rm -rf /fixture/source; mkdir -p /fixture/source')
            for name, content in source.items():
                inside("mkdir", "-p", "/fixture/source/" + str(Path(name).parent).replace("\\", "/"))
                write("/fixture/source/" + name, content.encode())
            operation, workload = secrets.token_hex(16), secrets.token_hex(16)
            recipe = {"engine": engine, "service": "app", "directory": ".", "output": "public" if engine == "static" else "", "build_command": "", "start_command": ""}
            definition = {"image": "busybox:1.37", "build": {"context": ".", "network": network, "args": proxies or {}}}
            request = {"id": operation, "workload": workload, "kind": "service", "action": "build",
                       "source": {"repository": "https://git.fixture.invalid/fixture.git", "branch": "main", "inline_compose": True},
                       "compose": json.dumps({"services": {"app": definition}}), "build_recipe": recipe,
                       "build": {"repository": "fixture/" + engine, "no_cache": True},
                       "registry": {"server": "127.0.0.1:15000", "username": "fixture", "password": "fixture-not-a-real-credential"}}
            if selected_secret:
                request["environment"] = {"BUILD_TOKEN": "fixture-selected-value", "UNSELECTED_COVE_SECRET": "fixture-unselected-value"}
                request["file_secrets"] = [{"name": "TOKEN", "variable": "BUILD_TOKEN", "build_services": ["app"]}]
            rpc("submit_build_recipe", request=request)
            for _ in range(480):
                status = rpc("status", id=operation)["operation"]
                if status["state"] not in ("queued", "running"):
                    logs = rpc("logs", id=operation).get("logs", "")
                    if status["state"] != expected:
                        raise RuntimeError(json.dumps(status) + "\n" + logs[-8000:])
                    assert status["build_recipe"]["engine"] == engine, status
                    print(engine, network, status["state"], flush=True)
                    return status, logs
                time.sleep(.5)
            raise RuntimeError("recipe fixture build timed out")

        try:
            records.append(verify_railpack_cache(options.railpack_cache))
            for image in ("docker:29-dind", "busybox:1.37", "registry:2.8.3", BUILDKIT):
                run("docker", "image", "inspect", image)
            build_env = env | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
            for package, name in (("./cmd/cove-worker", "cove-worker"), ("./test/build-recipes", "recipe-fixture")):
                result = subprocess.run(["go", "build", "-o", str(folder / name), package], cwd=ROOT, env=build_env, capture_output=True)
                records.append({"command": ["go", "build", package], "exit_code": result.returncode})
                if result.returncode:
                    raise RuntimeError(result.stderr.decode())
            result = subprocess.run(["go", "test", "-c", "-o", str(folder / "recipe-tests"), "./internal/worker"], cwd=ROOT, env=build_env, capture_output=True)
            records.append({"command": ["go", "test", "-c", "./internal/worker"], "exit_code": result.returncode})
            if result.returncode:
                raise RuntimeError(result.stderr.decode())
            run("docker", "run", "-d", "--pull", "never", "--name", owner, "--label", label, "--privileged", "--network", "none", "--cpus", "2", "--memory", "4g", "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind", "--tls=false", "--group=0")
            created = True
            for _ in range(60):
                if inside("docker", "info", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("owned daemon did not become ready")
            archive = folder / "images.tar"
            with archive.open("wb") as output:
                result = subprocess.run(["docker", "save", "busybox:1.37", "registry:2.8.3", "moby/buildkit:buildx-stable-1"], env=env, stdout=output)
            if result.returncode:
                raise RuntimeError("cached fixture export failed")
            for cached in (archive, options.railpack_cache):
                with cached.open("rb") as source:
                    result = subprocess.run(["docker", "exec", "-i", owner, "docker", "load"], env=env, stdin=source, capture_output=True)
                records.append({"command": ["load approved cached artifacts"], "exit_code": result.returncode})
                if result.returncode:
                    raise RuntimeError(result.stderr.decode())
            inside("mkdir", "-p", "/fixture/artifacts/blobs/sha256", "/fixture/state", "/fixture/tools", "/fixture/source", "/var/lib/cove/docker-config", "/run/cove")
            inside("chmod", "777", "/fixture/state")
            # Only verified OCI blob names and bytes are sent to the owned artifact server.
            with tarfile.open(options.railpack_cache) as cached:
                for member in cached:
                    if not member.isfile() or not member.name.startswith("blobs/sha256/"):
                        continue
                    digest = member.name.rsplit("/", 1)[-1]
                    if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
                        raise RuntimeError("invalid artifact blob path")
                    data = cached.extractfile(member).read()
                    if hashlib.sha256(data).hexdigest() != digest:
                        raise RuntimeError("cached artifact hash mismatch")
                    write("/fixture/artifacts/blobs/sha256/" + digest, data)
            buffer = io.BytesIO()
            with tarfile.open(fileobj=buffer, mode="w") as image:
                data = (folder / "recipe-fixture").read_bytes()
                entry = tarfile.TarInfo("fixture-server")
                entry.mode, entry.size = 0o755, len(data)
                image.addfile(entry, io.BytesIO(data))
            inside("docker", "import", "--change", 'ENTRYPOINT ["/fixture-server"]', "-", "cove-recipe-fixture:owned", data=buffer.getvalue())
            inside("docker", "network", "create", "--internal", "--label", label, "fixture-artifacts")
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-artifacts", "--label", label, "--network", "fixture-artifacts", "--network-alias", "ghcr.io", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "128m", "--cpus", ".5", "--pids-limit", "32", "-e", "COVE_RECIPE_FIXTURE=local-only", "--mount", "type=bind,src=/fixture/artifacts,dst=/artifacts,readonly", "--mount", "type=bind,src=/fixture/state,dst=/state", "cove-recipe-fixture:owned")
            for _ in range(40):
                if inside("test", "-s", "/fixture/state/ca.pem", check=False).returncode == 0:
                    break
                time.sleep(.25)
            else:
                raise RuntimeError("owned artifact fixture did not start")
            inside("docker", "network", "connect", "bridge", "fixture-artifacts")
            address = inside("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "bridge").IPAddress}}', "fixture-artifacts").stdout.decode().strip()
            write("/fixture/buildkitd.toml", b'[registry."ghcr.io"]\n  ca = ["/fixture/state/ca.pem"]\n')
            wrapper = '#!/bin/sh\nset -eu\nif [ "$1:$2" = buildx:create ]; then exec /usr/local/bin/docker "$@" --driver-opt image=' + BUILDKIT + ' --driver-opt network=fixture-artifacts --buildkitd-config /fixture/buildkitd.toml; fi\nif [ "$1:$2" = create:--pull ]; then\n for item in "$@"; do case "$item" in cove-recipe-*-egress) shift; exec /usr/local/bin/docker create --dns ' + address + ' "$@";; esac; done\nfi\nif [ "$1" = exec ] && [ "${4:-}" = build-recipe-output ] && [ "${5:-}" = exit ]; then\n case "$2" in cove-recipe-*-planner) /usr/local/bin/docker exec "$2" /bin/cat /output/info.json > /fixture/state/"$2"-info.json 2>/dev/null || :; /usr/local/bin/docker logs "${2%-planner}-egress" > /fixture/state/"$2"-relay.log 2>&1 || :;; esac\nfi\nexec /usr/local/bin/docker "$@"\n'
            write("/fixture/tools/docker", wrapper.encode(), "755")
            git = '#!/bin/sh\nset -eu\ncase "$1" in\nclone) for destination in "$@"; do :; done; mkdir -p "$destination"; cp -R /fixture/source/. "$destination/";;\nrev-parse) printf "%s\\n" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;;\n*) exit 2;;\nesac\n'
            write("/fixture/tools/git", git.encode(), "755")
            write("/tmp/cove-worker", (folder / "cove-worker").read_bytes(), "755")
            write("/tmp/recipe-tests", (folder / "recipe-tests").read_bytes(), "755")
            write("/fixture/.recipe-owner", owner.encode())
            write("/tmp/recipe-fixture", (folder / "recipe-fixture").read_bytes(), "755")
            inside("sh", "-ec", 'ip addr add 169.254.169.254/32 dev lo; COVE_RECIPE_FIXTURE=local-only /tmp/recipe-fixture metadata > /tmp/metadata.log 2>&1 &')
            gateway = inside("docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").stdout.decode().strip()
            base = ["docker", "run", "--rm", "--pull", "never", "--network", "bridge", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--entrypoint", "/bin/busybox", FRONTEND]
            inside(*base, "nc", "-w", "1", gateway, "2375")
            assert inside(*base, "wget", "-T", "2", "-qO-", "http://169.254.169.254/").stdout == b"owned-metadata-fixture", "metadata positive control failed"
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-registry", "--label", label, "-p", "127.0.0.1:15000:5000", "registry:2.8.3")
            inside("sh", "-ec", 'printf %s release-survives > /var/lib/cove/release-sentinel')
            inside("env", *worker_env, "sh", "-ec", '/tmp/cove-worker daemon > /tmp/worker.log 2>&1 &')
            for _ in range(60):
                if inside("test", "-S", "/run/cove/worker.sock", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("worker did not start")
            static, _ = build("static", {"public/index.html": "static-fixture", "public/.env": "fixture-private", "private.txt": "fixture-private", "Dockerfile": "FROM unsupported.invalid/never:latest\n"})
            reference = static["build"]["images"][0]["reference"]
            inside("docker", "pull", reference)
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-static", "--label", label, "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", reference)
            static_address = inside("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "bridge").IPAddress}}', "fixture-static").stdout.decode().strip()
            assert inside("wget", "-qO-", "http://" + static_address + ":8080/").stdout == b"static-fixture"
            for path in ("/.env", "/private.txt", "/../private.txt"):
                assert inside("wget", "-qO-", "http://" + static_address + ":8080" + path, check=False).returncode != 0
            shell = {"start.sh": "#!/bin/sh\ncat /app/engine-marker\n", "railpack.json": json.dumps({"provider": "shell", "steps": {"build": {"commands": ['test -n "$TOKEN" && test -z "${UNSELECTED_COVE_SECRET:-}" && printf built-on-railpack > /app/engine-marker']}}}), "Dockerfile": "FROM unsupported.invalid/never:latest\n"}
            railpack, railpack_logs = build("railpack", shell, selected_secret=True)
            reference = railpack["build"]["images"][0]["reference"]
            inside("docker", "pull", reference)
            configuration = inside("docker", "image", "inspect", reference).stdout.decode()
            assert "fixture-selected-value" not in configuration + railpack_logs and "fixture-unselected-value" not in configuration + railpack_logs, "secret values escaped build mounts"
            assert inside("docker", "run", "--rm", "--pull", "never", "--network", "none", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", reference).stdout.strip() == b"built-on-railpack"
            for policy in ({"network": "none"}, {"args": {"HTTPS_PROXY": "http://" + address + ":3129"}}):
                operation = secrets.token_hex(16)
                request = {"id": operation, "workload": secrets.token_hex(16), "action": "build", "kind": "service",
                           "source": {"repository": "https://git.fixture.invalid/fixture.git", "branch": "main", "inline_compose": True},
                           "compose": json.dumps({"services": {"app": {"image": "busybox:1.37", "build": {"context": ".", **policy}}}}),
                           "build_recipe": {"engine": "railpack", "service": "app", "directory": ".", "output": "", "build_command": "", "start_command": ""},
                           "build": {"repository": "fixture/refused"}, "registry": {"server": "127.0.0.1:15000", "username": "fixture", "password": "fixture-placeholder"}}
                refusal = json.loads(worker("/tmp/cove-worker", "rpc", data=json.dumps({"method": "submit_build_recipe", "request": request}).encode()).stdout)
                assert "pinned Railpack frontend does not apply" in refusal.get("error", ""), refusal
                assert inside("test", "-e", "/var/lib/cove/operations/" + operation + ".json", check=False).returncode != 0
            identity = inside("docker", "info", "--format", "{{.ID}}").stdout.decode().strip()
            tested = inside("env", *worker_env, "SSL_CERT_FILE=/fixture/state/ca.pem", "COVE_TEST_RECIPE_DOCKER=1", "COVE_TEST_RECIPE_DAEMON_ID=" + identity, "COVE_TEST_RECIPE_OWNER=" + owner,
                            "COVE_TEST_RECIPE_PROXY=http://" + address + ":3129", "/tmp/recipe-tests", "-test.run", "^TestRailpackPlannerDockerPolicy$", "-test.v", "-test.timeout", "5m", timeout=320)
            print(tested.stdout.decode(), flush=True)
            diagnostic = inside("sh", "-ec", 'for p in /fixture/state/*-info.json /fixture/state/*-relay.log; do test ! -f "$p" || { printf "%s\\n" "$p"; tail -c 4000 "$p"; }; done').stdout.decode()
            print(diagnostic, flush=True)
            events = [json.loads(line) for line in inside("cat", "/fixture/state/proxy.jsonl").stdout.splitlines()]
            assert events and all(event["method"] in ("CONNECT", "GET", "HEAD") for event in events), events
            records.append({"metadata_sinkhole_events": events, "external_downloads": False})
            artifact_address = inside("docker", "inspect", "--format", '{{(index .NetworkSettings.Networks "fixture-artifacts").IPAddress}}', "fixture-artifacts").stdout.decode().strip()
            build_owner = json.loads(inside("cat", "/var/lib/cove/build-owner.json").stdout)
            assert isinstance(build_owner, str) and len(build_owner) == 32
            inside("mkdir", "-p", "/fixture/network-policy")
            for mode in ("default", "none", "proxy"):
                script = ('exec 3<>/dev/tcp/' + artifact_address + '/3129; '
                          'printf "GET /build-network-' + mode + ' HTTP/1.1\\r\\nHost: fixture\\r\\nConnection: close\\r\\n\\r\\n" >&3; '
                          'head -n 1 <&3 > /app/network-proof; printf "%s" "${HTTP_PROXY:-unset}" > /app/proxy-proof')
                plan = {"steps": [{"name": "build", "inputs": [{"image": "ghcr.io/railwayapp/railpack-builder@sha256:99f681a26282f655ef23d0bcb7ac826116d5bd1b1ac6983be6a4937354e6ceb1"}],
                                   "commands": [{"cmd": "bash -c '" + script + "'"}]}],
                        "deploy": {"base": {"image": "ghcr.io/railwayapp/railpack-runtime@sha256:5283b6b4d8f8eb8cb6f24964a0721e517165242f772faeb5f8600327f53f77fa"}, "inputs": [{"step": "build", "include": ["."]}]}}
                write("/fixture/network-policy/plan.json", json.dumps(plan).encode())
                arguments = {"BUILDKIT_SYNTAX": FRONTEND, "cache-key": "fixture-upstream-" + mode}
                if mode == "proxy":
                    arguments["HTTP_PROXY"] = "http://unreachable.fixture.invalid:9999"
                compose = {"services": {"probe": {"image": "cove-policy-probe:" + mode,
                                                   "build": {"context": "/fixture/network-policy", "dockerfile": "plan.json", "network": "none" if mode == "none" else "default", "args": arguments, "labels": {"cove.recipe-fixture": owner}}}}}
                write("/fixture/network-policy/compose.json", json.dumps(compose).encode())
                inside("env", *worker_env, "BUILDX_CONFIG=/var/lib/cove/buildx", "docker", "compose", "--project-name", "fixture-policy-probe", "--file", "/fixture/network-policy/compose.json", "build", "--builder", "cove-" + build_owner, "--no-cache")
                proof = inside("docker", "run", "--rm", "--pull", "never", "--network", "none", "--entrypoint", "/bin/cat", "cove-policy-probe:" + mode, "/app/network-proof").stdout
                assert proof.startswith(b"HTTP/1.1 502"), "pinned upstream network behavior changed; deliberately revisit Cove's refusal"
                if mode == "proxy":
                    assert inside("docker", "run", "--rm", "--pull", "never", "--network", "none", "--entrypoint", "/bin/cat", "cove-policy-probe:" + mode, "/app/proxy-proof").stdout == b"unset", "upstream proxy behavior changed; revisit the refusal"
            records.append({"pinned_frontend_network_none_ignored": True, "pinned_frontend_proxy_ignored": True, "product_refuses_both": True})
            assert inside("cat", "/var/lib/cove/release-sentinel").stdout == b"release-survives"
            for kind in ("container", "volume", "network"):
                args = ["docker", kind, "ls", "-q", "--filter", "label=cove.managed=build-recipe"]
                if kind == "container":
                    args.append("--all")
                assert not inside(*args).stdout.strip(), "owned recipe resource cleanup incomplete"
            print("PASS: static runtime, real self-contained Railpack build, admission refusals and isolated planner metadata tests; no external downloads", flush=True)
        finally:
            if created:
                current = json.loads(run("docker", "inspect", owner).stdout)[0]
                if current["Config"]["Labels"].get("cove.recipe-fixture") != owner:
                    raise RuntimeError("fixture ownership changed; cleanup refused")
                run("docker", "rm", "-f", "-v", owner)
            (options.receipt_directory / "build-recipes-native-receipt.json").write_text(json.dumps(records, indent=2) + "\n", encoding="utf-8", newline="\n")


if __name__ == "__main__":
    main()
