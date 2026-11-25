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
        for kind in ("container", "network"):
            self.assertTrue(self.owned(kind, {"cove.managed": "cloudflare-tunnel", "cove.cloudflare-tunnel-owner": self.owner}))
            self.assertFalse(self.owned(kind, {"cove.managed": "cloudflare-tunnel", "cove.cloudflare-tunnel-owner": "d" * 32}))
        self.assertTrue(self.owned("container", {"cove.managed": "cloudflare-probe", "cove.cloudflare-tunnel-owner": self.owner}))
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

    def test_scanner_helpers_require_exact_fixture_worker_owner(self):
        labels = {"cove.managed": "image-scan", "cove.image-scan-owner": self.owner}
        self.assertTrue(self.owned("container", labels))
        self.assertFalse(self.owned("container", {**labels, "cove.image-scan-owner": "d" * 32}))
        self.assertFalse(self.owned("container", {**labels, "cove.owner": "foreign"}))

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


    def test_operations_helpers_use_backup_owner_or_exact_resource_journal(self):
        self.state["backup_owner"] = "e" * 32
        for labels in (
            {"dev.cove.backup-owner": "e" * 32, "dev.cove.database-verify": "c" * 32},
            {"dev.cove.snapshot-workload": "b" * 32, "dev.cove.operation": "c" * 32},
            {"dev.cove.environment-preview": "b" * 32},
            {"cove.backup": "b" * 32},
            {"dev.cove.restore": "b" * 32},
            {"com.docker.compose.project": "cove-" + "b" * 32},
        ):
            with self.subTest(labels=labels):
                self.assertTrue(self.owned("container", labels))
                self.assertFalse(self.owned("container", {**labels, "dev.cove.backup-owner": "foreign"}))
        self.state["shared_networks"] = {"f" * 32}
        self.assertTrue(self.owned("network", {"cove.shared-network": "f" * 32}))
        self.assertFalse(self.owned("network", {"cove.shared-network": "foreign"}))

    def test_file_helpers_need_owned_volume_and_exact_random_name(self):
        self.state["volumes"] = {"owned-data"}
        mounts = [{"Type": "volume", "Name": "owned-data"}]
        self.assertTrue(self.owned("container", {}, "cove-files-" + "a" * 24, Mounts=mounts))
        self.assertFalse(self.owned("container", {}, "cove-files-not-a-nonce", Mounts=mounts))
        self.assertFalse(self.owned("container", {}, "cove-files-" + "a" * 24, Mounts=[{"Type": "volume", "Name": "foreign"}]))
        self.assertFalse(self.owned("container", {"cove.owner": "foreign"}, "cove-files-" + "a" * 24, Mounts=mounts))

    def test_database_tools_require_workload_and_proved_worker_root(self):
        labels = {"cove.database-tool.workload": "b" * 32, "cove.database-tool.root": fixture.hashlib.sha256(b"/var/lib/cove").hexdigest()}
        self.assertTrue(self.owned("container", labels))
        self.assertFalse(self.owned("container", {**labels, "cove.database-tool.root": "other-root"}))
        self.assertFalse(self.owned("container", {**labels, "cove.database-tool.workload": "foreign"}))
        self.assertFalse(self.owned("container", {**labels, "cove.owner": "foreign"}))
        self.assertFalse(fixture.owns_worker_resource("container", {"Name": "tool", "Labels": labels}, self.name, self.state, False))

    def test_proved_root_bind_does_not_override_a_foreign_owner(self):
        self.state["bound"] = {"owned-bind"}
        self.assertTrue(self.owned("container", {}, ID="owned-bind"))
        self.assertFalse(self.owned("container", {"cove.owner": "foreign"}, ID="owned-bind"))
        self.assertFalse(self.owned("container", {}, ID="other-bind"))

    def test_baseline_resources_are_not_removed_even_with_matching_owner(self):
        identifier = "a" * 64
        self.state["previous"] = {"container": {identifier}}
        record = {"ID": identifier, "Name": "preexisting", "Labels": {"cove.managed": "route-aux", "cove.route-aux-owner": self.owner}}

        def listing(*args):
            if args[1] == "ps":
                self.assertIn("--no-trunc", args)
                return [identifier]
            return []

        with patch.object(fixture, "output_lines", listing), patch.object(fixture, "inspect_cleanup_resource", return_value=record), patch.object(fixture.subprocess, "run") as remove:
            fixture.cleanup_worker_resources(self.name, self.state, True)
        remove.assert_not_called()

    def test_inventory_cost_does_not_grow_with_journal_history(self):
        self.state["workloads"] = {format(i, "032x") for i in range(100)}
        self.state["operations"] = {format(i, "032x") for i in range(1000)}
        with patch.object(fixture, "output_lines", return_value=[]) as listing:
            fixture.cleanup_worker_resources(self.name, self.state, True)
        self.assertEqual(listing.call_count, 3)

    def test_inspection_failure_is_not_treated_as_missing(self):
        result = subprocess.CompletedProcess([], 1, "", "permission denied")
        with patch.object(fixture.subprocess, "run", return_value=result):
            with self.assertRaisesRegex(RuntimeError, "Could not inspect"):
                fixture.inspect_cleanup_resource("container", "unknown")


if __name__ == "__main__":
    unittest.main()
