"""Exercise the real worker on a fixture-created bounded filesystem, without host mounts."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
PIN = "docker.io/moby/buildkit@sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c"


def main():
    owner = "cove-cache-test-" + secrets.token_hex(6)
    label = "cove.build-cache-fixture=" + owner
    env = os.environ.copy()
    for key in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
        env.pop(key, None)
    records = []
    created = False
    output = Path(os.environ.get("COVE_TEST_RECEIPT_DIR", tempfile.gettempdir())).resolve()
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=owner) as temporary:
        folder = Path(temporary)
        config = folder / "docker"
        config.mkdir()
        host = subprocess.check_output(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], env=env, text=True).strip()
        env.update(DOCKER_HOST=host, DOCKER_CONFIG=str(config))

        def run(*args, data=None, check=True, timeout=120):
            result = subprocess.run(args, cwd=ROOT, env=env, input=data, capture_output=True, timeout=timeout)
            records.append({"command": list(args), "exit_code": result.returncode,
                            "stdout": result.stdout.decode("utf-8", "replace")[-16000:],
                            "stderr": result.stderr.decode("utf-8", "replace")[-16000:]})
            if check and result.returncode:
                raise RuntimeError(json.dumps(records[-1]))
            return result

        def inside(*args, **kwargs):
            return run("docker", "exec", "-i", owner, *args, **kwargs)

        worker_env = ["HOME=/var/lib/cove", "DOCKER_CONFIG=/var/lib/cove/docker-config", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONTEXT="]

        def worker(*args, **kwargs):
            return run("docker", "exec", "-i", "-u", "65534:0", owner, "env", *worker_env, *args, **kwargs)

        def rpc(method, **values):
            result = worker("/tmp/cove-worker", "rpc", data=json.dumps({"method": method, **values}).encode())
            value = json.loads(result.stdout)
            if value.get("error"):
                raise RuntimeError(value["error"])
            return value

        def configure(path):
            for _ in range(40):
                try:
                    return rpc("build_cache_configure", build_cache={"path": path})["build_cache"]
                except RuntimeError as error:
                    if str(error) != "wait for the active build or cache cleanup before configuring the build cache":
                        raise
                    time.sleep(.5)
            raise RuntimeError("startup cache cleanup did not release the build mutex")

        def start():
            run("docker", "exec", "-d", "-u", "65534:0", owner, "env", *worker_env, "sh", "-ec", 'echo $$ > /run/cove/fixture-worker.pid; exec /tmp/cove-worker daemon > /tmp/worker.log 2>&1')
            for _ in range(60):
                if inside("test", "-S", "/run/cove/worker.sock", check=False).returncode == 0:
                    try:
                        status = rpc("build_cache")["build_cache"]
                        if not status["cleanup_pending"]:
                            return status
                    except RuntimeError:
                        pass
                time.sleep(.5)
            raise RuntimeError("fixture worker did not become ready")

        def stop():
            inside("sh", "-ec", 'p=$(cat /run/cove/fixture-worker.pid); test "$(readlink /proc/$p/exe)" = /tmp/cove-worker; kill "$p"; for n in $(seq 1 40); do test ! -e /proc/$p && break; sleep .1; done; rm -f /run/cove/worker.sock')

        def checkpoint(prune=False, retire=False):
            state = json.loads(inside("cat", "/var/lib/cove/build-cache.json").stdout)
            if prune:
                state["current"]["prune"] = True
            if retire:
                state["retired"] = [state["current"]]
                state["current"] = None
            worker("sh", "-ec", 'cat > /var/lib/cove/build-cache.json', data=json.dumps(state).encode())

        workload = secrets.token_hex(16)
        directory = "/var/lib/cove/workloads/" + workload
        mount = "/cove-cache-fixture"
        registry = "127.0.0.1:15000"

        def build(expected="succeeded"):
            operation = secrets.token_hex(16)
            request = {"id": operation, "workload": workload, "kind": "service", "action": "build",
                       "compose": "services:\n  app:\n    build: .\n", "build": {"repository": "fixture/app", "no_cache": True},
                       "registry": {"server": registry, "username": "fixture", "password": "fixture-only-not-a-real-credential"}}
            rpc("submit", request=request)
            for _ in range(240):
                status = rpc("status", id=operation)["operation"]
                if status["state"] not in ("queued", "running"):
                    logs = rpc("logs", id=operation).get("logs", "")
                    print(status["state"], status.get("error", ""), flush=True)
                    if status["state"] != expected:
                        ids = inside("docker", "ps", "-aq", "--filter", "label=cove.managed=build-cache").stdout.split()
                        if ids:
                            metadata = inside("docker", "inspect", "--format", "{{json .HostConfig.SecurityOpt}} {{json .HostConfig.Tmpfs}}", *[value.decode() for value in ids]).stdout.decode()
                            logs += "\nOwned cache launch metadata: " + metadata
                        raise RuntimeError(json.dumps(status) + "\n" + logs)
                    return status, logs
                time.sleep(.5)
            raise RuntimeError("fixture build exceeded 120 seconds")

        try:
            for image in ("docker:29-dind", "busybox:1.37", "registry:2.8.3", PIN):
                run("docker", "image", "inspect", image)
            binary = folder / "cove-worker"
            build_env = env | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
            compiled = subprocess.run(["go", "build", "-o", str(binary), "./cmd/cove-worker"], cwd=ROOT, env=build_env)
            records.append({"command": ["go", "build", "./cmd/cove-worker"], "exit_code": compiled.returncode})
            if compiled.returncode:
                raise RuntimeError("worker build failed")
            tests = folder / "cache-tests"
            compiled = subprocess.run(["go", "test", "-c", "-o", str(tests), "./internal/worker"], cwd=ROOT, env=build_env)
            records.append({"command": ["go", "test", "-c", "./internal/worker"], "exit_code": compiled.returncode})
            if compiled.returncode:
                raise RuntimeError("Linux cache test build failed")
            run("docker", "run", "-d", "--pull", "never", "--name", owner, "--label", label, "--privileged", "--network", "none",
                "--cpus", "2", "--memory", "2g", "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind", "--tls=false", "--group=0")
            created = True
            for _ in range(60):
                if inside("docker", "info", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("owned Docker daemon did not become ready")
            archive = folder / "images.tar"
            with archive.open("wb") as destination:
                saved = subprocess.run(["docker", "save", "busybox:1.37", "registry:2.8.3", "moby/buildkit:buildx-stable-1"], env=env, stdout=destination)
            if saved.returncode:
                raise RuntimeError("cached artifact export failed")
            with archive.open("rb") as source:
                loaded = subprocess.run(["docker", "exec", "-i", owner, "docker", "load"], env=env, stdin=source, capture_output=True)
            records.append({"command": ["cached images load"], "exit_code": loaded.returncode})
            if loaded.returncode:
                raise RuntimeError(loaded.stderr.decode())
            inside("docker", "image", "inspect", PIN)
            inside("sh", "-ec", 'cat > /tmp/cove-worker; chmod 755 /tmp/cove-worker', data=binary.read_bytes())
            inside("sh", "-ec", 'cat > /tmp/cache-tests; chmod 755 /tmp/cache-tests', data=tests.read_bytes())
            inside("sh", "-ec", 'mkdir -p /var/lib/cove/docker-config /run/cove "$1" "$2"; mount -t tmpfs -o size=64m,mode=0700 tmpfs "$1"; chown -R nobody:root /var/lib/cove /run/cove "$1"; chmod 777 /var/run/docker.sock', "sh", mount, directory)
            worker("/tmp/cache-tests", "-test.run", "^TestBuildCache", "-test.v")
            inside("docker", "volume", "create", "--label", label, "cache-outside-registry")
            inside("docker", "run", "-d", "--pull", "never", "--name", "fixture-registry", "--label", label, "-p", "127.0.0.1:15000:5000",
                   "--mount", "type=volume,src=cache-outside-registry,dst=/var/lib/registry", "registry:2.8.3")
            worker("sh", "-ec", 'printf "FROM scratch\nCOPY marker /artifact\n" > "$1/Dockerfile"; printf %s cache-fixture > "$1/marker"; printf "*\n!Dockerfile\n!marker\n!payload\n" > "$1/.dockerignore"; printf %s release-survives > /var/lib/cove/release-sentinel', "sh", directory)
            status = start()
            assert not status["configured"] and not status["verified"] and status["capacity_bytes"] == 0
            negative = worker("/tmp/cove-worker", "rpc", data=json.dumps({"method": "build_cache_configure", "build_cache": {"path": directory}}).encode())
            assert json.loads(negative.stdout).get("error"), "ordinary directory accepted"
            stop()
            inside("sh", "-ec", 'unshare -m sh -ec \'mount --make-rprivate /; umount /cove-cache-fixture; mount -t tmpfs -o size=64m,mode=0700 tmpfs /cove-cache-fixture; chown nobody:root /cove-cache-fixture; exec su -s /bin/sh nobody -c "echo \\$\\$ > /run/cove/fixture-worker.pid; exec env HOME=/var/lib/cove DOCKER_CONFIG=/var/lib/cove/docker-config DOCKER_HOST=unix:///var/run/docker.sock DOCKER_CONTEXT= /tmp/cove-worker daemon"\' > /tmp/namespace-worker.log 2>&1 &')
            for _ in range(60):
                if inside("test", "-S", "/run/cove/worker.sock", check=False).returncode == 0:
                    break
                time.sleep(.5)
            else:
                raise RuntimeError("separate mount namespace worker did not start")
            negative = worker("/tmp/cove-worker", "rpc", data=json.dumps({"method": "build_cache_configure", "build_cache": {"path": mount}}).encode())
            assert "do not see the same" in json.loads(negative.stdout).get("error", ""), negative.stdout
            stop()
            start()
            status = configure(mount)
            assert status["verified"] and status["capacity_bytes"] == 64*1024*1024, status
            first, _ = build()
            reference = first["build"]["images"][0]["reference"]
            inside("docker", "pull", reference)
            image = inside("docker", "image", "inspect", "--format", "{{.Id}}", reference).stdout.strip()
            worker("sh", "-ec", 'dd if=/dev/urandom of="$1/payload" bs=1M count=80 2>/dev/null; printf "FROM scratch\nCOPY payload /payload\n" > "$1/Dockerfile"', "sh", directory)
            failed, logs = build("failed")
            assert "cache" in failed.get("error", "").lower() and "full" in failed["error"].lower(), failed
            assert "no space left on device" in logs.lower(), logs[-4000:]
            status = rpc("build_cache")["build_cache"]
            assert status["verified"] and not status["cleanup_pending"] and status["available_bytes"] > 0, status
            assert inside("docker", "image", "inspect", "--format", "{{.Id}}", reference).stdout.strip() == image
            inside("docker", "image", "inspect", "busybox:1.37")
            inside("docker", "pull", reference)
            assert inside("cat", "/var/lib/cove/release-sentinel").stdout == b"release-survives"
            worker("sh", "-ec", 'rm "$1/payload"; printf "FROM scratch\nCOPY marker /artifact\n" > "$1/Dockerfile"', "sh", directory)
            build()
            stop()
            checkpoint(prune=True)
            status = start()
            assert status["verified"] and not status["cleanup_pending"], status
            stop()
            inside("mount", "-t", "tmpfs", "-o", "remount,size=96m", "tmpfs", mount)
            status = start()
            assert not status["verified"] and "resized" in status["reason"], status
            rejected, _ = build("failed")
            assert not inside("test", "-e", "/var/lib/cove/builder-ready.json", check=False).returncode == 0, "uncapped fallback created"
            stop()
            inside("mount", "-t", "tmpfs", "-o", "remount,size=64m", "tmpfs", mount)
            assert start()["verified"]
            assert not configure("")["configured"]
            inside("test", "-f", mount + "/.cove-build-cache-owner.json")
            assert configure(mount)["verified"]
            build()
            stop()
            checkpoint(retire=True)
            status = start()
            assert not status["configured"] and not status["cleanup_pending"], status
            inside("test", "-f", mount + "/.cove-build-cache-owner.json")
            assert configure(mount)["verified"]
            stop()
            inside("umount", mount)
            status = start()
            assert not status["verified"] and "dedicated filesystem" in status["reason"], status
            stop()
            inside("mount", "-t", "tmpfs", "-o", "size=64m,mode=0700", "tmpfs", mount)
            inside("chown", "nobody:root", mount)
            status = start()
            assert not status["verified"] and "replaced" in status["reason"], status
            assert inside("test", "-e", mount + "/.cove-build-cache-owner.json", check=False).returncode != 0
            configure("")
            stop()
            print("PASS: unprivileged verification, real exhaustion/prune/retry, restart and negative mounts; outside data survived", flush=True)
        finally:
            if created:
                info = json.loads(run("docker", "inspect", owner).stdout)[0]
                if info["Config"]["Labels"].get("cove.build-cache-fixture") != owner:
                    raise RuntimeError("owned fixture identity changed; cleanup refused")
                run("docker", "rm", "-f", "-v", owner)
            (output / "build-cache-native-receipt.json").write_text(json.dumps(records, indent=2) + "\n", encoding="utf-8", newline="\n")


if __name__ == "__main__":
    main()
