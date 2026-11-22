#!/usr/bin/env python3
import argparse
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.request


def release_images(version):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?|(?:sha|beta)-[a-f0-9]{40}", version):
        raise ValueError("Choose an explicit vX.Y.Z release or beta-<40-character commit>.")
    return {"cove": f"ghcr.io/bwmp-dev/cove:{version}",
            "worker": f"ghcr.io/bwmp-dev/cove-worker:{version}"}


def latest_release(channel="release"):
    if channel not in ("beta", "release"):
        raise ValueError("Choose beta or release.")
    image = "ghcr.io/bwmp-dev/cove:" + ("beta" if channel == "beta" else "release")
    run(["docker", "pull", image])
    version = run(["docker", "image", "inspect", "--format", '{{index .Config.Labels "org.opencontainers.image.version"}}', image],
                  capture_output=True, text=True).stdout.strip()
    pattern = r"beta-[a-f0-9]{40}" if channel == "beta" else r"v[0-9]+\.[0-9]+\.[0-9]+"
    if not re.fullmatch(pattern, version):
        raise ValueError("Image version does not match the selected channel.")
    return version


def selected_channel(installation):
    config = json.loads(run(["docker", "compose", "config", "--format", "json"], cwd=installation,
                            capture_output=True, text=True).stdout)
    env = config["services"]["cove"]["environment"]
    origin = env["COVE_PUBLIC_URL"]
    if not origin.startswith("https://"):
        raise ValueError("The panel must use HTTPS.")
    request = urllib.request.Request(origin.rstrip("/") + "/api/updates", headers={"Authorization": "Bearer " + env["COVE_ADMIN_TOKEN"]})
    with urllib.request.urlopen(request, timeout=20) as response:
        channel = json.load(response)["channel"]
    if channel not in ("beta", "release"):
        raise ValueError("Invalid saved update channel.")
    return channel


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def rpc(compose, method):
    result = run([*compose, "exec", "-T", "worker", "cove-worker", "rpc"],
                 input=json.dumps({"method": method}), text=True, capture_output=True)
    response = json.loads(result.stdout)
    if response.get("error"):
        raise RuntimeError("Local worker RPC failed.")
    return response


def drain(compose):
    for _ in range(360):
        state = rpc(compose, "drain").get("maintenance", {})
        if state.get("draining") is not True or type(state.get("active")) is not int:
            raise RuntimeError("Worker does not support safe draining.")
        if state["active"] == 0:
            return
        time.sleep(5)
    raise RuntimeError("Worker is still busy; update cancelled.")


def update(installation, version):
    import fcntl

    images = release_images(version)
    os.umask(0o077)
    os.chdir(installation)
    # Keep concurrent invocations from replacing each other's recovery state.
    with open(".cove-update.lock", "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        compose = ["docker", "compose"]
        config = json.loads(run([*compose, "config", "--format", "json"],
                                capture_output=True, text=True).stdout)
        services = [name for name in images if name in config["services"]]
        if "cove" not in services or "database" not in config["services"]:
            raise RuntimeError("Expected Cove and database Compose services.")
        original_config = json.dumps(config).replace("$", "$$")
        environment = Path(".env")
        original_env = environment.read_text()
        previous = {}
        for service in services:
            container = run([*compose, "ps", "-q", service], capture_output=True, text=True).stdout.strip()
            if not container:
                raise RuntimeError(f"{service} must be running before updating.")
            previous[service] = {"image": run(["docker", "inspect", "--format", "{{.Image}}", container],
                                              capture_output=True, text=True).stdout.strip()}
            # Resolve tags to immutable local IDs before restarting anything.
            run(["docker", "pull", images[service]])
            config["services"][service]["image"] = run(
                ["docker", "image", "inspect", "--format", "{{.Id}}", images[service]],
                capture_output=True, text=True).stdout.strip()
            config["services"][service].pop("build", None)
        if all(previous[service]["image"] == config["services"][service]["image"] for service in services):
            print(f"Already running {version}; no update needed.")
            return
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
        backup = Path(".cove-updates", stamp).resolve()
        backup.mkdir(parents=True, mode=0o700)
        snapshot = backup / "compose.json"
        # Compose interpolates even resolved files; preserve literal dollars in credentials.
        snapshot.write_text(original_config)
        (backup / "environment").write_text(original_env)
        rollback = backup / "rollback.json"
        rollback.write_text(json.dumps({"services": previous}))
        target = backup / "target.json"
        target.write_text(json.dumps(config).replace("$", "$$"))
        old = ["docker", "compose", "--project-directory", str(installation), "-f", str(snapshot), "-f", str(rollback)]
        new = ["docker", "compose", "--project-directory", str(installation), "-f", str(target)]
        with (backup / "database.dump").open("wb") as output:
            run([*compose, "exec", "-T", "database", "pg_dump", "-U", "cove", "-d", "cove", "--format=custom"], stdout=output)
        drained = False
        replaced = False
        try:
            if "worker" in services:
                maintenance = run([*compose, "exec", "-T", "worker", "test", "!", "-e", "/var/lib/cove/draining.json"],
                                  capture_output=True)
                if maintenance.returncode != 0:
                    raise RuntimeError("Local worker is already in maintenance.")
                drained = True
                drain(compose)
            replaced = True
            run([*new, "up", "-d", "--no-deps", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "120", *services])
            if drained:
                for _ in range(30):
                    try:
                        if rpc(new, "probe").get("probe", {}).get("ready") is True:
                            break
                    except (subprocess.CalledProcessError, RuntimeError):
                        pass
                    time.sleep(1)
                else:
                    raise RuntimeError("Replacement local worker failed readiness.")
            lines = [line for line in original_env.splitlines() if not re.match(r"\s*(?:export\s+)?COVE_VERSION\s*=", line)]
            temporary_env = backup / "next-environment"
            temporary_env.write_text("\n".join([*lines, f"COVE_VERSION={version}", ""]))
            os.replace(temporary_env, environment)
        except BaseException:
            if replaced:
                run([*old, "up", "-d", "--no-deps", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "120", *services])
            raise
        finally:
            if drained:
                if rpc(compose, "resume").get("maintenance", {}).get("draining") is not False:
                    raise RuntimeError("Could not resume local worker; inspect it before continuing.")
        print(f"Updated to {version}. Remote workers with update timers will follow within three minutes.")
        print(f"Private recovery files: {backup}")
        print("Database changes are not automatically reversed; keep the dump and original encryption key.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Update the Cove panel and local container worker from published images.")
    parser.add_argument("version", nargs="?", help="vX.Y.Z or sha-<full commit>")
    parser.add_argument("--latest", action="store_true", help="Follow the channel selected in dashboard settings")
    parser.add_argument("--channel", choices=["beta", "release"], help="Override the saved channel for this check")
    parser.add_argument("--installation", type=Path, default=Path(__file__).resolve().parents[1])
    args = parser.parse_args()
    if sys.platform != "linux" or os.geteuid() != 0:
        parser.error("Run on the Linux panel host as root.")
    if bool(args.version) == args.latest:
        parser.error("Choose either a version or --latest.")
    if args.channel and not args.latest:
        parser.error("--channel requires --latest.")
    try:
        installation = args.installation.resolve()
        version = latest_release(args.channel or selected_channel(installation)) if args.latest else args.version
        update(installation, version)
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError):
        sys.exit("Update failed. Inspect .cove-updates recovery files and service logs; credentials were not printed.")
