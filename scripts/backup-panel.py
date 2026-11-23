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


def run(args, **kwargs):
    result = subprocess.run(args, stderr=subprocess.PIPE, **kwargs)
    if result.returncode:
        raise RuntimeError('Backup command failed: ' + ' '.join(args[:2]))
    return result


def restic(repository, password, *args, mounts=()):
    command = ['docker', 'run', '--rm', '--network', 'none', '--label', 'cove.backup=panel',
               '-v', str(repository) + ':/repository', '-v', str(password) + ':/password:ro']
    for source, target, mode in mounts:
        command += ['-v', str(source) + ':' + target + ':' + mode]
    return run(command + [RESTIC, '-r', '/repository', '--password-file', '/password', *args], stdout=subprocess.PIPE).stdout


def backup(installation, repository, password):
    compose = ['docker', 'compose', '--project-directory', str(installation)]
    containers = {}
    for service in ('cove', 'database', 'proxy'):
        ids = run(compose + ['ps', '-q', service], stdout=subprocess.PIPE).stdout.decode().split()
        if len(ids) != 1:
            raise RuntimeError('Exactly one running ' + service + ' container is required')
        containers[service] = ids[0]
    with tempfile.TemporaryDirectory(prefix='cove-panel-backup-') as temporary:
        folder = Path(temporary)
        for filename in ('.env', 'compose.yaml', 'deploy/traefik.yaml', 'deploy/traefik-panel.yaml'):
            source = installation / filename
            if source.is_symlink() or not source.is_file():
                raise RuntimeError('Expected regular installation file: ' + filename)
            target = folder / filename
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, target)
            target.chmod(0o600)
        info = json.loads(run(['docker', 'inspect', containers['cove']], stdout=subprocess.PIPE).stdout)[0]
        environment = dict(value.split('=', 1) for value in info['Config']['Env'] if '=' in value)
        if len(base64.b64decode(environment.get('COVE_ENCRYPTION_KEY', ''), validate=True)) != 32:
            raise RuntimeError('Running panel encryption key unavailable')
        (folder / 'runtime-environment.json').write_text(json.dumps(environment))
        with (folder / 'database.dump').open('wb') as stream:
            run(['docker', 'exec', containers['database'], 'pg_dump', '-U', 'cove', '-d', 'cove', '-Fc'], stdout=stream)
        run(['docker', 'cp', containers['proxy'] + ':/data/traefik-acme.json', str(folder / 'acme.json')], stdout=subprocess.PIPE)
        (folder / 'manifest.json').write_text(json.dumps({'format': 1, 'created_at': time.time(), 'panel_image': info['Image'],
                                                       'database': 'cove', 'kind': 'control-plane'}))
        output = restic(repository, password, 'backup', '/snapshot', '--tag', 'cove-panel', '--json', mounts=[(folder, '/snapshot', 'ro')])
        summaries = [json.loads(line) for line in output.splitlines() if line]
        snapshot = next((item['snapshot_id'] for item in summaries if item.get('message_type') == 'summary'), None)
        if not snapshot:
            raise RuntimeError('Backup did not return a snapshot identity')
        restic(repository, password, 'check', '--read-data')
        run(['docker', 'exec', containers['database'], 'psql', '-U', 'cove', '-d', 'cove', '-v', 'ON_ERROR_STOP=1', '-c',
             "BEGIN; UPDATE control_settings SET panel_backup_at=now() WHERE id=1; INSERT INTO audit_events(actor,action,subject,status) VALUES('host backup','panel backup verified','control-plane',200); COMMIT;"], stdout=subprocess.PIPE)
        return snapshot


def verify(repository, password, snapshot):
    # Restore only into a disposable database, never a running installation.
    import secrets
    name = 'cove-panel-verify-' + secrets.token_hex(8)
    with tempfile.TemporaryDirectory(prefix='cove-panel-verify-') as temporary:
        folder = Path(temporary)
        restic(repository, password, 'restore', snapshot, '--target', '/restore', mounts=[(folder, '/restore', 'rw')])
        root = folder / 'snapshot'
        manifest = json.loads((root / 'manifest.json').read_text())
        environment = json.loads((root / 'runtime-environment.json').read_text())
        if manifest.get('format') != 1 or manifest.get('kind') != 'control-plane' or len(base64.b64decode(environment.get('COVE_ENCRYPTION_KEY', ''), validate=True)) != 32:
            raise RuntimeError('Not a valid Cove control-plane snapshot')
        created = False
        try:
            run(['docker', 'run', '-d', '--network', 'none', '--name', name, '--label', 'cove.test=panel-restore',
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
        print('Verified encrypted snapshot: ' + backup(args.installation.resolve(strict=True), repository, password))
    else:
        if not args.snapshot or not all(c in '0123456789abcdef' for c in args.snapshot) or len(args.snapshot) not in (8, 64):
            parser.error('Supply an exact 8- or 64-character snapshot ID')
        verify(repository, password, args.snapshot)
        print('Snapshot restored successfully into a disposable database; the live panel was untouched')


if __name__ == '__main__':
    main()
