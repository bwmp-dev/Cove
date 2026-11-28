import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


WORKER = '#!' + sys.executable + '\n' + '''
import json, os, pathlib, sys
root = pathlib.Path(os.environ['TEST_ROOT'])
method = json.load(sys.stdin)['method']
with (root / 'rpc.log').open('a') as log: log.write(method + '\\n')
if method == 'probe':
 print(json.dumps({'probe': {'ready': not (os.environ.get('FAIL_READY') and '# NEW' in pathlib.Path(__file__).read_text())}}))
else:
 marker = root / 'state/draining.json'
 if method == 'drain': marker.touch()
 elif marker.exists(): marker.unlink()
 print(json.dumps({'maintenance': {'draining': method == 'drain', 'active': 0}}))
'''

COMMAND = '#!' + sys.executable + '\n' + '''
import hashlib, json, os, pathlib, sys
root = pathlib.Path(os.environ['TEST_ROOT'])
name = pathlib.Path(sys.argv[0]).name
if name == 'curl':
 data = (root / 'new-worker').read_bytes()
 url = sys.argv[-3]
 if url.endswith('/amd64') or url.endswith('/arm64'):
  data = json.dumps({'sha256': '0' * 64 if os.environ.get('BAD_HASH') else hashlib.sha256(data).hexdigest()}).encode()
 pathlib.Path(sys.argv[-1]).write_bytes(data)
elif name == 'systemctl':
 with (root / 'systemctl.log').open('a') as log: log.write(' '.join(sys.argv[1:]) + '\\n')
elif name == 'cosign':
 if os.environ.get('BAD_SIGNATURE'): sys.exit(1)
elif name == 'id':
 print('0')
'''


@unittest.skipUnless(os.name == 'posix', 'Requires Linux shell, jq and flock')
class WorkerUpdateTests(unittest.TestCase):
    def exercise(self, fail_ready=False, bad_hash=False, maintenance=False, unchanged=False, bad_signature=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for folder in ['bin', 'run', 'state']:
                (root / folder).mkdir()
            previous = WORKER + '# OLD\n'
            replacement = previous if unchanged else WORKER + '# NEW\n'
            for name, content in [('bin/cove-worker', previous), ('new-worker', replacement),
                                  ('bin/curl', COMMAND), ('bin/systemctl', COMMAND), ('bin/id', COMMAND), ('bin/cosign', COMMAND), ('bin/sleep', '#!/bin/sh\nexit 0\n')]:
                path = root / name
                path.write_text(content)
                path.chmod(0o755)
            if maintenance:
                (root / 'state/draining.json').touch()
            script = Path(__file__).resolve().parents[1].joinpath('internal/control/worker-update.sh.tmpl').read_text()
            script = script.replace('{{.URL}}', 'https://panel.example.com').replace('/usr/local/bin', str(root / 'bin')).replace('/run/cove-worker-update.lock', str(root / 'run/lock')).replace('/var/lib/cove', str(root / 'state'))
            env = {**os.environ, 'TEST_ROOT': str(root), 'PATH': str(root / 'bin') + ':' + os.environ['PATH']}
            if fail_ready: env['FAIL_READY'] = '1'
            if bad_hash: env['BAD_HASH'] = '1'
            if bad_signature: env['BAD_SIGNATURE'] = '1'
            result = subprocess.run(['sh'], input=script, text=True, capture_output=True, env=env, timeout=30)
            self.assertEqual(result.returncode, 1 if fail_ready or bad_hash or bad_signature else 0, result.stderr + result.stdout)
            expected = previous if fail_ready or bad_hash or bad_signature or maintenance or unchanged else replacement
            self.assertEqual((root / 'bin/cove-worker').read_text(), expected)
            self.assertEqual((root / 'state/draining.json').exists(), maintenance)
            methods = (root / 'rpc.log').read_text().splitlines() if (root / 'rpc.log').exists() else []
            if bad_hash or bad_signature or maintenance or unchanged:
                self.assertEqual(methods, [])
            else:
                self.assertEqual(methods[0], 'drain')
                self.assertEqual(methods[-1], 'resume')
                self.assertIn('probe', methods)

    def test_bad_signature_leaves_worker_alone(self): self.exercise(bad_signature=True)

    def test_replaces_and_resumes(self): self.exercise()
    def test_failed_readiness_rolls_back(self): self.exercise(fail_ready=True)
    def test_checksum_mismatch_leaves_worker_alone(self): self.exercise(bad_hash=True)
    def test_preserves_existing_maintenance(self): self.exercise(maintenance=True)
    def test_matching_binary_does_not_restart(self): self.exercise(unchanged=True)


if __name__ == '__main__':
    unittest.main()
