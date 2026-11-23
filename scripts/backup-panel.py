"""Encrypted control-plane backups for a local Cove Compose installation."""
import argparse
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

RESTIC = 'restic/restic:0.19.1'


def copy_bounded(source, target, remaining):
    with target.open('xb') as output:
        while True:
            chunk = source.read(min(1024 * 1024, remaining + 1))
            if not chunk:
                return remaining
            remaining -= len(chunk)
            if remaining < 0:
                raise RuntimeError('Panel snapshot exceeds its configured size limit')
            output.write(chunk)


def capture_bounded(command, target, remaining):
    with subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL) as process:
        try:
            remaining = copy_bounded(process.stdout, target, remaining)
            if process.wait(timeout=30) != 0:
                raise RuntimeError('Panel snapshot capture failed')
            return remaining
        except BaseException:
            process.kill()
            process.wait()
            raise


def run(args, **kwargs):
    result = subprocess.run(args, stderr=subprocess.PIPE, **kwargs)
    if result.returncode:
        raise RuntimeError('Backup command failed: ' + ' '.join(args[:2]))
    return result


def restic(repository, password, *args, mounts=(), owner=None):
    command = ['docker', 'run', '--rm', '--network', 'none', '--log-driver', 'none', '--memory', '1g', '--pids-limit', '128', '--cpus', '2', '--label', 'cove.backup=panel',
               '-v', str(repository) + ':/repository', '-v', str(password) + ':/password:ro']
    for source, target, mode in mounts:
        command += ['-v', str(source) + ':' + target + ':' + mode]
    if owner:
        command += ['--label', 'dev.cove.panel-agent=' + owner, '--label', 'dev.cove.panel-helper=true']
    return run(command + [RESTIC, '-r', '/repository', '--password-file', '/password', *args], stdout=subprocess.PIPE).stdout


def backup(installation, repository, password, max_bytes=8 << 30, stage=None, extra=None, tag=None, owner=None):
    compose = ['docker', 'compose', '--project-directory', str(installation)]
    containers = {}
    for service in ('cove', 'database', 'proxy'):
        ids = run(compose + ['ps', '-q', service], stdout=subprocess.PIPE).stdout.decode().split()
        if len(ids) != 1:
            raise RuntimeError('Exactly one running ' + service + ' container is required')
        containers[service] = ids[0]
    if max_bytes < 64 << 20 or max_bytes > 64 << 30:
        raise RuntimeError('Panel snapshot limit must be 64 MiB–64 GiB')
    if shutil.disk_usage(stage or installation).free < 2 * max_bytes + (256 << 20):
        raise RuntimeError('Insufficient free space for the configured snapshot and encrypted copy')
    with tempfile.TemporaryDirectory(prefix='cove-panel-backup-', dir=stage) as temporary:
        folder = Path(temporary)
        remaining = max_bytes - (64 << 10)
        info = json.loads(run(['docker', 'inspect', containers['cove']], stdout=subprocess.PIPE).stdout)[0]
        filenames = {'.env', 'compose.yaml', 'deploy/traefik.yaml', 'deploy/traefik-panel.yaml'}
        configs = (info['Config'].get('Labels') or {}).get('com.docker.compose.project.config_files', '').split(',')
        compose_files = []
        if len(configs)>16:
            raise RuntimeError('Panel installation uses too many Compose source files')
        for value in configs:
            if not value:
                continue
            source = Path(value)
            if source == installation / '.cove-panel-recovery/restore-compose.json':
                continue
            try:
                relative = source.relative_to(installation)
            except ValueError:
                raise RuntimeError('Panel Compose source files must be inside the installation directory') from None
            if any(part in ('.', '..', '.cove-panel-recovery') for part in relative.parts) or str(relative) in ('manifest.json', 'database.dump', 'runtime-environment.json', 'acme.json', 'locked-compose.json'):
                raise RuntimeError('Panel Compose source path is not safe to archive')
            filenames.add(str(relative))
            compose_files.append(str(relative))
        for filename in sorted(filenames):
            source = installation / filename
            if source.is_symlink() or not source.is_file() or source.resolve().relative_to(installation) != Path(filename):
                raise RuntimeError('Expected regular installation file: ' + filename)
            target = folder / filename
            target.parent.mkdir(parents=True, exist_ok=True)
            with source.open('rb') as input_stream:
                remaining = copy_bounded(input_stream, target, remaining)
            target.chmod(0o600)
        environment = dict(value.split('=', 1) for value in info['Config']['Env'] if '=' in value)
        if len(base64.b64decode(environment.get('COVE_ENCRYPTION_KEY', ''), validate=True)) != 32:
            raise RuntimeError('Running panel encryption key unavailable')
        payload = json.dumps(environment).encode()
        if len(payload) > min(remaining, 1 << 20):
            raise RuntimeError('Panel environment exceeds snapshot limit')
        (folder / 'runtime-environment.json').write_bytes(payload)
        remaining -= len(payload)
        remaining = capture_bounded(['docker', 'exec', containers['database'], 'timeout', '1800', 'pg_dump', '-U', 'cove', '-d', 'cove', '-Fc'], folder / 'database.dump', remaining)
        acme_limit = min(remaining,64 << 20)
        acme_remaining = capture_bounded(['docker', 'exec', containers['proxy'], 'sh', '-ec', 'test -f /data/traefik-acme.json && test ! -L /data/traefik-acme.json && timeout 30 cat /data/traefik-acme.json'], folder / 'acme.json', acme_limit)
        remaining -= acme_limit - acme_remaining
        if not isinstance(json.loads((folder/'acme.json').read_bytes()),dict):
            raise RuntimeError('Panel ACME storage must be valid JSON, at most 64 MiB')
        manifest = {'format': 1, 'created_at': time.time(), 'panel_image': info['Image'], 'database': 'cove', 'kind': 'control-plane', 'installation_files': sorted(filenames), 'compose_files': compose_files or ['compose.yaml']}
        if extra:
            for name, value in extra.get('files', {}).items():
                if name != 'locked-compose.json':
                    raise RuntimeError('Unexpected panel snapshot file')
                payload = value.encode()
                if len(payload) > min(remaining, 2 << 20):
                    raise RuntimeError('Panel Compose configuration exceeds snapshot limit')
                (folder / name).write_bytes(payload)
                remaining -= len(payload)
            manifest.update(extra.get('manifest', {}))
        manifest['bytes'] = max_bytes - remaining
        (folder / 'manifest.json').write_text(json.dumps(manifest))
        tags = ['--tag', tag] if tag else []
        output = restic(repository, password, 'backup', '/snapshot', '--tag', 'cove-panel', *tags, '--json', mounts=[(folder, '/snapshot', 'ro')], owner=owner)
        summaries = [json.loads(line) for line in output.splitlines() if line]
        snapshot = next((item['snapshot_id'] for item in summaries if item.get('message_type') == 'summary'), None)
        if not snapshot:
            raise RuntimeError('Backup did not return a snapshot identity')
        restic(repository, password, 'check', '--read-data', owner=owner)
        run(['docker', 'exec', containers['database'], 'psql', '-U', 'cove', '-d', 'cove', '-v', 'ON_ERROR_STOP=1', '-c',
             "BEGIN; UPDATE control_settings SET panel_backup_at=now() WHERE id=1; INSERT INTO audit_events(actor,action,subject,status) VALUES('host backup','panel backup verified','control-plane',200); COMMIT;"], stdout=subprocess.PIPE)
        return snapshot


def verify(repository, password, snapshot, stage=None, owner=None):
    # Restore only into a disposable database, never a running installation.
    import secrets
    name = 'cove-panel-verify-' + secrets.token_hex(8)
    with tempfile.TemporaryDirectory(prefix='cove-panel-verify-', dir=stage) as temporary:
        folder = Path(temporary)
        restic(repository, password, 'restore', snapshot, '--target', '/restore', mounts=[(folder, '/restore', 'rw')], owner=owner)
        root = folder / 'snapshot'
        manifest = json.loads((root / 'manifest.json').read_text())
        environment = json.loads((root / 'runtime-environment.json').read_text())
        if manifest.get('format') != 1 or manifest.get('kind') != 'control-plane' or len(base64.b64decode(environment.get('COVE_ENCRYPTION_KEY', ''), validate=True)) != 32:
            raise RuntimeError('Not a valid Cove control-plane snapshot')
        created = False
        try:
            labels = ['--label', 'dev.cove.panel-agent=' + owner, '--label', 'dev.cove.panel-helper=true'] if owner else []
            run(['docker', 'run', '-d', '--network', 'none', '--name', name, '--label', 'cove.test=panel-restore',
                 '--log-driver', 'none', '--memory', '2g', '--pids-limit', '512', '--cpus', '2', *labels,
                 '--tmpfs', '/var/lib/postgresql/data:rw,size=2g',
                 '-e', 'POSTGRES_HOST_AUTH_METHOD=trust', 'postgres:17-alpine'], stdout=subprocess.PIPE)
            created = True
            for _ in range(60):
                ready = subprocess.run(['docker', 'exec', name, 'pg_isready', '-U', 'postgres'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                if ready.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError('Disposable restore database did not start')
            run(['docker', 'exec', name, 'createdb', '-U', 'postgres', 'cove_verify'], stdout=subprocess.PIPE)
            with (root / 'database.dump').open('rb') as stream:
                run(['docker', 'exec', '-i', name, 'pg_restore', '-U', 'postgres', '-d', 'cove_verify', '--no-owner', '--no-acl', '--exit-on-error'], stdin=stream, stdout=subprocess.PIPE)
            run(['docker', 'exec', name, 'psql', '-U', 'postgres', '-d', 'cove_verify', '-v', 'ON_ERROR_STOP=1', '-c',
                 'SELECT count(*) FROM schema_migrations; SELECT count(*) FROM workloads; SELECT count(*) FROM nodes;'], stdout=subprocess.PIPE)
        finally:
            if created:
                run(['docker', 'rm', '-f', '-v', name], stdout=subprocess.PIPE)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['init', 'backup', 'verify'])
    parser.add_argument('--installation', type=Path, default=Path('/opt/cove'))
    parser.add_argument('--repository', type=Path, required=True)
    parser.add_argument('--password-file', type=Path, required=True)
    parser.add_argument('--snapshot', help='Exact snapshot ID to restore-test')
    parser.add_argument('--max-bytes', type=int, help='Snapshot byte limit, 64 MiB–64 GiB; default fits available staging space, up to 8 GiB')
    args = parser.parse_args()
    if os.name != 'posix' or os.geteuid() != 0:
        parser.error('Run on the Linux Docker host as root')
    os.umask(0o077)
    password = args.password_file.resolve(strict=True)
    if not password.is_file() or password.stat().st_mode & 0o077 or password.stat().st_size < 16:
        parser.error('Use a private password file containing at least 16 bytes; store it separately from the repository')
    repository = args.repository.resolve()
    if repository == password or repository in password.parents:
        parser.error('Password file must be outside the backup repository')
    repository.mkdir(mode=0o700, parents=True, exist_ok=True)
    if args.command == 'init':
        restic(repository, password, 'init')
        print('Encrypted panel backup repository initialized')
    elif args.command == 'backup':
        limit = args.max_bytes if args.max_bytes is not None else min(8 << 30, (shutil.disk_usage(repository).free - (272 << 20)) // 2)
        print('Verified encrypted snapshot: ' + backup(args.installation.resolve(strict=True), repository, password, limit))
    else:
        if not args.snapshot or not all(c in '0123456789abcdef' for c in args.snapshot) or len(args.snapshot) not in (8, 64):
            parser.error('Supply an exact 8- or 64-character snapshot ID')
        verify(repository, password, args.snapshot)
        print('Snapshot restored successfully into a disposable database; the live panel was untouched')


if __name__ == '__main__':
    main()
