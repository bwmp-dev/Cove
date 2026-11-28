from datetime import datetime, timedelta, timezone
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import scanner_fixture


class ScannerFixtureTests(unittest.TestCase):
    def archive(self, directory, *, stale=False, extra=None):
        now = datetime.now(timezone.utc)
        metadata = json.dumps({"Version": 2, "UpdatedAt": (now - timedelta(hours=48 if stale else 1)).isoformat(), "NextUpdate": (now + timedelta(hours=2)).isoformat()}).encode()
        path = Path(directory) / "database.tar"
        with tarfile.open(path, "w") as output:
            for name, data in [("db/trivy.db", b"fixture-database"), ("db/metadata.json", metadata)]:
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                output.addfile(entry, io.BytesIO(data))
            if extra:
                output.addfile(extra)
        return path, metadata

    def test_seed_binds_fresh_main_database_to_content_hash_and_owned_state(self):
        with tempfile.TemporaryDirectory() as directory:
            archive, metadata = self.archive(directory)
            calls = []
            def run(*args, **kwargs):
                calls.append((args, kwargs))
                self.assertEqual(kwargs["stdin"].read(), archive.read_bytes())
            scanner_fixture.seed_scanner_database(run, "owned-worker:test", "cove-browser-fixture", archive)
        self.assertEqual(len(calls), 1)
        args, kwargs = calls[0]
        joined = " ".join(args)
        for required in ["--network none", "--read-only", "--cap-drop ALL", "--user cove", "cove.browser-fixture=cove-browser-fixture", ".browser-owner", "test ! -e /var/lib/cove/scanner"]:
            self.assertIn(required, joined)
        current = json.loads(args[-1])
        digest = hashlib.sha256(metadata + b"fixture-database").hexdigest()
        self.assertEqual(current["digest"], "sha256:" + digest)
        self.assertEqual(current["path"], "db-" + digest)
        self.assertLessEqual(kwargs["timeout"], 180)

    def test_refuses_stale_extra_duplicate_and_link_entries_before_docker(self):
        for kind in ["stale", "java", "duplicate", "link"]:
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                extra = None
                if kind != "stale":
                    extra = tarfile.TarInfo("db/trivy.db" if kind == "duplicate" else "java-db/trivy-java.db")
                    if kind == "link":
                        extra.type = tarfile.SYMTYPE
                        extra.linkname = "/foreign"
                archive, _ = self.archive(directory, stale=kind == "stale", extra=extra)
                with self.assertRaises(RuntimeError):
                    scanner_fixture.seed_scanner_database(lambda *args, **kwargs: self.fail("invalid fixture reached Docker"), "worker", "fixture", archive)

    def test_refuses_budget_overflow_before_docker(self):
        with tempfile.TemporaryDirectory() as directory:
            archive, _ = self.archive(directory)
            with patch.object(scanner_fixture, "DATABASE_LIMIT", 1), self.assertRaises(RuntimeError):
                scanner_fixture.seed_scanner_database(lambda *args, **kwargs: self.fail("oversized fixture reached Docker"), "worker", "fixture", archive)


if __name__ == "__main__":
    unittest.main()
