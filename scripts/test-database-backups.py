"""Run logical database backup acceptance in disposable Docker containers."""
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def main():
    name = "cove-db-test-" + secrets.token_hex(5)
    remote = "/tmp/" + name
    isolated_registry = len(sys.argv)>1 and sys.argv[1] == "TestPromotionRegistryRoundTrip"
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        binary = Path(directory) / "worker.test"
        run("go", "test", "-c", "-o", str(binary), "./internal/worker",
            env=os.environ | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"})
        if isolated_registry:
            run("docker", "network", "create", name, stdout=subprocess.DEVNULL)
            run("docker", "run", "-d", "--privileged", "--name", name+"-engine", "--network", name,
                "--network-alias", "engine", "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind",
                "--insecure-registry=127.0.0.1:5000", stdout=subprocess.DEVNULL)
        options = ["--network", name, "-e", "DOCKER_HOST=tcp://engine:2375"] if isolated_registry else ["-v", "/var/run/docker.sock:/var/run/docker.sock"]
        run("docker", "run", "-d", "--name", name, "--label", "cove.test=true",
            "-v", "/tmp:/tmp", *options,
            "docker:29-cli", "sleep", "1200", stdout=subprocess.DEVNULL)
        try:
            if isolated_registry:
                for _ in range(60):
                    if subprocess.run(["docker", "exec", name, "docker", "info"], capture_output=True).returncode == 0: break
                    time.sleep(1)
                else: raise RuntimeError("Isolated Docker daemon did not start")
            run("docker", "exec", name, "mkdir", "-p", remote)
            run("docker", "cp", str(binary), name + ":/usr/local/bin/worker.test")
            run("docker", "exec", name, "chmod", "700", "/usr/local/bin/worker.test")
            run("docker", "exec", "-e", "COVE_DATABASE_FIXTURE=" + remote, name,
                "worker.test", "-test.run", sys.argv[1] if len(sys.argv)>1 else "TestDatabase.*RoundTrip|TestPrivateNetworkCompose", "-test.v", "-test.timeout", "20m")
        finally:
            run("docker", "exec", name, "rm", "-rf", remote)
            run("docker", "rm", "-f", name, stdout=subprocess.DEVNULL)
            if isolated_registry:
                run("docker", "rm", "-f", "-v", name+"-engine", stdout=subprocess.DEVNULL)
                run("docker", "network", "rm", name, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
