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


def verify_image(image):
    repository = image.rsplit(":", 1)[0]
    digests = json.loads(run(["docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image],
                             capture_output=True, text=True).stdout)
    matches = [digest for digest in digests or [] if re.fullmatch(re.escape(repository) + r"@sha256:[a-f0-9]{64}", digest)]
    if len(matches) != 1:
        raise RuntimeError("Expected one immutable release image digest.")
    run(["cosign", "verify", "--certificate-identity",
         "https://github.com/bwmp-dev/Cove/.github/workflows/images.yml@refs/heads/main",
         "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", matches[0]],
        stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    return matches[0]


def image_labels(image):
    return json.loads(run(["docker", "inspect", "--format", "{{json .Config.Labels}}", image],
                          capture_output=True, text=True).stdout) or {}


def is_downgrade(current, target):
    def stable(value):
        if re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", value):
            return tuple(map(int, value[1:].split(".")))
        return None
    before = current.get("org.opencontainers.image.version", "")
    after = target.get("org.opencontainers.image.version", "")
    if before and before == after:
        return False
    a, b = stable(before), stable(after)
    if a is not None and b is not None:
        return b < a
    if current.get("org.opencontainers.image.revision") and current.get("org.opencontainers.image.revision") == target.get("org.opencontainers.image.revision"):
        return False
    try:
        old_generation = int(current["io.cove.source-generation"])
        new_generation = int(target["io.cove.source-generation"])
    except (KeyError, ValueError):
        raise RuntimeError("Cannot establish update order; use a backed-up metadata-aware forward upgrade first") from None
    if old_generation < 1 or new_generation < 1 or old_generation == new_generation or not current.get("io.cove.source-root") or current.get("io.cove.source-root") != target.get("io.cove.source-root"):
        raise RuntimeError("Source histories are incomparable; automatic channel replacement is refused")
    return new_generation < old_generation



def enforce_update_order(current, target, version, confirmed):
    if target.get("org.opencontainers.image.version") != version:
        raise RuntimeError("Target image version does not match the requested release")
    if is_downgrade(current, target):
        old_schema = current.get("io.cove.schema-sha256", "")
        if not re.fullmatch(r"[a-f0-9]{64}", old_schema) or target.get("io.cove.schema-sha256") != old_schema:
            raise RuntimeError("Unsafe schema downgrade blocked; restore a compatible backup explicitly")
        if not confirmed:
            raise RuntimeError("Downgrade requires explicit confirmation")


def report_channel(installation, channel, version):
    labels = image_labels("ghcr.io/bwmp-dev/cove:" + channel)
    host_request(installation, "/api/settings/update-status", {
        "channel": channel, "version": version,
        "revision": labels.get("org.opencontainers.image.revision", ""),
        "schema_hash": labels.get("io.cove.schema-sha256", ""),
        "revision_time": labels.get("io.cove.revision-time", ""),
        "source_root": labels.get("io.cove.source-root", ""), "source_generation": int(labels.get("io.cove.source-generation", "0"))})


def latest_release(channel="release"):
    if channel not in ("beta", "release"):
        raise ValueError("Choose beta or release.")
    image = "ghcr.io/bwmp-dev/cove:" + ("beta" if channel == "beta" else "release")
    run(["docker", "pull", image])
    verify_image(image)
    version = run(["docker", "image", "inspect", "--format", '{{index .Config.Labels "org.opencontainers.image.version"}}', image],
                  capture_output=True, text=True).stdout.strip()
    pattern = r"beta-[a-f0-9]{40}" if channel == "beta" else r"v[0-9]+\.[0-9]+\.[0-9]+"
    if not re.fullmatch(pattern, version):
        raise ValueError("Image version does not match the selected channel.")
    return version


def host_settings(installation):
    config = json.loads(run(["docker", "compose", "config", "--format", "json"], cwd=installation,
                            capture_output=True, text=True).stdout)
    return config["services"]["cove"]["environment"]


def host_request(installation, path, payload=None):
    env = host_settings(installation)
    origin = env["COVE_PUBLIC_URL"]
    if not origin.startswith("https://"):
        raise ValueError("The panel must use HTTPS.")
    headers = {"Authorization": "Bearer " + env["COVE_ADMIN_TOKEN"], "Content-Type": "application/json"}
    request = urllib.request.Request(origin.rstrip("/") + path, headers=headers,
                                     data=json.dumps(payload).encode() if payload is not None else None)
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)


def load_outbound_proxy(installation):
    config = host_request(installation, "/api/settings/outbound-proxy/environment")
    for field, name in [("http_proxy", "HTTP_PROXY"), ("https_proxy", "HTTPS_PROXY"), ("no_proxy", "NO_PROXY")]:
        os.environ[name] = config[field]
        os.environ[name.lower()] = config[field]
    urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler()))


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


def pre_update_snapshot(installation, lock):
    script = Path(__file__).with_name('panel-recovery.py')
    run([sys.executable, str(script), 'before-update', '--installation', str(installation),
         '--lock-fd', str(lock.fileno())], pass_fds=(lock.fileno(),))


def update(installation, version, offline_images=None, confirm_downgrade=False):
    import fcntl

    images = release_images(version)
    os.umask(0o077)
    os.chdir(installation)
    # Keep concurrent invocations from replacing each other's recovery state.
    with open(".cove-update.lock", "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if Path('.cove-panel-recovery/updates-paused.json').exists():
            raise RuntimeError('Panel recovery paused updates; review the restored panel and explicitly resume updates in Settings.')
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
        schema_changed = False
        for service in services:
            container = run([*compose, "ps", "-q", service], capture_output=True, text=True).stdout.strip()
            if not container:
                raise RuntimeError(f"{service} must be running before updating.")
            previous[service] = {"image": run(["docker", "inspect", "--format", "{{.Image}}", container],
                                              capture_output=True, text=True).stdout.strip()}
            # Resolve tags to immutable local IDs before restarting anything.
            if offline_images is None:
                run(["docker", "pull", images[service]])
                verify_image(images[service])
            else:
                if service not in offline_images or not re.fullmatch(r"sha256:[a-f0-9]{64}", offline_images[service]):
                    raise ValueError("Invalid verified offline image identity")
                images[service] = offline_images[service]
            config["services"][service]["image"] = run(
                ["docker", "image", "inspect", "--format", "{{.Id}}", images[service]],
                capture_output=True, text=True).stdout.strip()
            config["services"][service].pop("build", None)
            if service == "cove":
                current_labels = image_labels(previous[service]["image"])
                target_labels = image_labels(config["services"][service]["image"])
                enforce_update_order(current_labels, target_labels, version, confirm_downgrade)
                schema_changed = current_labels.get("io.cove.schema-sha256") != target_labels.get("io.cove.schema-sha256")
        if all(previous[service]["image"] == config["services"][service]["image"] for service in services):
            print(f"Already running {version}; no update needed.")
            return
        pre_update_snapshot(installation, lock)
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
            if replaced and schema_changed:
                print("Schema fingerprint changed; automatic binary rollback was refused. The panel is stopped. Restore the saved database/configuration explicitly before starting an older image.", file=sys.stderr)
                run([*new, "stop", "cove"])
            elif replaced:
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
    parser.add_argument("--confirm-downgrade", action="store_true", help="Explicitly permit a downgrade only when schema fingerprints match")
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
        load_outbound_proxy(installation)
        channel = args.channel or selected_channel(installation) if args.latest else None
        version = latest_release(channel) if args.latest else args.version
        confirmed = args.confirm_downgrade
        if args.latest:
            report_channel(installation, channel, version)
            other = "beta" if channel == "release" else "release"
            try:
                other_version = latest_release(other)
                report_channel(installation, other, other_version)
            except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError):
                print("Other channel could not be verified; its switching observation was not refreshed.", file=sys.stderr)
            settings = host_request(installation, "/api/updates")
            target_revision = image_labels("ghcr.io/bwmp-dev/cove:" + channel).get("org.opencontainers.image.revision")
            confirmed = confirmed or (settings.get("confirmed_downgrade_version") == version and settings.get("confirmed_downgrade_revision") == target_revision)
        update(installation, version, confirm_downgrade=confirmed)
    except RuntimeError as error:
        sys.exit("Update failed: " + str(error) + ". Inspect the saved recovery files before retrying.")
    except (OSError, ValueError, subprocess.CalledProcessError):
        sys.exit("Update failed. Inspect .cove-updates recovery files and service logs; credentials were not printed.")
