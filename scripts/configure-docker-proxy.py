"""Explicitly configure dockerd proxy settings from a worker's authenticated proxy settings."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--settings', type=Path, default=Path('/var/lib/cove/outbound-proxy.json'))
    parser.add_argument('--apply', action='store_true', help='Explicitly approve writing daemon.json; requires a later operator-approved Docker restart')
    args = parser.parse_args()
    if not args.apply:
        parser.error('No host changes made. Pass --apply only during an approved maintenance window.')
    if os.name != 'posix' or os.geteuid() != 0:
        parser.error('Run as root on the Linux Docker host')
    os.umask(0o077)
    settings = json.loads(args.settings.read_text())
    path = Path('/etc/docker/daemon.json')
    if path.is_symlink():
        parser.error('Refusing a symlinked daemon configuration')
    original = path.read_bytes() if path.exists() else b'{}'
    config = json.loads(original)
    config['proxies'] = {key: settings[source] for key, source in
                         [('http-proxy', 'http_proxy'), ('https-proxy', 'https_proxy'), ('no-proxy', 'no_proxy')]}
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as stream:
        temporary = Path(stream.name)
        stream.write(json.dumps(config).encode())
    try:
        subprocess.run(['dockerd', '--validate', '--config-file', str(temporary)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        backup = path.with_name('daemon.json.cove-before-proxy')
        with backup.open('xb') as stream:
            stream.write(original)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)
    print('Docker proxy configuration saved with a private recovery copy. Docker was not restarted. Schedule its restart explicitly; existing workloads may be interrupted.')


if __name__ == '__main__':
    main()
