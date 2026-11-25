import json
import os
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

from ci_previews_fixture import install_ci_previews_fixture


class CIPreviewFixtureTests(unittest.TestCase):
    def test_default_certificate_is_in_a_supported_traefik_file(self):
        name = "cove-browser-abcdef123456"
        installed = {}

        def run(*arguments, **options):
            if arguments[:2] == ("docker", "info"):
                return SimpleNamespace(stdout="owned-daemon\n")
            if arguments[:2] == ("docker", "inspect"):
                return SimpleNamespace(stdout=json.dumps([{"Config": {"Labels": {"cove.browser-fixture": name}}}]))
            if arguments[0] == "fixture-openssl":
                for flag in ("-keyout", "-out"):
                    if flag in arguments:
                        Path(arguments[arguments.index(flag) + 1]).write_bytes(b"generated fixture material")
            if "cat" in arguments:
                return SimpleNamespace(stdout=name)
            if "input" in options and arguments[:2] == ("docker", "exec"):
                installed[arguments[-1]] = options["input"]
            return SimpleNamespace(stdout="")

        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"COVE_BROWSER_MANAGED_DAEMON_ID": "owned-daemon"}):
            env = {"COVE_BROWSER_MANAGED_REGISTRY_ROOT": str(Path(directory) / "registry-ca.pem")}
            install_ci_previews_fixture(run, name, name + "-worker", env, directory, "fixture-openssl")
            dynamic = [path for path in installed if path.startswith("/data/dynamic/")]
            self.assertEqual(dynamic, ["/data/dynamic/ci-preview-fixture.yaml"])
            certificate = json.loads(installed[dynamic[0]])["tls"]["stores"]["default"]["defaultCertificate"]
            self.assertEqual(certificate, {"certFile": "/data/ci-preview-fixture.pem", "keyFile": "/data/ci-preview-fixture.key"})
            self.assertEqual(env["COVE_BROWSER_CI_PREVIEWS"], "1")
            self.assertEqual(env["COVE_BROWSER_CI_GIT_CALLS"], "/tmp/ci-preview-git-calls")


if __name__ == "__main__":
    unittest.main()
