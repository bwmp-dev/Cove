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
    def test_downgrade_requires_confirmation_and_matching_schema(self):
        current = {"org.opencontainers.image.version": "v2.0.0", "io.cove.schema-sha256": "a" * 64}
        target = {"org.opencontainers.image.version": "v1.0.0", "io.cove.schema-sha256": "a" * 64}
        with self.assertRaises(RuntimeError):
            updater.enforce_update_order(current, target, "v1.0.0", False)
        updater.enforce_update_order(current, target, "v1.0.0", True)
        target["io.cove.schema-sha256"] = "b" * 64
        with self.assertRaises(RuntimeError):
            updater.enforce_update_order(current, target, "v1.0.0", True)

    def test_beta_order_uses_signed_source_lineage(self):
        current = {"org.opencontainers.image.version": "beta-" + "a" * 40,
                   "io.cove.source-root": "c" * 40, "io.cove.source-generation": "20"}
        target = {"org.opencontainers.image.version": "v1.0.0",
                  "io.cove.source-root": "c" * 40, "io.cove.source-generation": "19"}
        self.assertTrue(updater.is_downgrade(current, target))
        target["io.cove.source-root"] = "d" * 40
        with self.assertRaises(RuntimeError):
            updater.is_downgrade(current, target)

    def test_offline_update_uses_verified_local_ids_without_pull(self):
        self.exercise_update(offline=True)

    def test_failed_schema_change_never_starts_older_panel(self):
        self.exercise_update(failure=True, schema_changed=True)

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

    def exercise_update(self, failure=False, local=False, unchanged=False, offline=False, schema_changed=False):
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
                if "inspect" in args and "{{json .Config.Labels}}" in args:
                    output = json.dumps({"org.opencontainers.image.version": "v1.0.0" if args[-1] == "sha256:old" and not unchanged else "v1.2.3", "io.cove.schema-sha256": ("b" if schema_changed and args[-1] != "sha256:old" else "a") * 64})
                elif "config" in args:
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
                with patch.object(updater, "verify_image"), patch.object(updater, "pre_update_snapshot") as pre_snapshot, patch.object(updater, "run", side_effect=run), patch.dict("sys.modules", {"fcntl": types.SimpleNamespace(flock=lambda *args: None, LOCK_EX=1, LOCK_NB=2)}):
                    if failure:
                        with self.assertRaises(subprocess.CalledProcessError):
                            updater.update(root, "v1.2.3", {name: "sha256:" + "b" * 64 for name in ("cove", "worker")} if offline else None)
                    else:
                        updater.update(root, "v1.2.3", {name: "sha256:" + "b" * 64 for name in ("cove", "worker")} if offline else None)
            finally:
                os.chdir(previous)
            self.assertEqual(pre_snapshot.call_count, 0 if unchanged else 1)
            if unchanged:
                self.assertFalse((root / ".cove-updates").exists())
                self.assertFalse(any("up" in call for call in calls))
                self.assertEqual(methods, [])
                return
            if offline:
                self.assertFalse(any("pull" in call for call in calls))
            if failure and schema_changed:
                self.assertTrue(any("stop" in call and "cove" in call for call in calls))
                self.assertFalse(any(any("rollback.json" in str(arg) for arg in call) for call in calls if "up" in call))
            self.assertIn("COVE_VERSION=" + ("v1.0.0" if failure else "v1.2.3"), (root / ".env").read_text())
            ups = [call for call in calls if "up" in call]
            self.assertEqual(len(ups), 2 if failure and not schema_changed else 1)
            if failure and not schema_changed:
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

    def test_paused_recovery_blocks_updates_before_docker_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / '.cove-panel-recovery').mkdir()
            (root / '.cove-panel-recovery/updates-paused.json').write_text('{}')
            previous = Path.cwd()
            try:
                with patch.object(updater, 'run') as run, patch.dict('sys.modules', {'fcntl': types.SimpleNamespace(flock=lambda *args: None, LOCK_EX=1, LOCK_NB=2)}):
                    with self.assertRaises(RuntimeError):
                        updater.update(root, 'v1.2.3')
                    run.assert_not_called()
            finally:
                os.chdir(previous)

    def test_snapshot_failure_blocks_database_and_container_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / '.env').write_text('COVE_VERSION=v1.0.0\n')
            calls = []

            def execute(args, **kwargs):
                calls.append(args)
                if 'config' in args:
                    output = json.dumps({'services': {'cove': {'image': 'old'}, 'database': {'image': 'postgres:17-alpine'}}})
                elif 'ps' in args:
                    output = 'old-container'
                elif 'inspect' in args:
                    output = 'sha256:new' if 'image' in args else 'sha256:old'
                else:
                    output = ''
                return subprocess.CompletedProcess(args, 0, stdout=output)

            previous = Path.cwd()
            try:
                with patch.object(updater, 'verify_image'), patch.object(updater, 'image_labels', return_value={'org.opencontainers.image.version':'v1.2.3','io.cove.schema-sha256':'a'*64}), patch.object(updater, 'run', side_effect=execute), patch.object(updater, 'pre_update_snapshot', side_effect=RuntimeError('verification failed')), patch.dict('sys.modules', {'fcntl': types.SimpleNamespace(flock=lambda *args: None, LOCK_EX=1, LOCK_NB=2)}):
                    with self.assertRaises(RuntimeError):
                        updater.update(root, 'v1.2.3')
            finally:
                os.chdir(previous)
            self.assertFalse(any('up' in call or 'pg_dump' in call or 'rpc' in call for call in calls))
            self.assertEqual((root / '.env').read_text(), 'COVE_VERSION=v1.0.0\n')
            self.assertFalse((root / '.cove-updates').exists())

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


    def test_release_rejection_precedes_snapshot_and_restarts(self):
        for boundary in ("signature", "downgrade"):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                original = "COVE_VERSION=v1.0.0\n"
                (root / ".env").write_text(original)
                calls = []
                def execute(args, **kwargs):
                    calls.append(args)
                    if "config" in args:
                        value = json.dumps({"services": {"cove": {"image": "old"}, "database": {"image": "postgres:17-alpine"}}})
                    else:
                        value = "sha256:fixture"
                    return subprocess.CompletedProcess(args, 0, stdout=value)
                previous = Path.cwd()
                try:
                    with patch.object(updater, "run", side_effect=execute), patch.object(updater, "verify_image", side_effect=RuntimeError("signature refused") if boundary == "signature" else None), patch.object(updater, "enforce_update_order", side_effect=RuntimeError("downgrade refused") if boundary == "downgrade" else None), patch.object(updater, "image_labels", return_value={}), patch.object(updater, "pre_update_snapshot") as snapshot, patch.dict("sys.modules", {"fcntl": types.SimpleNamespace(flock=lambda *args: None, LOCK_EX=1, LOCK_NB=2)}):
                        with self.assertRaises(RuntimeError):
                            updater.update(root, "v1.2.3")
                        snapshot.assert_not_called()
                finally:
                    os.chdir(previous)
                self.assertEqual((root / ".env").read_text(), original)
                self.assertFalse((root / ".cove-updates").exists())
                self.assertFalse(any("up" in call or "pg_dump" in call or "rpc" in call for call in calls))


if __name__ == "__main__":
    unittest.main()
