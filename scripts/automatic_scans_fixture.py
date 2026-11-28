"""Change only the owned offline browser fixture's scanner cache identity."""
import os
import re
import subprocess
import sys


def change_cache(action):
    worker = os.environ.get("COVE_BROWSER_WORKER", "")
    match = re.fullmatch(r"(cove-browser-[a-f0-9]{12})-worker", worker)
    if not match or os.environ.get("COVE_BROWSER_DATABASE") != match[1] + "-db":
        raise RuntimeError("An owned scanner browser fixture is required")
    if os.environ.get("COVE_BROWSER_IMAGE_SCANS") != "1":
        raise RuntimeError("The scanner fixture must be explicitly enabled")
    scripts = {
        # Whitespace changes the verified bundle identity, not database contents or freshness.
        "rotate": r'''
set -eu
root=/var/lib/cove/scanner
current=$(cat "$root/current.json")
old=$(printf %s "$current" | sed -n 's/.*"path"[[:space:]]*:[[:space:]]*"\(db-[a-f0-9]*\)".*/\1/p')
test "${#old}" = 67
test -f "$root/$old/db/trivy.db"
temporary="$root/fixture-metadata.tmp"
cat "$root/$old/db/metadata.json" > "$temporary"
printf '\n' >> "$temporary"
digest=$(cat "$temporary" "$root/$old/db/trivy.db" | sha256sum | cut -d ' ' -f1)
test "${#digest}" = 64
next="db-$digest"
test ! -e "$root/$next"
mkdir -p "$root/$next/db"
ln "$root/$old/db/trivy.db" "$root/$next/db/trivy.db"
mv "$temporary" "$root/$next/db/metadata.json"
chmod 444 "$root/$next/db/metadata.json"
chmod 555 "$root/$next/db"
printf %s "$current" | sed "s/$old/$next/g; s/sha256:[a-f0-9]*/sha256:$digest/g" > "$root/fixture-current.tmp"
mv "$root/fixture-current.tmp" "$root/current.json"
printf 'sha256:%s\n' "$digest"
''',
        "missing": r'''
set -eu
root=/var/lib/cove/scanner
test ! -e "$root/fixture-current.saved"
mv "$root/current.json" "$root/fixture-current.saved"
test ! -e "$root/automatic-refresh.json" || mv "$root/automatic-refresh.json" "$root/fixture-refresh.saved"
printf '%s' '{"next_attempt":"2099-01-01T00:00:00Z","message":"Offline fixture: missing database, refresh cooldown held; no public fetch."}' > "$root/automatic-refresh.json"
''',
        "restore": r'''
set -eu
root=/var/lib/cove/scanner
test -f "$root/fixture-current.saved"
mv "$root/fixture-current.saved" "$root/current.json"
if test -e "$root/fixture-refresh.saved"; then
 mv "$root/fixture-refresh.saved" "$root/automatic-refresh.json"
else
 rm "$root/automatic-refresh.json"
fi
''',
    }
    if action not in scripts:
        raise ValueError("Expected rotate, missing or restore")
    script = 'test "$(cat /var/lib/cove/.browser-owner)" = "$1"; ' + scripts[action]
    result = subprocess.run(
        ["docker", "exec", "-u", "cove", worker, "sh", "-ec", script, "sh", match[1]],
        check=True, capture_output=True, text=True, timeout=60,
    )
    return result.stdout.strip()


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("Usage: automatic_scans_fixture.py rotate|missing|restore")
    print(change_cache(sys.argv[1]))
