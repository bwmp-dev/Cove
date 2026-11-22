import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("audit", Path(__file__).with_name("audit-coolify-host.py"))
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


class AuditTests(unittest.TestCase):
    def test_secrets_and_host_paths_are_not_exported(self):
        result = audit.summarize({
            "Name": "/database", "Config": {"Image": "postgres:17", "Env": ["PASSWORD=secret"],
            "Cmd": ["--password", "secret"], "Labels": {"secret": "secret", "traefik.http.middlewares.auth": "secret"}},
            "Mounts": [{"Type": "bind", "Source": "/private/secret", "Destination": "/data", "RW": True}],
            "State": {"Status": "exited"}, "HostConfig": {"RestartPolicy": {"Name": "unless-stopped"}},
        })
        self.assertNotIn("secret", json.dumps(result))
        self.assertEqual(result["state"], "exited")
        self.assertIn("preserve-database-version-and-test-restore", result["checks"])
        self.assertIn("replace-coolify-ingress-labels", result["checks"])
        self.assertIn("transfer-bind-mounts-and-permissions", result["checks"])

    def test_host_requirements_and_shared_networks(self):
        result = audit.summarize({"HostConfig": {"NetworkMode": "host", "CapAdd": ["NET_ADMIN"],
            "PortBindings": {"443/tcp": [{"HostPort": "443"}]}},
            "Mounts": [{"Type": "bind", "Destination": "/var/run/docker.sock", "RW": False}],
            "NetworkSettings": {"Networks": {"shared": {}, "private": {}}}})
        self.assertEqual(result["networks"], ["private", "shared"])
        for check in ("docker-socket-host-access", "host-network-port-conflicts",
                      "review-host-privileges-and-devices", "review-published-ports-and-firewall"):
            self.assertIn(check, result["checks"])

    def test_sidecars_ephemeral_storage_and_completed_jobs(self):
        result = audit.summarize({"HostConfig": {"NetworkMode": "container:opaque-id"},
            "Mounts": [{"Type": "tmpfs", "Destination": "/data", "RW": True}],
            "State": {"Status": "exited", "ExitCode": 0}})
        self.assertEqual(result["exit_code"], 0)
        self.assertIn("preserve-shared-network-namespace", result["checks"])
        self.assertIn("review-ephemeral-storage-and-recovery", result["checks"])
        self.assertNotIn("restore-test-persistent-data", result["checks"])


if __name__ == "__main__":
    unittest.main()
