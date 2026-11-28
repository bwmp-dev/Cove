import subprocess
import tempfile
import unittest
from unittest.mock import patch

from build_nodes_fixture import BuildNodesFixture


class BuildNodeFixtureTests(unittest.TestCase):
    def test_refuses_foreign_name(self):
        with self.assertRaises(RuntimeError):
            BuildNodesFixture(None, "foreign-worker", ".", {}, [])

    def test_refuses_unverified_daemon(self):
        def run(*args, **kwargs):
            return subprocess.CompletedProcess(args, 0, "other-daemon\n", "")

        with tempfile.TemporaryDirectory() as directory:
            fixture = BuildNodesFixture(run, "cove-browser-0123456789ab", directory, {"COVE_BROWSER_MANAGED_DAEMON_ID": "owned-daemon"}, [])
            with self.assertRaisesRegex(RuntimeError, "verified owned"):
                fixture.start("cove-browser-0123456789ab-worker:development", None, "registry-0123456789ab.example.test")

    def test_refuses_unapproved_cache_before_creating_resources(self):
        calls = []

        def run(*args, **kwargs):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, "owned-daemon\n" if args[1] == "info" else "different-image\n", "")

        with tempfile.TemporaryDirectory() as directory:
            fixture = BuildNodesFixture(run, "cove-browser-0123456789ab", directory, {"COVE_BROWSER_MANAGED_DAEMON_ID": "owned-daemon"}, [])
            with self.assertRaisesRegex(RuntimeError, "approved artifact"):
                fixture.start("cove-browser-0123456789ab-worker:development", None, "registry-0123456789ab.example.test")
        self.assertEqual(len(calls), 2)

    def test_cleanup_rechecks_ownership(self):
        with patch("build_nodes_fixture.subprocess.run", return_value=subprocess.CompletedProcess([], 0, "foreign-owner\n", "")):
            fixture = BuildNodesFixture(None, "cove-browser-0123456789ab", ".", {}, [])
            with self.assertRaisesRegex(RuntimeError, "ownership changed"):
                fixture.cleanup()


if __name__ == "__main__":
    unittest.main()
