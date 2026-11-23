"""Create or apply a signed Cove offline bundle. Docker/Compose, Python and OpenSSL must be preinstalled."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
ASSETS = ('compose.yaml', 'compose.local.yaml', 'deploy/traefik.yaml', 'deploy/traefik-panel.yaml',
          'scripts/configure.sh', 'scripts/update-panel.py', 'scripts/offline-bundle.py')


def run(args, **kwargs):
    result = subprocess.run([str(arg) for arg in args], stderr=subprocess.PIPE, **kwargs)
    if result.returncode:
        raise subprocess.CalledProcessError(result.returncode, [str(arg) for arg in args], output=result.stdout, stderr=result.stderr)
    return result


def updater(root):
    spec = importlib.util.spec_from_file_location('cove_offline_updater', root / 'scripts/update-panel.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def fingerprint(root):
    public_der = run(['openssl', 'pkey', '-pubin', '-in', root / 'trust.pem', '-outform', 'DER'], stdout=subprocess.PIPE).stdout
    return hashlib.sha256(public_der).hexdigest()


def create(destination, version, signing_key, platform="linux/amd64"):
    releases = updater(ROOT)
    images = releases.release_images(version)
    if destination.exists():
        raise ValueError('Destination already exists')
    destination.mkdir(parents=True, mode=0o700)
    for name in ASSETS:
        target = destination / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / name, target)
    images.update({'database': 'postgres:17-alpine', 'proxy': 'traefik:v3.7.19'})
    run(['cosign', 'initialize'], stdout=subprocess.DEVNULL)
    trust_material = Path.home() / '.sigstore/root/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json'
    shutil.copyfile(trust_material, destination / 'sigstore-trusted-root.json')
    identities = {}
    for service, image in images.items():
        run(['docker', 'pull', '--platform', platform, image], stdout=subprocess.DEVNULL)
        if service in ('cove', 'worker'):
            releases.verify_image(image)
            run(['cosign', 'save', image, '--dir', destination / ('signed-image-' + service)], stdout=subprocess.DEVNULL)
        identities[service] = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', image], stdout=subprocess.PIPE).stdout.decode().strip()
    run(['docker', 'save', '-o', destination / 'images.tar', *images.values()])
    container = run(['docker', 'create', images['cove']], stdout=subprocess.PIPE).stdout.decode().strip()
    try:
        run(['docker', 'cp', container + ':/app/bin', destination / 'binaries'])
    finally:
        run(['docker', 'rm', container], stdout=subprocess.DEVNULL)
    for arch in ('amd64', 'arm64'):
        for suffix in ('', '.sigstore.json'):
            if not (destination / 'binaries' / ('cove-worker-linux-' + arch + suffix)).is_file():
                raise ValueError('Signed worker binaries for both architectures are required')
    run(['openssl', 'pkey', '-in', signing_key, '-pubout', '-out', destination / 'trust.pem'])
    files = {str(path.relative_to(destination)).replace('\\', '/'): digest(path)
             for path in destination.rglob('*') if path.is_file()}
    manifest = {'format': 1, 'version': version, 'platform': platform, 'images': identities, 'files': files}
    (destination / 'manifest.json').write_text(json.dumps(manifest, sort_keys=True))
    run(['openssl', 'dgst', '-sha256', '-sign', signing_key, '-out', destination / 'manifest.sig', destination / 'manifest.json'])
    print('Bundle trust fingerprint (verify through an independent channel): ' + fingerprint(destination))


def verify(root, expected_fingerprint):
    if not re.fullmatch(r'[a-f0-9]{64}', expected_fingerprint) or fingerprint(root) != expected_fingerprint:
        raise ValueError('Bundle trust root fingerprint does not match the independently trusted value')
    run(['openssl', 'dgst', '-sha256', '-verify', root / 'trust.pem', '-signature', root / 'manifest.sig', root / 'manifest.json'], stdout=subprocess.DEVNULL)
    manifest = json.loads((root / 'manifest.json').read_text())
    if manifest.get('format') != 1 or set(manifest['images']) != {'cove', 'worker', 'database', 'proxy'}:
        raise ValueError('Unsupported offline bundle')
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?|(?:sha|beta)-[a-f0-9]{40}', manifest.get('version', '')):
        raise ValueError('Invalid offline version')
    if manifest.get('platform') not in ('linux/amd64', 'linux/arm64'):
        raise ValueError('Unsupported image platform')
    for name, expected in manifest['files'].items():
        path = root / name
        if Path(name).is_absolute() or '..' in Path(name).parts or path.is_symlink() or not path.resolve().is_relative_to(root.resolve()) or digest(path) != expected:
            raise ValueError('Offline bundle file verification failed')
    required = {*ASSETS, 'images.tar', 'trust.pem', 'sigstore-trusted-root.json'}
    for arch in ('amd64', 'arm64'):
        required.update({'binaries/cove-worker-linux-' + arch, 'binaries/cove-worker-linux-' + arch + '.sigstore.json'})
    if not required.issubset(manifest['files']) or any(not re.fullmatch(r'sha256:[a-f0-9]{64}', value) for value in manifest['images'].values()):
        raise ValueError('Incomplete offline bundle')
    return manifest


def load_images(root, manifest):
    arch = run(['docker', 'info', '--format', '{{.Architecture}}'], stdout=subprocess.PIPE).stdout.decode().strip()
    expected = 'arm64' if arch in ('arm64', 'aarch64') else 'amd64' if arch in ('amd64', 'x86_64') else ''
    if manifest['platform'] != 'linux/' + expected:
        raise ValueError('Offline bundle platform does not match Docker host')
    run(['docker', 'load', '-i', root / 'images.tar'], stdout=subprocess.DEVNULL)
    for image in manifest['images'].values():
        actual = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', image], stdout=subprocess.PIPE).stdout.decode().strip()
        if actual != image:
            raise ValueError('Offline image identity mismatch')


def install(root, manifest, installation, domain, certificate, private_key, bind_address, local_worker=False, base_path=""):
    if not re.fullmatch(r'(?:/[A-Za-z0-9_-]+)*', base_path) or len(base_path) > 200 or base_path == '/healthz':
        raise ValueError('A simple base path without a trailing slash is required')
    if installation.exists():
        raise ValueError('Installation exists; use update')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]', domain):
        raise ValueError('A valid dashboard DNS hostname is required')
    run(['openssl', 'x509', '-in', certificate, '-noout', '-checkhost', domain, '-checkend', '0'], stdout=subprocess.DEVNULL)
    cert_pub = run(['openssl', 'x509', '-in', certificate, '-pubkey', '-noout'], stdout=subprocess.PIPE).stdout
    key_pub = run(['openssl', 'pkey', '-in', private_key, '-pubout'], stdout=subprocess.PIPE).stdout
    if cert_pub != key_pub:
        raise ValueError('TLS certificate and key do not match')
    installation.mkdir(parents=True, mode=0o700)
    for name in ASSETS:
        target = installation / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(root / name, target)
    tls_dir = installation / 'tls'
    tls_dir.mkdir(mode=0o700)
    for source, name in [(certificate, 'panel.crt'), (private_key, 'panel.key')]:
        shutil.copyfile(source, tls_dir / name)
        (tls_dir / name).chmod(0o600)
    dynamic = (installation / 'deploy/traefik-panel.yaml').read_text().replace('tls:\n        certResolver: cove', 'tls: {}')
    dynamic += '\ntls:\n  certificates:\n    - certFile: /etc/cove-tls/panel.crt\n      keyFile: /etc/cove-tls/panel.key\n'
    (installation / 'deploy/traefik-panel.yaml').write_text(dynamic)
    run(['sh', installation / 'scripts/configure.sh', domain, base_path, bind_address])
    override = {'services': {service: {'image': image, 'pull_policy': 'never'} for service, image in manifest['images'].items() if service != 'worker' or local_worker}}
    override['services']['proxy']['volumes'] = ['./tls:/etc/cove-tls:ro']
    (installation / 'compose.offline.json').write_text(json.dumps(override))
    with (installation / '.env').open('a') as stream:
        stream.write('\nCOMPOSE_FILE=compose.yaml:' + ('compose.local.yaml:' if local_worker else '') + 'compose.offline.json\nCOVE_VERSION=' + manifest['version'] + '\n')
    run(['docker', 'compose', '--project-directory', installation, 'up', '-d', '--pull', 'never', '--no-build', '--wait', '--wait-timeout', '180'], cwd=installation)
    print('Offline panel installed. Configure private DNS and trust the supplied TLS issuer before first-user setup. No Internet update timer was enabled.')


def apply_update(root, manifest, installation, confirm_downgrade=False):
    previous_directory = Path.cwd()
    try:
        updater(root).update(installation, manifest['version'], manifest['images'], confirm_downgrade)
    finally:
        os.chdir(previous_directory)
    override_path = installation / 'compose.offline.json'
    if override_path.exists():
        override = json.loads(override_path.read_text())
        for service in ('cove', 'worker'):
            if service in override['services']:
                override['services'][service]['image'] = manifest['images'][service]
        temporary = override_path.with_suffix('.next')
        temporary.write_text(json.dumps(override))
        os.replace(temporary, override_path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['create', 'verify', 'install', 'update', 'trust-worker'])
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--version')
    parser.add_argument('--platform', choices=['linux/amd64', 'linux/arm64'], default='linux/amd64')
    parser.add_argument('--signing-key', type=Path)
    parser.add_argument('--trust-fingerprint')
    parser.add_argument('--installation', type=Path, default=Path('/opt/cove'))
    parser.add_argument('--domain')
    parser.add_argument('--tls-certificate', type=Path)
    parser.add_argument('--tls-key', type=Path)
    parser.add_argument('--confirm-downgrade', action='store_true')
    parser.add_argument('--local-worker', action='store_true')
    parser.add_argument('--bind-address', default='127.0.0.1')
    parser.add_argument('--base-path', default='')
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.command == 'create':
            if not args.version or not args.signing_key:
                parser.error('create requires --version and --signing-key')
            create(args.bundle.resolve(), args.version, args.signing_key.resolve(strict=True), args.platform)
            return
        root = args.bundle.resolve(strict=True)
        manifest = verify(root, args.trust_fingerprint or '')
        if args.command == 'verify':
            print('Offline signatures and checksums verified')
            return
        if sys.platform != 'linux' or os.geteuid() != 0:
            parser.error('install/update require root on the Linux Docker host')
        if args.command == 'trust-worker':
            directory = Path('/etc/cove')
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            target = directory / 'sigstore-trusted-root.json'
            if target.exists():
                raise ValueError('Existing worker trust material must be reviewed before replacement')
            shutil.copyfile(root / 'sigstore-trusted-root.json', target)
            target.chmod(0o600)
            print('Worker trust root prepared. Run the private panel enrollment command with COVE_OFFLINE=1 and preinstalled prerequisites/Cosign; no public Sigstore service is needed.')
            return
        load_images(root, manifest)
        if args.command == 'install':
            if not args.domain or not args.tls_certificate or not args.tls_key:
                parser.error('install requires --domain, --tls-certificate and --tls-key')
            install(root, manifest, args.installation.resolve(), args.domain, args.tls_certificate.resolve(strict=True), args.tls_key.resolve(strict=True), args.bind_address, args.local_worker, args.base_path)
        else:
            installation = args.installation.resolve(strict=True)
            apply_update(root, manifest, installation, args.confirm_downgrade)
    except (RuntimeError, ValueError) as error:
        sys.exit('Offline operation failed: ' + str(error))
    except (OSError, KeyError, subprocess.CalledProcessError):
        sys.exit('Offline operation failed. No verification bypass is available; inspect the bundle, trust fingerprint and host prerequisites.')


if __name__ == '__main__':
    main()
