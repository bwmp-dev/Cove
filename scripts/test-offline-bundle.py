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
        with self.assertRaises(subprocess.CalledProcessError):
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

    @unittest.skipUnless(os.environ.get('COVE_OFFLINE_INSTALL_FIRST'), 'Requires prepared control-plane images and a shared fixture volume')
    def test_network_isolated_install_and_update(self):
        first = os.environ['COVE_OFFLINE_INSTALL_FIRST']
        second = os.environ['COVE_OFFLINE_INSTALL_NEXT']
        worker = os.environ['COVE_OFFLINE_INSTALL_WORKER']
        namespace = 'cove-offline-' + self.root.parent.name.lower()
        previous_project = os.environ.get('COMPOSE_PROJECT_NAME')
        os.environ['COMPOSE_PROJECT_NAME'] = namespace
        installation = self.root.parent / 'installation'
        certificate, key = self.root.parent / 'fixture.crt', self.root.parent / 'fixture-tls.key'
        bundle.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key,
                    '-out', certificate, '-days', '1', '-subj', '/CN=cove.offline.test',
                    '-addext', 'subjectAltName=DNS:cove.offline.test'], stdout=subprocess.DEVNULL)
        compose_path = self.root / 'compose.yaml'
        compose = compose_path.read_text().replace('cove-ingress', namespace)
        compose = compose.replace(':80:80"', '::80"').replace(':443:443"', '::443"')
        compose = compose.replace('  ingress:\n    name:', '  ingress:\n    internal: true\n    name:')
        compose += '\n  default:\n    internal: true\n'
        compose_path.write_text(compose)

        def prepare(panel, version):
            images = {'cove': panel, 'worker': worker, 'database': 'postgres:17-alpine', 'proxy': 'traefik:v3.7.19'}
            self.manifest['images'] = {name: bundle.run(['docker', 'image', 'inspect', '--format', '{{.Id}}', image], stdout=subprocess.PIPE).stdout.decode().strip()
                                       for name, image in images.items()}
            self.manifest['version'] = version
            bundle.run(['docker', 'save', '-o', self.root / 'images.tar', *images.values()])
            self.manifest['files'] = {str(path.relative_to(self.root)): bundle.digest(path)
                                      for path in self.root.rglob('*') if path.is_file() and path.name not in ('manifest.json', 'manifest.sig')}
            self.sign()

        compose_command = ['docker', 'compose', '--project-directory', installation, '-f', installation / 'compose.yaml', '-f', installation / 'compose.offline.json']
        try:
            prepare(first, 'v9.9.9')
            verified = bundle.verify(self.root, self.fingerprint)
            bundle.load_images(self.root, verified)
            bundle.install(self.root, verified, installation, 'cove.offline.test', certificate, key, '127.0.0.1')
            panel = bundle.run([*compose_command, 'ps', '-q', 'cove'], stdout=subprocess.PIPE).stdout.decode().strip()
            config = json.loads(bundle.run(['docker', 'exec', panel, 'wget', '-qO-', 'http://127.0.0.1:8080/auth/config'], stdout=subprocess.PIPE).stdout)
            self.assertTrue(config['setup_required'])
            containers = bundle.run([*compose_command, 'ps', '-q'], stdout=subprocess.PIPE).stdout.decode().split()
            for container in containers:
                networks = json.loads(bundle.run(['docker', 'inspect', '--format', '{{json .NetworkSettings.Networks}}', container], stdout=subprocess.PIPE).stdout)
                for network in networks:
                    internal = bundle.run(['docker', 'network', 'inspect', '--format', '{{.Internal}}', network], stdout=subprocess.PIPE).stdout.decode().strip()
                    self.assertEqual(internal, 'true')
            client = os.environ['COVE_OFFLINE_INSTALL_CLIENT']
            tls_check = "import socket,ssl; c=ssl.create_default_context(cafile='/cert.pem'); s=c.wrap_socket(socket.create_connection(('proxy',443),timeout=10),server_hostname='cove.offline.test'); s.sendall(b'GET /healthz HTTP/1.1\\r\\nHost: cove.offline.test\\r\\nConnection: close\\r\\n\\r\\n'); r=s.recv(4096); assert b' 200 ' in r.split(b'\\r\\n',1)[0],r; s.close()"
            bundle.run(['docker', 'run', '--rm', '--pull', 'never', '--network', namespace,
                        '--label', 'cove.test=offline-tls', '-v', str(certificate) + ':/cert.pem:ro',
                        client, 'python', '-c', tls_check], stdout=subprocess.PIPE)
            bundle.run([*compose_command, 'exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-v', 'ON_ERROR_STOP=1', '-c',
                        "INSERT INTO projects(id,name,team_id) VALUES('offline-fixture','Offline fixture','default')"], stdout=subprocess.PIPE)
            prepare(second, 'v9.9.10')
            verified = bundle.verify(self.root, self.fingerprint)
            bundle.load_images(self.root, verified)
            bundle.apply_update(self.root, verified, installation)
            panel = bundle.run([*compose_command, 'ps', '-q', 'cove'], stdout=subprocess.PIPE).stdout.decode().strip()
            version = bundle.run(['docker', 'inspect', '--format', '{{index .Config.Labels "org.opencontainers.image.version"}}', panel], stdout=subprocess.PIPE).stdout.decode().strip()
            self.assertEqual(version, 'v9.9.10')
            count = bundle.run([*compose_command, 'exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-Atc',
                                "SELECT count(*) FROM projects WHERE id='offline-fixture'"], stdout=subprocess.PIPE).stdout.decode().strip()
            self.assertEqual(count, '1')
            self.assertTrue(list((installation / '.cove-updates').glob('*/database.dump')))
            pins = json.loads((installation / 'compose.offline.json').read_text())
            self.assertEqual(pins['services']['cove']['image'], self.manifest['images']['cove'])
        except subprocess.CalledProcessError as error:
            message = (error.stderr or b'').decode(errors='replace')
            if (installation / '.env').exists():
                for line in (installation / '.env').read_text().splitlines():
                    if '=' in line:
                        value = line.split('=', 1)[1]
                        if len(value) >= 8:
                            message = message.replace(value, '[redacted]')
            self.fail('Offline fixture command failed: ' + message[-4000:])
        finally:
            if (installation / '.env').exists():
                bundle.run([*compose_command, 'down', '--volumes', '--remove-orphans'], stdout=subprocess.PIPE)
            if previous_project is None:
                os.environ.pop('COMPOSE_PROJECT_NAME', None)
            else:
                os.environ['COMPOSE_PROJECT_NAME'] = previous_project


if __name__ == '__main__':
    unittest.main()
