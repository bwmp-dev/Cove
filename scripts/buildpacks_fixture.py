"""A self-contained Procfile source for the owned packaged CNB fixture."""
import json
import os
from pathlib import Path
import subprocess


def install_buildpacks_fixture(run, name, worker, env, directory):
    identity = run("docker", "info", "--format", "{{.ID}}", capture_output=True, text=True).stdout.strip()
    if identity != os.environ.get("COVE_BROWSER_MANAGED_DAEMON_ID"):
        raise RuntimeError("CNB browser acceptance requires its verified owned nested daemon")
    marker = run("docker", "exec", worker, "cat", "/var/lib/cove/.browser-owner", capture_output=True, text=True).stdout
    if marker != name:
        raise RuntimeError("CNB fixture worker ownership changed")
    binary = Path(directory) / "cnb-fixture-server"
    run("go", "build", "-trimpath", "-o", str(binary), "./test/buildpacks", env=dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0"))
    run("docker", "exec", "--user", "0", worker, "sh", "-ec", "rm -rf /tmp/cove-recipe-fixture/source; mkdir -p /tmp/cove-recipe-fixture/source", stdout=subprocess.DEVNULL)
    run("docker", "cp", str(binary), worker + ":/tmp/cove-recipe-fixture/source/fixture-server", stdout=subprocess.DEVNULL)
    run("docker", "exec", "--user", "0", worker, "sh", "-ec", "printf 'web: ./fixture-server\\n' > /tmp/cove-recipe-fixture/source/Procfile; chmod -R a+rX /tmp/cove-recipe-fixture/source; chmod 755 /tmp/cove-recipe-fixture/source/fixture-server", stdout=subprocess.DEVNULL)
    probe = json.loads(run("docker", "exec", "--user", "cove", worker, "/usr/local/bin/cove-worker", "probe", capture_output=True, text=True).stdout)
    capacity = probe.get("buildpacks_capacity")
    if not probe.get("buildpacks") or not isinstance(capacity, dict) or any(not isinstance(capacity.get(field), int) or capacity[field] <= 0 for field in ("memory_available", "disk_available", "state_available")):
        raise RuntimeError("Real CNB resource accounting is unavailable: " + json.dumps({key: probe.get(key) for key in ("buildpacks", "buildpacks_reason", "buildpacks_capacity")}))
    if probe.get("buildpacks_reason"):
        print("CNB packaged fixture admission refused with real measurements: " + json.dumps({"reason": probe["buildpacks_reason"], "capacity": capacity}), flush=True)
    env["COVE_BROWSER_BUILDPACKS"] = "1"
