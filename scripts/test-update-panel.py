import importlib.util
from pathlib import Path
import unittest
import json
import os
import subprocess
import tempfile
import types
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("update_panel", Path(__file__).with_name("update-panel.py"))
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)


class UpdateTests(unittest.TestCase):
    def test_signature_verification_requires_expected_repository_digest(self):
        with patch.object(updater, "run") as run:
            run.return_value = subprocess.CompletedProcess([], 0, stdout=json.dumps(["ghcr.io/attacker/cove@sha256:" + "a" * 64]))
            with self.assertRaises(RuntimeError):
                updater.verify_image("ghcr.io/bwmp-dev/cove:v1.2.3")
            self.assertEqual(run.call_count, 1)

    def test_signature_failure_is_propagated(self):
        with patch.object(updater, "run") as run:
            run.side_effect = [subprocess.CompletedProcess([], 0, stdout=json.dumps(["ghcr.io/bwmp-dev/cove@sha256:" + "a" * 64])), subprocess.CalledProcessError(1, ["cosign"])]
            with self.assertRaises(subprocess.CalledProcessError):
                updater.verify_image("ghcr.io/bwmp-dev/cove:v1.2.3")

    def verified_latest(self, channel="release"):
        with patch.object(updater, "verify_image"):
            return updater.latest_release(channel)

    @patch.object(updater, "run")
    def test_latest_accepts_only_stable_release_label(self, run):
        for version in ["main", "v1.2.3-beta.1", "", "1.2.3"]:
            run.return_value = subprocess.CompletedProcess([], 0, stdout=version)
            with self.assertRaises(ValueError):
                self.verified_latest()
        run.return_value = subprocess.CompletedProcess([], 0, stdout="v1.2.3\n")
        self.assertEqual(self.verified_latest(), "v1.2.3")

    @patch.object(updater, "run")
    def test_beta_channel_resolves_immutable_version(self, run):
        version = "beta-" + "a" * 40
        run.return_value = subprocess.CompletedProcess([], 0, stdout=version)
        self.assertEqual(self.verified_latest("beta"), version)
        self.assertEqual(updater.release_images(version)["cove"], "ghcr.io/bwmp-dev/cove:" + version)
        run.return_value = subprocess.CompletedProcess([], 0, stdout="v1.2.3")
        with self.assertRaises(ValueError):
            self.verified_latest("beta")
        run.return_value = subprocess.CompletedProcess([], 0, stdout=version)
        with self.assertRaises(ValueError):
            self.verified_latest("release")

    def exercise_update(self, failure=False, local=False, unchanged=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / ".env").write_text("COVE_DOMAIN=panel.example.com\nCOVE_VERSION=v1.0.0\n")
            calls = []
            methods = []
            services = {"cove": {"image": "old-panel", "environment": {"EXAMPLE": "literal$dollar"}}, "database": {"image": "postgres:17-alpine"}}
            if local:
                services["worker"] = {"image": "old-worker"}

            def run(args, **kwargs):
                calls.append(args)
                if "config" in args:
                    output = json.dumps({"name": "cove", "services": services})
                elif "ps" in args:
                    output = "container-id"
                elif "inspect" in args:
                    output = "sha256:old" if "image" not in args or unchanged else "sha256:new"
                elif "rpc" in args:
                    method = json.loads(kwargs["input"])["method"]
                    methods.append(method)
                    output = json.dumps({"maintenance": {"draining": method == "drain", "active": 0}, "probe": {"ready": True}})
                elif "up" in args and failure and any("target.json" in str(arg) for arg in args):
                    raise subprocess.CalledProcessError(1, args)
                else:
                    output = ""
                if "stdout" in kwargs:
                    kwargs["stdout"].write(b"database backup")
                return subprocess.CompletedProcess(args, 0, stdout=output)

            previous = Path.cwd()
            try:
                with patch.object(updater, "verify_image"), patch.object(updater, "run", side_effect=run), patch.dict("sys.modules", {"fcntl": types.SimpleNamespace(flock=lambda *args: None, LOCK_EX=1, LOCK_NB=2)}):
                    if failure:
                        with self.assertRaises(subprocess.CalledProcessError):
                            updater.update(root, "v1.2.3")
                    else:
                        updater.update(root, "v1.2.3")
            finally:
                os.chdir(previous)
            if unchanged:
                self.assertFalse((root / ".cove-updates").exists())
                self.assertFalse(any("up" in call for call in calls))
                self.assertEqual(methods, [])
                return
            self.assertIn("COVE_VERSION=" + ("v1.0.0" if failure else "v1.2.3"), (root / ".env").read_text())
            ups = [call for call in calls if "up" in call]
            self.assertEqual(len(ups), 2 if failure else 1)
            if failure:
                self.assertTrue(any("rollback.json" in str(arg) for arg in ups[-1]))
            self.assertTrue(all("--no-deps" in call and "--no-build" in call for call in ups))
            self.assertEqual(next((root / ".cove-updates").glob("*/database.dump")).read_bytes(), b"database backup")
            self.assertIn("literal$$dollar", next((root / ".cove-updates").glob("*/target.json")).read_text())
            if local:
                self.assertEqual(methods, ["drain", "resume"] if failure else ["drain", "probe", "resume"])

    def test_update_persists_version_and_backup(self):
        self.exercise_update()

    def test_current_images_do_not_restart_or_create_backups(self):
        self.exercise_update(unchanged=True)

    def test_failure_restores_previous_images(self):
        self.exercise_update(failure=True)

    def test_local_worker_drains_and_resumes(self):
        self.exercise_update(local=True)

    def test_local_worker_resumes_after_rollback(self):
        self.exercise_update(local=True, failure=True)

    def test_requires_explicit_release(self):
        for invalid in ["latest", "main", "v1", "v1.2.3;id", "sha-123", "../release"]:
            with self.assertRaises(ValueError):
                updater.release_images(invalid)
        self.assertEqual(updater.release_images("v1.2.3")["cove"], "ghcr.io/bwmp-dev/cove:v1.2.3")
        self.assertTrue(updater.release_images("sha-" + "a" * 40)["worker"].endswith("a" * 40))

    @patch.object(updater.time, "sleep")
    @patch.object(updater, "rpc")
    def test_drain_waits_for_operations(self, rpc, sleep):
        rpc.side_effect = [{"maintenance": {"draining": True, "active": 2}},
                           {"maintenance": {"draining": True, "active": 0}}]
        updater.drain(["docker", "compose"])
        self.assertEqual(rpc.call_count, 2)
        sleep.assert_called_once_with(5)

    @patch.object(updater, "rpc", return_value={"maintenance": {"draining": False, "active": 0}})
    def test_rejects_failed_drain(self, rpc):
        with self.assertRaises(RuntimeError):
            updater.drain([])


if __name__ == "__main__":
    unittest.main()
