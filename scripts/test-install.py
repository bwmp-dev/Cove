"""Exercise the installer with isolated filesystem paths and simulated host services."""
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
REVISION = "a" * 40

HELPER = r'''
import json, os, pathlib, shutil, sys, urllib.request
root = pathlib.Path(os.environ["FIXTURE"])
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with (root / "calls").open("a") as log:
    log.write(json.dumps([name] + args) + "\n")
if name == "id":
    print("0")
elif name == "cosign":
    if os.environ.get("BAD_SIGNATURE"): sys.exit(1)
elif name == "systemctl":
    if "docker" in args:
        (root / "docker-running").touch()
elif name == "apt-get":
    if "docker-ce" in args:
        shutil.copy(root / "helper", root / "bin/docker")
elif name == "dpkg":
    print("amd64")
elif name == "ss":
    if os.environ.get("PORT_BUSY"):
        print("LISTEN 0 128 0.0.0.0:443")
elif name == "curl":
    url = next(arg for arg in args if arg.startswith("https://"))
    target = pathlib.Path(args[args.index("-o") + 1])
    if "download.docker.com" in url:
        target.write_text("fixture signing key")
    else:
        prefix = "https://raw.githubusercontent.com/bwmp-dev/Cove/" + "a" * 40 + "/"
        assert url.startswith(prefix), url
        relative = url.removeprefix(prefix)
        assert relative in ["compose.yaml", "deploy/traefik.yaml", "deploy/traefik-panel.yaml", "scripts/configure.sh", "scripts/update-panel.py", "scripts/install-auto-update.sh"]
        content = (pathlib.Path(os.environ["SOURCE"]) / relative).read_text()
        content = content.replace("/usr/local/lib/cove", str(root / "lib"))
        content = content.replace("/etc/systemd/system", str(root / "units"))
        target.write_text(content)
elif name == "docker":
    if args[0] == "info" and not (root / "docker-running").exists():
        sys.exit(1)
    if args[:2] == ["image", "inspect"]:
        if "{{json .Config.Labels}}" in args:
            print(json.dumps({"org.opencontainers.image.version": "beta-" + "a" * 40, "org.opencontainers.image.revision": "a" * 40, "io.cove.schema-sha256": "b" * 64, "io.cove.revision-time": "2026-11-22T12:00:00Z", "io.cove.source-root": "c" * 40, "io.cove.source-generation": "123"}))
        else:
            print("ghcr.io/bwmp-dev/cove@sha256:" + "a" * 64 if "RepoDigests" in args[3] else "a" * 40 if "revision" in args[3] else "beta-" + "a" * 40)
elif name == "python3":
    if args[0] == "-c":
        sys.argv = ["-c", *args[2:]]
        exec(args[1])
        sys.exit(0)
    class Response:
        status = 200
        def __enter__(self): return self
        def __exit__(self, *args): pass
    def urlopen(request, timeout):
        if isinstance(request, urllib.request.Request):
            env = dict(line.split("=", 1) for line in (root / "installation/.env").read_text().splitlines())
            assert request.get_header("Authorization") == "Bearer " + env["COVE_ADMIN_TOKEN"]
            if request.full_url.endswith("/api/settings/update-status"):
                assert request.method == "POST"
                assert json.loads(request.data)["channel"] == "beta"
            else:
                assert request.method == "PUT"
                assert json.loads(request.data) == {"channel": "beta"}
                (root / "channel-set").touch()
        else:
            env = dict(line.split("=", 1) for line in (root / "installation/.env").read_text().splitlines())
            assert request == "https://cove.example.com" + env.get("COVE_BASE_PATH", "") + "/healthz"
        return Response()
    urllib.request.urlopen = urlopen
    sys.argv = args
    exec(compile(sys.stdin.read(), "installer-stdin", "exec"), {"__name__": "__main__"})
'''


@unittest.skipIf(os.name == "nt", "Run this POSIX fixture on Linux")
class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="cove-installer-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        (self.root / "units").mkdir()
        (self.root / "systemd").mkdir()
        (self.root / "apt/sources.list.d").mkdir(parents=True)
        (self.root / "os-release").write_text("ID=debian\nVERSION_CODENAME=bookworm\n")
        helper = "#!" + sys.executable + "\n" + HELPER
        (self.root / "helper").write_text(helper)
        (self.root / "helper").chmod(0o700)
        for command in ["id", "apt-get", "cosign", "systemctl", "dpkg", "ss", "curl", "python3"]:
            shutil.copy(self.root / "helper", self.bin / command)
        for command in ["sh", "uname", "install", "chmod", "grep", "openssl", "dirname", "mktemp", "ln", "rm", "cat", "mkdir"]:
            executable = shutil.which(command)
            self.assertIsNotNone(executable, command)
            (self.bin / command).symlink_to(executable)
        script = (ROOT / "scripts/install.sh").read_text()
        for old, new in [("/run/systemd/system", "systemd"), ("/etc/os-release", "os-release"), ("/etc/apt", "apt")]:
            script = script.replace(old, str(self.root / new))
        self.script = self.root / "install.sh"
        self.script.write_text(script)
        self.env = {**os.environ, "PATH": str(self.bin), "FIXTURE": str(self.root), "SOURCE": str(ROOT)}

    def run_installer(self, domain="cove.example.com", base_path="", bind_address="0.0.0.0"):
        return subprocess.run([str(self.bin / "sh"), str(self.script), domain, "beta", str(self.root / "installation"), base_path, bind_address], env=self.env, capture_output=True, text=True, timeout=30)

    def calls(self):
        return [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]

    def test_bad_signature_refuses_installation(self):
        self.env["BAD_SIGNATURE"] = "1"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "installation").exists())

    def test_fresh_host_generates_credentials_installs_services_and_updates(self):
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        env_path = self.root / "installation/.env"
        env = dict(line.split("=", 1) for line in env_path.read_text().splitlines())
        self.assertEqual(env["COVE_VERSION"], "beta-" + REVISION)
        self.assertNotIn("COVE_ADMIN_PASSWORD", env)
        for name in ["POSTGRES_PASSWORD", "COVE_ADMIN_TOKEN", "COVE_ENCRYPTION_KEY"]:
            self.assertGreaterEqual(len(env[name]), 32)
            self.assertNotIn(env[name], result.stdout + result.stderr)
        self.assertEqual(stat.S_IMODE(env_path.stat().st_mode), 0o600)
        self.assertTrue((self.root / "channel-set").exists())
        self.assertIn(["apt-get", "install", "-y", "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"], self.calls())
        self.assertIn(["docker", "compose", "up", "-d", "--no-build", "--wait", "--wait-timeout", "180"], self.calls())
        self.assertIn(["systemctl", "enable", "--now", "cove-panel-update.timer"], self.calls())
        self.assertIn("--latest --installation", (self.root / "units/cove-panel-update.service").read_text())
        compose = (self.root / "installation/compose.yaml").read_text()
        self.assertIn("postgres://cove:${POSTGRES_PASSWORD}@database:5432/cove", compose)
        self.assertNotIn("COVE_SMTP", compose)
        self.assertNotIn("COVE_GITHUB_CLIENT", compose)

    def test_reuses_docker_without_installing_or_restarting_it(self):
        shutil.copy(self.root / "helper", self.bin / "docker")
        (self.root / "docker-running").touch()
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertFalse(any("docker-ce" in call for call in self.calls()))
        self.assertNotIn(["systemctl", "enable", "--now", "docker"], self.calls())

    def test_refuses_existing_installation_before_changing_host(self):
        installation = self.root / "installation"
        installation.mkdir()
        (installation / ".env").write_text("keep-existing-credentials")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already exists", result.stderr)
        self.assertEqual((installation / ".env").read_text(), "keep-existing-credentials")
        self.assertEqual(self.calls(), [["id", "-u"]])

    def test_busy_https_port_leaves_existing_service_alone(self):
        self.env["PORT_BUSY"] = "1"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already in use", result.stderr)
        self.assertFalse((self.root / "installation").exists())
        self.assertFalse(any(call[:2] == ["docker", "pull"] for call in self.calls()))

    def test_path_and_private_bind_preserve_signed_installation(self):
        result = self.run_installer(base_path="/control/cove", bind_address="100.100.100.10")
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        env = dict(line.split("=", 1) for line in (self.root / "installation/.env").read_text().splitlines())
        self.assertEqual(env["COVE_BASE_PATH"], "/control/cove")
        self.assertEqual(env["COVE_BIND_ADDRESS"], "100.100.100.10")
        self.assertTrue(any(call[0] == "cosign" for call in self.calls()))
        self.assertIn("https://cove.example.com/control/cove/register", result.stdout)

    def test_rejects_invalid_hostname(self):
        result = self.run_installer("https://cove.example.com")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Usage:", result.stderr)
        self.assertEqual(self.calls(), [["id", "-u"]])


if __name__ == "__main__":
    unittest.main()
