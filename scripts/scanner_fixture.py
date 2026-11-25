"""Seed a verified main vulnerability database into this browser fixture only."""
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import tarfile


SCANNER_PIN = "docker.io/aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb"
DATABASE_LIMIT = 2 * 1024 * 1024 * 1024


def seed_scanner_database(run, worker_image, fixture, archive):
    archive = Path(archive)
    if not archive.is_file() or archive.stat().st_size > DATABASE_LIMIT + 1024 * 1024:
        raise RuntimeError("A bounded cached main scanner database archive is required")
    with tarfile.open(archive, "r:") as source:
        entries = {}
        for item in source:
            name = item.name.removeprefix("./")
            if item.isdir() and name in (".", "db"):
                continue
            if name not in ("db/trivy.db", "db/metadata.json") or name in entries or not item.isfile() or item.size <= 0:
                raise RuntimeError("Scanner fixture archive contains unsupported entries")
            entries[name] = item
        if set(entries) != {"db/trivy.db", "db/metadata.json"} or sum(item.size for item in entries.values()) > DATABASE_LIMIT or entries["db/metadata.json"].size > 16384:
            raise RuntimeError("Scanner fixture main database is incomplete or oversized")
        metadata_bytes = source.extractfile(entries["db/metadata.json"]).read(16385)
        metadata = json.loads(metadata_bytes)
        now = datetime.now(timezone.utc)
        updated = datetime.fromisoformat(metadata["UpdatedAt"].replace("Z", "+00:00"))
        next_update = datetime.fromisoformat(metadata["NextUpdate"].replace("Z", "+00:00"))
        if metadata["Version"] != 2 or not now - timedelta(hours=24) <= updated <= now or next_update <= now:
            raise RuntimeError("Scanner fixture database is stale, future-dated or unsupported")
        digest = hashlib.sha256(metadata_bytes)
        with source.extractfile(entries["db/trivy.db"]) as database:
            while chunk := database.read(1024 * 1024):
                digest.update(chunk)
    name = "db-" + digest.hexdigest()
    current = json.dumps({"digest": "sha256:" + digest.hexdigest(), "updated_at": metadata["UpdatedAt"], "next_update": metadata["NextUpdate"], "path": name})
    script = ('test "$(cat /var/lib/cove/.browser-owner)" = "$1"; '
              'test ! -e /var/lib/cove/scanner; umask 077; mkdir -p "/var/lib/cove/scanner/$2"; '
              'tar -xf - -C "/var/lib/cove/scanner/$2"; '
              'chmod 444 "/var/lib/cove/scanner/$2/db/trivy.db" "/var/lib/cove/scanner/$2/db/metadata.json"; '
              'chmod 555 "/var/lib/cove/scanner/$2/db"; '
              'printf %s "$3" > /var/lib/cove/scanner/current.json')
    with archive.open("rb") as input_file:
        run("docker", "run", "--rm", "-i", "--pull", "never", "--network", "none", "--read-only",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--user", "cove",
            "--memory", "128m", "--cpus", "1", "--pids-limit", "32", "--label", "cove.browser-fixture=" + fixture,
            "--mount", "type=bind,source=/var/lib/cove,target=/var/lib/cove", "--entrypoint", "sh", worker_image,
            "-ec", script, "sh", fixture, name, current, stdin=input_file, timeout=180)
    print("Seeded verified main scanner database; no Java index or public refresh", flush=True)
