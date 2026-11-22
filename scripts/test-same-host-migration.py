"""Run migration acceptance in its own Docker daemon; never uses production Docker resources."""
import argparse
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', help='Optional SSH test host')
    parser.add_argument('--known-hosts')
    args = parser.parse_args()
    if args.host and not args.known_hosts:
        parser.error('--known-hosts is required with --host')
    options = ['-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'UserKnownHostsFile=' + (args.known_hosts or '')]
    name = 'cove-same-host-' + secrets.token_hex(5)
    def docker(*command, **kwargs):
        if args.host:
            import shlex
            invocation = ['ssh', *options, args.host, shlex.join(['sudo', '-n', 'docker', *command])]
        else:
            invocation = ['docker', *command]
        return subprocess.run(invocation, check=True, **kwargs)
    with tempfile.TemporaryDirectory(prefix=name) as folder:
        binary = Path(folder) / 'cove-worker'
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/cove-worker'], cwd=ROOT,
                       env=os.environ | {'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, check=True)
        created = False
        try:
            docker('run', '-d', '--privileged', '--name', name, '--label', 'cove.test=same-host', '-e', 'DOCKER_TLS_CERTDIR=',
                   'docker:29-dind', '--host=unix:///var/run/docker.sock', '--storage-driver=overlay2')
            created = True
            for _ in range(60):
                try:
                    docker('exec', name, 'docker', 'info', stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    break
                except subprocess.CalledProcessError:
                    time.sleep(1)
            else:
                raise RuntimeError('Isolated Docker daemon did not start')
            docker('exec', name, 'apk', 'add', '--no-cache', 'python3', 'openssl', stdout=subprocess.DEVNULL)
            docker('exec', name, 'mkdir', '-p', '/fixture')
            docker('exec', name, 'touch', '/cove-isolated-migration-fixture')
            for source, target in [(binary, '/usr/local/bin/cove-worker'), (ROOT / 'scripts/migrate-same-host.py', '/fixture/migrate-same-host.py'),
                                   (ROOT / 'scripts/backup-panel.py', '/fixture/backup-panel.py'), (ROOT / 'test/systemd/panel_backup_acceptance.py', '/fixture/panel_backup_acceptance.py'), (ROOT / 'test/systemd/same_host_acceptance.py', '/fixture/acceptance.py')]:
                # Stream files through exec, avoiding host mounts and remote staging directories.
                docker('exec', '-i', name, 'sh', '-c', 'cat > ' + target, input=source.read_bytes())
            docker('exec', name, 'chmod', '700', '/usr/local/bin/cove-worker')
            docker('exec', name, 'python3', '-u', '/fixture/acceptance.py')
            docker('exec', name, 'python3', '-u', '/fixture/panel_backup_acceptance.py')
        except Exception:
            if created:
                docker('exec', name, 'sh', '-c', 'tail -100 /fixture/worker.log 2>/dev/null || true')
                docker('exec', name, 'sh', '-c', 'docker logs cove-ingress 2>&1 | tail -50')
            raise
        finally:
            if created:
                docker('rm', '-f', '-v', name, stdout=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
