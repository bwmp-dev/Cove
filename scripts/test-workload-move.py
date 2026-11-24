"""Test node moves using two private Docker daemons, never the host Docker socket."""
import argparse
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--browser", action="store_true", help="exercise the real move UI using frontend/dist")
    args = parser.parse_args()
    if args.browser and not (ROOT / "frontend/dist/index.html").is_file():
        parser.error("build the frontend with pnpm --dir frontend build first")
    owner = "cove-move-test-" + secrets.token_hex(6)
    image = owner + ":test"
    database = owner + "-db"
    created = built = False
    with tempfile.TemporaryDirectory(prefix=owner) as directory:
        fixture = Path(directory)
        try:
            run("go", "build", "-o", str(fixture / "cove-worker"), "./cmd/cove-worker",
                env=os.environ | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"})
            for dependency in ("busybox:1.37", "restic/restic:0.19.1", "traefik:v3.7.19"):
                if subprocess.run(["docker", "image", "inspect", dependency], capture_output=True).returncode:
                    run("docker", "pull", dependency)
            run("docker", "image", "save", "-o", str(fixture / "seed.tar"), "busybox:1.37", "restic/restic:0.19.1", "traefik:v3.7.19")
            # The DinD wrapper mounts /tmp, so the cached image archive must live outside it.
            (fixture / "Dockerfile").write_text(
                "FROM docker:29-dind\n"
                "RUN apk add --no-cache openssh && adduser -D -h /var/lib/cove cove && addgroup cove root && passwd -u cove "
                "&& mkdir -p /run/sshd /run/cove /var/lib/cove/.ssh && chown -R cove:cove /run/cove /var/lib/cove && ssh-keygen -A\n"
                "COPY cove-worker /usr/local/bin/cove-worker\nCOPY entrypoint.sh /entrypoint.sh\nCOPY seed.tar /usr/local/share/cove-move-seed.tar\n"
                "RUN chmod 755 /entrypoint.sh /usr/local/bin/cove-worker\n"
                "ENV DOCKER_TLS_CERTDIR=\"\"\nENTRYPOINT [\"/entrypoint.sh\"]\n", newline="\n")
            (fixture / "entrypoint.sh").write_text(
                "#!/bin/sh\nset -eu\nrm -f /run/docker.pid\ndockerd-entrypoint.sh dockerd --storage-driver=vfs --host=unix:///var/run/docker.sock > /var/log/dockerd.log 2>&1 &\n"
                "for i in $(seq 1 90); do if docker info >/dev/null 2>&1; then break; fi; sleep 1; done\n"
                "docker info >/dev/null\ndocker image inspect busybox:1.37 restic/restic:0.19.1 traefik:v3.7.19 >/dev/null 2>&1 || docker load -i /usr/local/share/cove-move-seed.tar\n"
                "chgrp root /var/run/docker.sock\nchmod 660 /var/run/docker.sock\n"
                "su -s /bin/sh cove -c '/usr/local/bin/cove-worker daemon' &\n"
                "exec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no\n", newline="\n")
            run("docker", "build", "-t", image, str(fixture))
            built = True
            password = secrets.token_hex(24)
            environment = os.environ | {"POSTGRES_PASSWORD": password}
            run("docker", "run", "-d", "--name", database, "--label", "cove.test=true",
                "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB=cove_test", "-p", "127.0.0.1::5432", "postgres:17-alpine",
                env=environment, stdout=subprocess.DEVNULL)
            created = True
            port = run("docker", "port", database, "5432/tcp", capture_output=True, text=True).stdout.strip().rsplit(":", 1)[1]
            for _ in range(30):
                if subprocess.run(["docker", "exec", database, "pg_isready", "-U", "postgres"], capture_output=True).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("Disposable PostgreSQL did not start")
            environment.update(COVE_TEST_DATABASE_URL=f"postgres://postgres:{password}@127.0.0.1:{port}/cove_test?sslmode=disable",
                               COVE_TEST_DATABASE_CONTAINER=database, COVE_TEST_NODE_MOVE_IMAGE=image)
            if args.browser:
                shutil.copytree(ROOT / "frontend/dist", fixture / "dist")
                shots = ROOT / "frontend/test-results" / owner
                shots.mkdir(parents=True)
                environment.update(COVE_MOVE_BROWSER_SCRIPT=str(ROOT / "frontend/e2e/workload-move-real.mjs"),
                                   COVE_MOVE_BROWSER_DIST=str(fixture / "dist"),
                                   COVE_MOVE_BROWSER_FRONTEND=str(ROOT / "frontend"),
                                   COVE_MOVE_BROWSER_SHOTS=str(shots))
            run("go", "test", "./internal/control", "-run", "TestWorkloadMoveTwoDockerDaemons", "-v", "-count=1", "-timeout", "20m", env=environment)
        finally:
            if created:
                run("docker", "rm", "-f", "-v", database, stdout=subprocess.DEVNULL)
            if built:
                run("docker", "image", "rm", image, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
