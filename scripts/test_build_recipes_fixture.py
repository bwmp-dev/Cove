import json
import os
import subprocess
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from build_recipes_fixture import BUILDKIT_CACHE_ID, BUILDKIT_PIN, install_build_recipes_fixture


class RecipeFixtureTests(unittest.TestCase):
    def setUp(self):
        self.name = "cove-browser-abcdef012345"
        self.worker = self.name + "-worker"
        self.calls = []
        self.identity = "owned-daemon"
        self.cache = BUILDKIT_CACHE_ID
        self.temporary = tempfile.TemporaryDirectory(prefix=self.name)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "registry-ca.pem"
        self.root.write_bytes(b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n")

    def run_command(self, *arguments, **options):
        self.calls.append((arguments, options))
        if arguments[1:3] == ("info", "--format"):
            output = self.identity
        elif arguments[1] == "inspect":
            output = json.dumps([{"Config": {"Image": self.name + "-worker:test"}, "NetworkSettings": {"Networks": {self.name: {}}}}])
        elif arguments[1:3] == ("image", "inspect"):
            output = self.cache
        elif arguments[-1] == "/var/lib/cove/.browser-owner":
            output = self.name
        elif arguments[-1] == "probe":
            output = json.dumps({"ready": True, "build_recipes": True})
        else:
            output = ""
        return subprocess.CompletedProcess(arguments, 0, stdout=output)

    def test_no_foreign_worker_or_daemon_writes(self):
        with patch.dict(os.environ, {"COVE_BROWSER_MANAGED_DAEMON_ID": "different-daemon"}):
            with self.assertRaisesRegex(RuntimeError, "verified owned nested daemon"):
                install_build_recipes_fixture(self.run_command, self.name, self.worker, {})
        self.assertEqual(len(self.calls), 1)
        with self.assertRaisesRegex(RuntimeError, "Owned recipe"):
            install_build_recipes_fixture(self.run_command, self.name, "foreign-worker", {})

    def test_unapproved_cached_builder_refused_before_mutation(self):
        self.cache = "sha256:" + "0" * 64
        with patch.dict(os.environ, {"COVE_BROWSER_MANAGED_DAEMON_ID": self.identity}):
            with self.assertRaisesRegex(RuntimeError, "approved pin"):
                install_build_recipes_fixture(self.run_command, self.name, self.worker, {})
        self.assertFalse(any("--user" in args for args, _ in self.calls))

    def test_synthetic_source_guard_and_pinned_builder(self):
        environment = {"COVE_BROWSER_MANAGED_REGISTRY_ROOT": str(self.root)}
        with patch.dict(os.environ, {"COVE_BROWSER_MANAGED_DAEMON_ID": self.identity}):
            install_build_recipes_fixture(self.run_command, self.name, self.worker, environment)
        raw = [options["input"] for _, options in self.calls if "input" in options]
        self.assertTrue(all(isinstance(value, bytes) and b"\r" not in value for value in raw))
        writes = [value.decode("utf-8") for value in raw]
        self.assertEqual(len(writes), 2)
        self.assertIn("https://git.fixture.invalid/build-recipes.git", writes[0])
        self.assertIn('test "$found" = 1', writes[0])
        self.assertIn(BUILDKIT_PIN, writes[1])
        self.assertNotIn("buildx-stable-1", writes[1])
        self.assertEqual(environment["COVE_BROWSER_BUILD_RECIPES"], "1")


if __name__ == "__main__":
    unittest.main()
