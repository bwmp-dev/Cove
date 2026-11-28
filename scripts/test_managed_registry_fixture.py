import os
from pathlib import Path
import types
import unittest
from unittest.mock import patch

from managed_registry_fixture import ManagedRegistryFixture


class ManagedRegistryFixtureTest(unittest.TestCase):
    def test_refuses_unverified_daemon_before_tls_or_trust_mutations(self):
        calls = []
        def run(*args, **kwargs):
            calls.append(args)
            return types.SimpleNamespace(stdout="different-daemon\n")
        with patch.dict(os.environ, {"COVE_BROWSER_MANAGED_DAEMON_ID": "owned-daemon"}):
            with self.assertRaisesRegex(RuntimeError, "verified owned nested daemon"):
                ManagedRegistryFixture(run, Path("fixture"), "cove-browser-0123456789ab", "openssl", {})
        self.assertEqual(calls, [("docker", "info", "--format", "{{.ID}}")])

    def test_refuses_foreign_worker_or_image_before_commands(self):
        fixture = ManagedRegistryFixture.__new__(ManagedRegistryFixture)
        fixture.name = "cove-browser-0123456789ab"
        fixture.run = lambda *args, **kwargs: self.fail("foreign resource caused Docker execution")
        with self.assertRaisesRegex(RuntimeError, "another worker"):
            fixture.install(fixture.name + "-worker:development", "foreign-worker")
        with self.assertRaisesRegex(RuntimeError, "fixture's worker image"):
            fixture.helper("foreign:test", "true")

    def test_accepts_only_the_canonical_owned_worker_image(self):
        fixture = ManagedRegistryFixture.__new__(ManagedRegistryFixture)
        fixture.name = "cove-browser-0123456789ab"
        fixture.hostname = "registry.example.test"
        fixture.fixture = Path("fixture")
        calls = []
        fixture.run = lambda *args, **kwargs: calls.append(args)
        fixture.helper(fixture.name + "-worker:development", "true")
        self.assertEqual(len(calls), 1)
        self.assertIn("cove.browser-fixture=" + fixture.name, calls[0])
        self.assertIn(fixture.name + "-worker:development", calls[0])
        with self.assertRaisesRegex(RuntimeError, "fixture's worker image"):
            fixture.helper(fixture.name + "-worker:test", "true")
        self.assertEqual(len(calls), 1)

    def test_failed_cleanup_preserves_ownership_intent(self):
        fixture = ManagedRegistryFixture.__new__(ManagedRegistryFixture)
        fixture.installed = True
        def refuse(*args):
            raise RuntimeError("ownership proof failed")
        fixture.helper = refuse
        with self.assertRaisesRegex(RuntimeError, "ownership proof failed"):
            fixture.cleanup("worker:test")
        self.assertTrue(fixture.installed)


if __name__ == "__main__":
    unittest.main()
