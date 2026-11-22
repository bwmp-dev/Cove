"""Offline verification tests use disposable fixture keys; no release credentials are used."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('offline_bundle', Path(__file__).with_name('offline-bundle.py'))
bundle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundle)


class OfflineBundleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / 'bundle'
        self.root.mkdir()
        self.key = self.root.parent / 'fixture-private-key.pem'
        subprocess.run(['openssl', 'genpkey', '-algorithm', 'RSA', '-pkeyopt', 'rsa_keygen_bits:2048', '-out', str(self.key)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        bundle.run(['openssl', 'pkey', '-in', self.key, '-pubout', '-out', self.root / 'trust.pem'])
        for name in bundle.ASSETS:
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(bundle.ROOT / name, target)
        (self.root / 'images.tar').write_bytes(b'fixture image archive')
        (self.root / 'sigstore-trusted-root.json').write_text('{}')
        (self.root / 'binaries').mkdir()
        for arch in ('amd64', 'arm64'):
            for suffix in ('', '.sigstore.json'):
                (self.root / 'binaries' / ('cove-worker-linux-' + arch + suffix)).write_bytes(b'fixture')
        self.manifest = {'format': 1, 'version': 'v1.2.3', 'platform': 'linux/amd64',
                         'images': {name: 'sha256:' + 'a' * 64 for name in ('cove', 'worker', 'database', 'proxy')},
                         'files': {str(path.relative_to(self.root)): bundle.digest(path)
                                   for path in self.root.rglob('*') if path.is_file()}}
        self.sign()
        self.fingerprint = bundle.fingerprint(self.root)

    def sign(self):
        (self.root / 'manifest.json').write_text(json.dumps(self.manifest))
        bundle.run(['openssl', 'dgst', '-sha256', '-sign', self.key, '-out', self.root / 'manifest.sig', self.root / 'manifest.json'])

    def test_verifies_without_network(self):
        self.assertEqual(bundle.verify(self.root, self.fingerprint)['version'], 'v1.2.3')

    def test_rejects_substituted_trust_root(self):
        with self.assertRaises(ValueError):
            bundle.verify(self.root, '0' * 64)

    def test_rejects_changed_image_archive(self):
        (self.root / 'images.tar').write_bytes(b'tampered')
        with self.assertRaises(ValueError):
            bundle.verify(self.root, self.fingerprint)

    def test_rejects_changed_manifest_signature(self):
        self.manifest['version'] = 'v9.9.9'
        (self.root / 'manifest.json').write_text(json.dumps(self.manifest))
        with self.assertRaises(RuntimeError):
            bundle.verify(self.root, self.fingerprint)

    def test_rejects_missing_worker_binary(self):
        del self.manifest['files']['binaries/cove-worker-linux-arm64']
        self.sign()
        with self.assertRaises(ValueError):
            bundle.verify(self.root, self.fingerprint)

    def test_rejects_path_traversal(self):
        self.manifest['files']['../fixture-private-key.pem'] = bundle.digest(self.key)
        self.sign()
        with self.assertRaises(ValueError):
            bundle.verify(self.root, self.fingerprint)

    def test_load_only_uses_local_images(self):
        calls = []
        def run(args, **kwargs):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, stdout=b'amd64' if args[:2] == ['docker', 'info'] else ('sha256:' + 'a' * 64).encode())
        with patch.object(bundle, 'run', side_effect=run):
            bundle.load_images(self.root, self.manifest)
        self.assertEqual(calls[1][:2], ['docker', 'load'])
        self.assertTrue(all(call[:2] in (['docker', 'load'], ['docker', 'image'], ['docker', 'info']) for call in calls))

    @unittest.skipUnless(os.environ.get('COVE_OFFLINE_DOCKER_FIXTURE'), 'Requires a prepared local fixture image and Docker socket')
    def test_network_isolated_docker_load(self):
        image = os.environ['COVE_OFFLINE_DOCKER_FIXTURE']
        identity = bundle.run(['docker', 'image', 'inspect', '--format', '{{.Id}}', image], stdout=subprocess.PIPE).stdout.decode().strip()
        bundle.run(['docker', 'save', '-o', self.root / 'images.tar', image])
        self.manifest['images'] = {name: identity for name in ('cove', 'worker', 'database', 'proxy')}
        self.manifest['files']['images.tar'] = bundle.digest(self.root / 'images.tar')
        self.sign()
        verified = bundle.verify(self.root, self.fingerprint)
        bundle.load_images(self.root, verified)


if __name__ == '__main__':
    unittest.main()
