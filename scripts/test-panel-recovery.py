"""Exercise panel snapshots and recovery inside a disposable isolated Docker daemon."""
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import tarfile
import time

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, check=True, cwd=ROOT, **kwargs)


def main():
    name = 'cove-panel-recovery-test-' + secrets.token_hex(6)
    created = False
    with tempfile.TemporaryDirectory(prefix=name) as temporary:
        binary = Path(temporary) / 'cove'
        run('go', 'build', '-o', str(binary), './cmd/cove',
            env=os.environ | {'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'})
        historical = Path(temporary) / 'old-source'
        historical.mkdir()
        with tarfile.open(ROOT / 'scripts/testdata/panel-recovery/previous-panel.tar.gz', 'r:gz') as contents:
            contents.extractall(historical, filter='data')
        old_binary = Path(temporary) / 'cove-old'
        subprocess.run(['go', 'build', '-o', str(old_binary), './cmd/cove'], cwd=historical, check=True,
                       env=os.environ | {'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'})
        try:
            run('docker', 'run', '-d', '--privileged', '--name', name, '--label', 'cove.test=panel-recovery',
                '-e', 'DOCKER_TLS_CERTDIR=', 'docker:29-dind', '--host=unix:///var/run/docker.sock', stdout=subprocess.DEVNULL)
            created = True
            for _ in range(60):
                if subprocess.run(['docker', 'exec', name, 'docker', 'info'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError('Disposable Docker daemon did not start')
            run('docker', 'exec', name, 'apk', 'add', '--no-cache', 'python3', 'openssl', stdout=subprocess.DEVNULL)
            run('docker', 'exec', name, 'mkdir', '-p', '/fixture')
            files = [(binary, '/fixture/cove'), (old_binary, '/fixture/cove-old'),
                     (ROOT / 'scripts/backup-panel.py', '/fixture/backup-panel.py'),
                     (ROOT / 'scripts/panel-recovery.py', '/fixture/panel-recovery.py'),
                     (ROOT / 'scripts/install-panel-recovery.sh', '/fixture/install-panel-recovery.sh'),
                     (ROOT / 'test/systemd/panel_recovery_acceptance.py', '/fixture/acceptance.py')]
            for source, target in files:
                payload = source.read_bytes()
                if source.suffix == '.sh':
                    payload = payload.replace(b'\r\n', b'\n')
                run('docker', 'exec', '-i', name, 'sh', '-c', 'cat > ' + target, input=payload)
            run('docker', 'exec', name, 'chmod', '755', '/fixture/cove')
            run('docker', 'exec', name, 'chmod', '755', '/fixture/cove-old')
            run('docker', 'exec', name, 'python3', '-u', '/fixture/acceptance.py')
        finally:
            if created:
                run('docker', 'rm', '-f', '-v', name, stdout=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
