import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("browser_fixture", Path(__file__).with_name("test-browser.py"))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class WorkerCleanupTests(unittest.TestCase):
    def setUp(self):
        self.owner = "a" * 32
        self.state = {"owner": self.owner, "workloads": {"b" * 32}, "operations": {"c" * 32}, "links": True}
        self.name = "cove-browser-fixture"

    def owned(self, kind, labels, name="resource", **extra):
        return fixture.owns_worker_resource(kind, {"Name": name, "Labels": labels, **extra}, self.name, self.state, True)

    def test_scopes_every_managed_helper_to_fixture_identity(self):
        self.assertTrue(self.owned("container", {"cove.managed": "route-aux", "cove.route-aux-owner": self.owner}))
        self.assertTrue(self.owned("network", {"cove.managed": "idle", "cove.idle-workload": "b" * 32}))
        self.assertTrue(self.owned("volume", {"cove.managed": "copy", "dev.cove.operation": "c" * 32}))
        self.assertTrue(self.owned("container", {"cove.managed": "link"}, Mounts=[{"Type": "bind", "Source": "/var/lib/cove/links/state"}]))
        self.assertTrue(self.owned("network", {"cove.managed": "link-mesh"}, name="cove-link-mesh"))
        self.assertTrue(self.owned("volume", {"cove.managed": "ingress", "cove.browser-fixture": self.name}))
        self.assertFalse(self.owned("container", {"cove.managed": "route-aux", "cove.route-aux-owner": "d" * 32}, Mounts=[{"Type": "bind", "Source": "/var/lib/cove/links/state"}]))
        self.assertFalse(self.owned("network", {"cove.managed": "idle", "cove.idle-workload": "d" * 32}))
        self.assertFalse(self.owned("network", {"cove.managed": "link-mesh"}, name="another-fixture-mesh"))
        self.assertFalse(self.owned("volume", {"cove.managed": "ingress", "cove.browser-fixture": "other-fixture"}))
        self.assertFalse(self.owned("container", {}, Mounts=[{"Type": "bind", "Source": "/var/lib/cove/links/state"}]))

    def test_legacy_validator_requires_an_owned_volume(self):
        self.state["volumes"] = {"cove-ingress"}
        mounts = [{"Type": "volume", "Name": "cove-ingress"}]
        self.assertTrue(self.owned("container", {"cove.managed": "ingress-check"}, Mounts=mounts))
        self.assertFalse(self.owned("container", {"cove.managed": "ingress-check", "cove.owner": "foreign"}, Mounts=mounts))
        self.assertFalse(fixture.owns_worker_resource("network", {"Name": "cove-link-mesh", "Labels": {"cove.managed": "link-mesh"}}, self.name, self.state, False))

    def test_operations_helpers_without_managed_label_keep_exact_ownership(self):
        for label in ("dev.cove.backup-owner", "dev.cove.pitr.owner"):
            self.assertTrue(self.owned("container", {label: self.owner}))
            self.assertTrue(self.owned("volume", {label: self.owner}))
            self.assertFalse(self.owned("container", {label: "d" * 32}))
            self.assertFalse(self.owned("container", {label: self.owner, "cove.owner": "d" * 32}))
        self.assertTrue(self.owned("volume", {"cove.backup": "b" * 32}))
        self.assertFalse(self.owned("volume", {"cove.backup": "d" * 32}))
        self.assertTrue(self.owned("container", {"cove.managed": "files", "dev.cove.workload": "b" * 32}))
        self.assertFalse(self.owned("container", {"cove.managed": "files", "dev.cove.workload": "d" * 32}))

    def test_build_cache_uses_exact_worker_nonce(self):
        builder = "buildx_buildkit_cove-" + self.owner + "0"
        self.assertTrue(self.owned("container", {}, name="/" + builder))
        self.assertTrue(self.owned("volume", {}, name=builder + "_state"))
        self.assertFalse(self.owned("volume", {}, name=builder + "_state-foreign"))

    def test_inspect_handles_each_docker_missing_resource_error(self):
        for kind, error in (("container", "No such container"), ("network", "network not found"), ("volume", "no such volume")):
            with self.subTest(kind=kind), patch.object(fixture.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", error)):
                self.assertIsNone(fixture.inspect_cleanup_resource(kind, "absent"))

    def test_cleanup_removes_containers_first_preserves_foreign_and_reports_failure(self):
        records = {
            ("container", "own-helper"): {"ID": "own-helper", "Name": "helper", "Labels": {"cove.managed": "route-aux", "cove.route-aux-owner": self.owner}},
            ("container", "foreign-helper"): {"ID": "foreign-helper", "Name": "foreign", "Labels": {"cove.managed": "route-aux", "cove.route-aux-owner": "d" * 32}},
            ("network", "own-net"): {"ID": "own-net", "Name": "idle-net", "Labels": {"cove.managed": "idle", "cove.idle-workload": "b" * 32}},
            ("volume", "own-volume"): {"Name": "own-volume", "Labels": {"cove.managed": "copy", "cove.owner": self.owner}},
        }
        deleted = []

        def listing(*args):
            if any(value.startswith("name=buildx_") for value in args):
                return []
            kind = "container" if args[1] == "ps" else args[1]
            return [name for candidate, name in records if candidate == kind]

        def remove(args, **kwargs):
            kind = "container" if args[1] == "rm" else args[1]
            name = args[-1]
            deleted.append((kind, name))
            if kind == "network":
                return subprocess.CompletedProcess(args, 1, "", "foreign endpoint is attached")
            records.pop((kind, name))
            return subprocess.CompletedProcess(args, 0, "", "")

        with patch.object(fixture, "output_lines", listing), patch.object(fixture, "inspect_cleanup_resource", lambda kind, name: records.get((kind, name))), patch.object(fixture.subprocess, "run", remove):
            with self.assertRaisesRegex(RuntimeError, "could not remove network own-net"):
                fixture.cleanup_worker_resources(self.name, self.state, True)
        self.assertEqual(deleted[0], ("container", "own-helper"))
        self.assertNotIn(("container", "foreign-helper"), deleted)
        self.assertIn(("container", "foreign-helper"), records)
        self.assertIn(("volume", "own-volume"), deleted)


if __name__ == "__main__":
    unittest.main()
