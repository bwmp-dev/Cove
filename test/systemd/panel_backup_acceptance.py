"""Disposable control-plane backup and restore acceptance inside the migration DinD fixture."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import time


def main():
    if not Path('/cove-isolated-migration-fixture').exists():
        raise SystemExit('Dedicated fixture required')
    os.umask(0o077)
    spec = importlib.util.spec_from_file_location('panel_backup', '/fixture/backup-panel.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    root = Path('/fixture/panel')
    (root / 'deploy').mkdir(parents=True)
    for name in ('.env', 'deploy/traefik.yaml', 'deploy/traefik-panel.yaml'):
        (root / name).write_text('fixture-only')
    (root / 'compose.yaml').write_text('''services:
  cove:
    image: busybox:1.37
    command: [sleep, '300']
    environment:
      COVE_ENCRYPTION_KEY: MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=
  database:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: cove
      POSTGRES_DB: cove
      POSTGRES_PASSWORD: fixture-only
  proxy:
    image: busybox:1.37
    command: [sh, -ec, 'mkdir -p /data; echo {} > /data/traefik-acme.json; sleep 300']
''')
    compose = ['docker', 'compose', '--project-directory', str(root)]
    repository = Path('/fixture/panel-backups')
    repository.mkdir()
    password = Path('/fixture/panel-backup-password')
    password.write_text('fixture-only-separate-backup-password')
    try:
        subprocess.run(compose + ['up', '-d'], check=True)
        for _ in range(60):
            result = subprocess.run(compose + ['exec', '-T', 'database', 'pg_isready', '-U', 'cove'], stdout=subprocess.DEVNULL)
            if result.returncode == 0:
                break
            time.sleep(1)
        sql = 'CREATE TABLE schema_migrations(id text); CREATE TABLE workloads(id text); CREATE TABLE nodes(id text); INSERT INTO workloads VALUES (\'preserved\');'
        subprocess.run(compose + ['exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-c', sql], check=True, stdout=subprocess.DEVNULL)
        module.restic(repository, password, 'init')
        snapshot = module.backup(root, repository, password)
        module.verify(repository, password, snapshot)
        result = subprocess.check_output(compose + ['exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-Atc', 'SELECT id FROM workloads']).decode().strip()
        assert result == 'preserved'
        wrong = Path('/fixture/wrong-password')
        wrong.write_text('incorrect-fixture-password')
        try:
            module.restic(repository, wrong, 'snapshots')
        except RuntimeError:
            pass
        else:
            raise AssertionError('Wrong backup password accepted')
        print('PASS: encrypted panel snapshot, verified repository, isolated PostgreSQL restore, wrong-password rejection, live database unchanged', flush=True)
    finally:
        subprocess.run(compose + ['down', '-v'], check=True)


if __name__ == '__main__':
    main()
