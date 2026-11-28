#!/usr/bin/env python3
"""Host-side encrypted panel snapshots and journaled recovery."""
import argparse
import base64
import datetime
import hashlib
import hmac
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

spec = importlib.util.spec_from_file_location('panel_backup', Path(__file__).with_name('backup-panel.py'))
backup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(backup)


def private_json(path, value):
    data = json.dumps(value, separators=(',', ':')).encode()
    if len(data) > 1 << 20:
        raise RuntimeError('Panel recovery metadata exceeds its limit')
    temporary = path.with_suffix('.next')
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    with os.fdopen(descriptor, 'wb') as output:
        info = os.fstat(output.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
            raise RuntimeError('Panel recovery metadata must be private root-owned regular files')
        output.write(data)
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, path)
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def read_json(path, default=None):
    if not path.exists():
        return default
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, 'rb') as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
            raise RuntimeError('Panel recovery metadata must be private root-owned regular files')
        data = source.read((1 << 20) + 1)
    if len(data) > 1 << 20:
        raise RuntimeError('Panel recovery metadata exceeds its limit')
    return json.loads(data)


def command(args, *, input=None, environment=None, timeout=1800):
    result = subprocess.run(args, input=input, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            env=environment, timeout=timeout)
    if result.returncode:
        raise RuntimeError('Panel host command failed')
    if len(result.stdout) > 4 << 20:
        raise RuntimeError('Panel host command exceeded its output limit')
    return result.stdout


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, target):
        raise RuntimeError('Panel host requests must not redirect credentials')


def root_installation(path):
    installation = path.resolve(strict=True)
    for directory in (installation, *installation.parents):
        info = directory.stat()
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise RuntimeError('Use a root-owned installation below root-owned directories without group/world write access')
    return installation


class Recovery:
    def __init__(self, installation):
        self.installation = root_installation(installation)
        self.root = self.installation / '.cove-panel-recovery'
        if self.root.is_symlink():
            raise RuntimeError('Panel recovery directory must not be a symlink')
        self.root.mkdir(mode=0o700, exist_ok=True)
        if self.root.stat().st_uid != 0 or self.root.stat().st_mode & 0o077:
            raise RuntimeError('Panel recovery directory must be private')
        self.stage = self.root / 'stage'
        self.repository = self.root / 'repository'
        self.password = self.root / 'password'
        if not self.password.exists():
            descriptor = os.open(self.password, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(descriptor, 'wb') as output:
                output.write(base64.b64encode(secrets.token_bytes(32)))
                output.flush()
                os.fsync(output.fileno())
        if self.password.is_symlink() or self.password.stat().st_uid != 0 or not self.password.is_file() or self.password.stat().st_mode & 0o077 or self.password.stat().st_size != 44:
            raise RuntimeError('Panel repository password must be a private 32-byte base64 key')
        identity = read_json(self.root / 'identity.json')
        if identity is None:
            identity = {'agent_id': secrets.token_hex(16)}
            private_json(self.root / 'identity.json', identity)
        self.agent_id = identity['agent_id']
        if not re.fullmatch('[a-f0-9]{32}', self.agent_id):
            raise RuntimeError('Invalid panel host identity')
        self.compose = ['docker', 'compose', '--project-directory', str(self.installation)]
        self.image = None
        self.environment = None

    def inspect(self):
        ids = command([*self.compose, 'ps', '-q', 'cove']).decode().split()
        if len(ids) != 1:
            raise RuntimeError('Exactly one running panel is required')
        info = json.loads(command(['docker', 'inspect', ids[0]]))[0]
        self.environment = dict(item.split('=', 1) for item in info['Config']['Env'] if '=' in item)
        self.image = info['Image']
        origin = self.environment.get('COVE_PUBLIC_URL', '')
        parsed = urllib.parse.urlsplit(origin)
        if parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or len(parsed.path) > 200 or not re.fullmatch(r'(/[A-Za-z0-9_-]+)*', parsed.path) or parsed.path == '/healthz' or any(char in origin for char in "'\"\\?# \t\r\n\x00"):
            raise RuntimeError('The panel host requires an HTTPS panel URL with an optional simple base path')
        pinned = read_json(self.root / 'origin.json')
        if pinned is None:
            private_json(self.root / 'origin.json', {'origin': origin})
        elif pinned.get('origin') != origin:
            raise RuntimeError('Panel origin changed; review host recovery configuration before reconnecting')
        return origin

    def api(self, action, payload):
        if self.environment is None:
            self.inspect()
        origin = read_json(self.root / 'origin.json')['origin']
        if origin != self.environment.get('COVE_PUBLIC_URL'):
            raise RuntimeError('Refusing to send credentials to another panel origin')
        body = json.dumps(payload, separators=(',', ':')).encode()
        path = '/api/internal/panel-host/' + action
        stamp = str(int(time.time()))
        key = base64.b64decode(self.environment['COVE_ENCRYPTION_KEY'], validate=True)
        derived = hmac.digest(key, b'cove.panel.host.v1', 'sha256')
        signature = hmac.new(derived, (stamp + '\nPOST\n' + path + '\n').encode() + body, hashlib.sha256).hexdigest()
        request = urllib.request.Request(origin + path, data=body, headers={
            'Authorization': 'Bearer ' + self.environment['COVE_ADMIN_TOKEN'], 'Content-Type': 'application/json',
            'X-Cove-Host-Time': stamp, 'X-Cove-Host-Signature': signature})
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=30) as response:
            data = response.read((1 << 20) + 1)
        if len(data) > 1 << 20:
            raise RuntimeError('Panel host response exceeds its limit')
        return json.loads(data)

    def crypto(self, value, action, *, local=False, image=None):
        env = os.environ.copy()
        name = 'COVE_PANEL_PLAN_KEY' if local else 'COVE_ENCRYPTION_KEY'
        env[name] = self.password.read_text() if local else self.environment[name]
        output = command(['docker', 'run', '--rm', '-i', '--network', 'none', '--read-only', '--user', '10001',
                          '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--log-driver', 'none',
                          '--memory', '128m', '--pids-limit', '64', '--cpus', '1',
                          '--label', 'dev.cove.panel-agent=' + self.agent_id, '--label', 'dev.cove.panel-helper=true',
                          '-e', name, image or self.image, 'panel-backup-plan', action],
                         input=json.dumps(value).encode(), environment=env, timeout=30)
        return json.loads(output)

    def clear_stage(self):
        if self.stage.is_symlink():
            raise RuntimeError('Recovery stage must not be a symlink')
        if self.stage.exists():
            shutil.rmtree(self.stage)
        self.stage.mkdir(mode=0o700)

    def inventory(self):
        items = read_json(self.root / 'snapshots.json', [])
        if len(items) > 103:
            raise RuntimeError('Panel snapshot inventory exceeds its cap')
        return items

    def init_repository(self):
        if self.repository.is_symlink():
            raise RuntimeError('Panel repository must not be a symlink')
        self.repository.mkdir(mode=0o700, exist_ok=True)
        if not (self.repository / 'config').exists():
            self.restic('init')

    def restic(self, *args, mounts=()):
        return backup.restic(self.repository, self.password, *args, mounts=mounts, owner=self.agent_id)

    def locked_configuration(self):
        config = json.loads(command([*self.compose, 'config', '--format', 'json']))
        for name, service in config['services'].items():
            ids = command([*self.compose, 'ps', '-q', name]).decode().split()
            if len(ids) != 1:
                raise RuntimeError('Every installation service must be running before a panel snapshot')
            info = json.loads(command(['docker', 'inspect', ids[0]]))[0]
            service['image'] = info['Image']
            service.pop('build', None)
            service.pop('pull_policy', None)
        if not {'cove', 'database', 'proxy'} <= config['services'].keys():
            raise RuntimeError('Expected the standard Cove installation services')
        if len(config['services']) > 8:
            raise RuntimeError('Panel recovery supports at most eight installation services')
        return config

    def hold_images(self, snapshot, config):
        for index, service in enumerate(config['services'].values()):
            name = 'cove-panel-image-' + self.agent_id + '-' + snapshot[:16] + '-' + str(index)
            exists = subprocess.run(['docker', 'container', 'inspect', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if exists.returncode == 0:
                info = json.loads(command(['docker', 'inspect', name]))[0]
                if info['Config']['Labels'].get('dev.cove.panel-agent') != self.agent_id or info['Image'] != service['image']:
                    raise RuntimeError('Panel recovery image holder ownership mismatch')
                continue
            command(['docker', 'create', '--name', name, '--network', 'none', '--log-driver', 'none',
                     '--label', 'dev.cove.panel-agent=' + self.agent_id, '--label', 'dev.cove.panel-snapshot=' + snapshot,
                     '--entrypoint', '/cove-image-retention-only', service['image']])

    def create_snapshot(self, plan):
        self.init_repository()
        self.restic('prune')
        tag = 'cove-panel-job:' + plan['job_id']
        matches = json.loads(self.restic('snapshots', '--tag', tag, '--json'))
        if len(matches) > 1:
            raise RuntimeError('Multiple snapshots reference one panel recovery job')
        config = self.locked_configuration() if not matches else None
        if config is not None:
            saved_environment = config['services']['cove'].get('environment',{})
            if any(saved_environment.get(name)!=self.environment.get(name) for name in ('COVE_ENCRYPTION_KEY','COVE_ADMIN_TOKEN','COVE_PUBLIC_URL','DATABASE_URL')):
                raise RuntimeError('Apply or revert pending runtime credential/configuration edits before taking a panel snapshot')
        version = self.environment.get('COVE_VERSION', '')
        labels = json.loads(command(['docker', 'image', 'inspect', self.image]))[0]['Config'].get('Labels') or {}
        extra = {'files': {'locked-compose.json': json.dumps(config).replace('$', '$$')},
                 'manifest': {'agent_id': self.agent_id, 'origin': self.environment['COVE_PUBLIC_URL'],
                              'version': labels.get('org.opencontainers.image.version', version),
                              'revision': labels.get('org.opencontainers.image.revision', ''),
                              'job_id': plan['job_id']}}
        if not matches:
            snapshot = backup.backup(self.installation, self.repository, self.password, plan['config']['max_bytes'], self.stage, extra, tag, self.agent_id)
            matches = json.loads(self.restic('snapshots', '--tag', tag, '--json'))
        else:
            snapshot = matches[0]['id']
        if len(matches) != 1 or not matches[0]['id'].startswith(snapshot):
            raise RuntimeError('Panel snapshot identity could not be reconciled')
        snapshot = matches[0]['id']
        if config is None:
            config = json.loads(self.restic('dump', snapshot, '/snapshot/locked-compose.json'))
        self.hold_images(snapshot, config)
        backup.verify(self.repository, self.password, snapshot, self.stage, self.agent_id)
        manifest = json.loads(self.restic('dump', snapshot, '/snapshot/manifest.json'))
        item = {'id': snapshot, 'created_at': datetime.datetime.fromtimestamp(manifest['created_at'], datetime.timezone.utc).isoformat(),
                'version': manifest['version'], 'revision': manifest['revision'], 'bytes': manifest['bytes'],
                'verified': True, 'offsite': False, 'origin': manifest['origin']}
        items = self.inventory()
        items = [old for old in items if old['id'] != snapshot]
        items.append(item)
        private_json(self.root / 'snapshots.json', items)
        return snapshot

    def cleanup_helpers(self):
        ids = command(['docker', 'ps', '-aq', '--filter', 'label=dev.cove.panel-agent=' + self.agent_id,
                       '--filter', 'label=dev.cove.panel-helper=true']).decode().split()
        if len(ids) > 32:
            raise RuntimeError('Panel recovery helper count exceeds its cap')
        for identity in ids:
            info = json.loads(command(['docker', 'inspect', identity]))[0]
            labels = info['Config'].get('Labels') or {}
            if labels.get('dev.cove.panel-agent') != self.agent_id or labels.get('dev.cove.panel-helper') != 'true':
                raise RuntimeError('Panel helper ownership mismatch')
            command(['docker', 'rm', '-f', '-v', identity])
        if (self.repository / 'config').exists():
            self.restic('unlock')

    def save_journal(self, journal):
        private_json(self.root / 'journal.json', journal)

    def read_plan(self, journal):
        if 'bootstrap_plan' not in journal:
            return self.crypto({'sealed': journal['sealed']}, 'open', local=True, image=journal['image'])
        plan = journal['bootstrap_plan']
        encoded = json.dumps(plan, sort_keys=True, separators=(',', ':')).encode()
        key = base64.b64decode(self.password.read_bytes(), validate=True)
        signature = hmac.new(key, b'cove.panel.bootstrap.v1\n' + encoded, hashlib.sha256).hexdigest()
        config = plan.get('config', {})
        if not hmac.compare_digest(signature, journal.get('signature', '')) or set(plan) != {'kind', 'agent_id', 'job_id', 'actor_id', 'action', 'config'}:
            raise RuntimeError('Bootstrap snapshot journal could not be authenticated')
        if plan['kind'] != 'cove.panel-backup.v1' or plan['agent_id'] != self.agent_id or not re.fullmatch('[a-f0-9]{32}', plan['job_id']) or plan['actor_id'] != 'host-bootstrap' or plan['action'] != 'backup':
            raise RuntimeError('Invalid bootstrap snapshot journal identity')
        if set(config) != {'enabled', 'interval_hours', 'keep_last', 'keep_days', 'max_bytes', 'storage_id'} or config['enabled'] is not False or config['interval_hours'] != 24 or config['keep_last'] != 7 or config['keep_days'] != 30 or config['storage_id'] != '' or type(config['max_bytes']) is not int or not 64 << 20 <= config['max_bytes'] <= 64 << 30:
            raise RuntimeError('Bootstrap journals may contain only the local non-secret default policy')
        return {'plan': plan}

    def bootstrap_journal(self):
        budget = min(8 << 30, (shutil.disk_usage(self.root).free - (272 << 20)) // 2)
        if budget < 64 << 20:
            raise RuntimeError('Insufficient free space for a verified pre-update panel snapshot')
        plan = {'kind': 'cove.panel-backup.v1', 'agent_id': self.agent_id, 'job_id': secrets.token_hex(16),
                'actor_id': 'host-bootstrap', 'action': 'backup', 'config': {'enabled': False, 'interval_hours': 24,
                'keep_last': 7, 'keep_days': 30, 'max_bytes': budget, 'storage_id': ''}}
        encoded = json.dumps(plan, sort_keys=True, separators=(',', ':')).encode()
        key = base64.b64decode(self.password.read_bytes(), validate=True)
        return {'image': self.image, 'phase': 'prepared', 'bootstrap_plan': plan,
                'signature': hmac.new(key, b'cove.panel.bootstrap.v1\n' + encoded, hashlib.sha256).hexdigest()}

    def restore(self, plan, journal):
        snapshot = plan['snapshot_id']
        if not re.fullmatch('[a-f0-9]{64}', snapshot):
            raise RuntimeError('An exact retained panel snapshot is required')
        item = next((item for item in self.inventory() if item['id'] == snapshot and item['verified']), None)
        if not item:
            raise RuntimeError('Panel snapshot is not retained and verified')
        manifest = json.loads(self.restic('dump', snapshot, '/snapshot/manifest.json'))
        runtime = json.loads(self.restic('dump', snapshot, '/snapshot/runtime-environment.json'))
        config_text = self.restic('dump', snapshot, '/snapshot/locked-compose.json').decode()
        config = json.loads(config_text)
        files = manifest.get('installation_files', ['.env', 'compose.yaml', 'deploy/traefik.yaml', 'deploy/traefik-panel.yaml'])
        if not isinstance(files,list) or len(files)>20 or any(not isinstance(name,str) for name in files) or not {'.env','compose.yaml','deploy/traefik.yaml','deploy/traefik-panel.yaml'}<=set(files):
            raise RuntimeError('Invalid installation file manifest')
        for name in files:
            if not name or name.startswith('/') or any(part in ('', '.', '..', '.cove-panel-recovery') for part in name.split('/')) or any(ord(c)<32 or c=='\\' for c in name):
                raise RuntimeError('Unsafe installation file manifest')
        compose_files = manifest.get('compose_files',['compose.yaml'])
        if not isinstance(compose_files,list) or not 1<=len(compose_files)<=16 or any(not isinstance(name,str) or name not in files for name in compose_files):
            raise RuntimeError('Invalid Compose source manifest')
        database_url = urllib.parse.urlsplit(runtime.get('DATABASE_URL',''))
        password = urllib.parse.unquote(database_url.password or '')
        if database_url.scheme not in ('postgres','postgresql') or database_url.path!='/cove' or urllib.parse.unquote(database_url.username or '')!='cove' or not password or len(password)>4096 or any(c in password for c in '\x00\r\n'):
            raise RuntimeError('Saved database authentication is invalid')
        origin = read_json(self.root / 'origin.json')['origin']
        if manifest.get('kind') != 'control-plane' or manifest.get('format') != 1 or manifest.get('agent_id') != self.agent_id or manifest.get('origin') != origin or runtime.get('COVE_PUBLIC_URL') != origin:
            raise RuntimeError('Snapshot identity or public origin does not match this installation')
        if len(base64.b64decode(runtime.get('COVE_ENCRYPTION_KEY', ''), validate=True)) != 32 or not {'cove', 'database', 'proxy'} <= config.get('services', {}).keys():
            raise RuntimeError('Snapshot runtime configuration is invalid')
        for service in config['services'].values():
            image = service.get('image', '')
            if not re.fullmatch('sha256:[a-f0-9]{64}', image):
                raise RuntimeError('Snapshot does not pin every installation image')
            command(['docker', 'image', 'inspect', image])
        current_version = command([*self.compose,'exec','-T','database','postgres','--version']).decode()
        target_version = command(['docker','run','--rm','--network','none','--read-only','--log-driver','none','--memory','128m',
                                  '--pids-limit','64','--cpus','1','--cap-drop','ALL','--security-opt','no-new-privileges',
                                  '--label','dev.cove.panel-agent='+self.agent_id,'--label','dev.cove.panel-helper=true',
                                  '--entrypoint','postgres',config['services']['database']['image'],'--version']).decode()
        current_major = re.search(r'PostgreSQL\)\s+(\d+)',current_version)
        target_major = re.search(r'PostgreSQL\)\s+(\d+)',target_version)
        if not current_major or not target_major or current_major[1]!=target_major[1]:
            raise RuntimeError('Cross-major PostgreSQL recovery requires a separate host; no panel data was overwritten')
        self.hold_images(snapshot, config)
        if journal['phase'] == 'prepared':
            backup.verify(self.repository, self.password, snapshot, self.stage, self.agent_id)
            safety = dict(plan, job_id=plan['job_id'] + '-safety', action='backup', snapshot_id='')
            journal['safety_id'] = self.create_snapshot(safety)
            journal['phase'] = 'safety_verified'
            self.save_journal(journal)
        folder = self.stage / 'restore'
        folder.mkdir(mode=0o700, exist_ok=True)
        self.restic('restore', snapshot, '--target', '/restore', mounts=[(folder, '/restore', 'rw')])
        source = folder / 'snapshot'
        total = 0
        for path in source.rglob('*'):
            if path.is_symlink():
                raise RuntimeError('Snapshot contains an unsafe symbolic link')
            if path.is_file():
                total += path.stat().st_size
        if total > plan['config']['max_bytes']:
            raise RuntimeError('Snapshot exceeds the configured restore limit')
        for name in [*files,'database.dump','runtime-environment.json','locked-compose.json','manifest.json','acme.json']:
            if not (source/name).is_file():
                raise RuntimeError('Required panel snapshot file is missing')
        if (source/'acme.json').stat().st_size>64<<20 or not isinstance(json.loads((source/'acme.json').read_bytes()),dict):
            raise RuntimeError('Saved ACME certificate storage is invalid')
        source_args = [part for name in compose_files for part in ('-f',str(source/name))]
        checked_config = json.loads(command(['docker','compose','--project-directory',str(self.installation),'--env-file',str(source/'.env'),*source_args,'-f',str(source/'locked-compose.json'),'config','--format','json']))
        saved_environment = checked_config['services']['cove'].get('environment',{})
        if any(saved_environment.get(name)!=runtime.get(name) for name in ('COVE_ENCRYPTION_KEY','COVE_ADMIN_TOKEN','COVE_PUBLIC_URL','DATABASE_URL')):
            raise RuntimeError('Snapshot runtime credentials do not match its locked Compose configuration')
        target = self.root / 'restore-compose.json'
        if journal['phase'] in ('safety_verified', 'panel_stopped'):
            command([*self.compose, 'stop', '-t', '30', 'cove'])
            journal['phase'] = 'panel_stopped'
            self.save_journal(journal)
            busy = command([*self.compose, 'exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-Atc',
                            "SELECT count(*) FROM operations WHERE state IN ('queued','running')"]).decode().strip()
            if busy != '0':
                command([*self.compose, 'start', 'cove'])
                journal['phase'] = 'safety_verified'
                self.save_journal(journal)
                raise RuntimeError('Finish or cancel all resource operations before panel recovery')
            journal['phase'] = 'restore_database'
            self.save_journal(journal)
        if journal['phase'] == 'restore_database':
            command([*self.compose, 'exec', '-T', 'database', 'dropdb', '-U', 'cove', '--if-exists', '--force', 'cove'])
            command([*self.compose, 'exec', '-T', 'database', 'createdb', '-U', 'cove', '-O', 'cove', 'cove'])
            with (source / 'database.dump').open('rb') as stream:
                backup.run([*self.compose, 'exec', '-T', 'database', 'pg_restore', '-U', 'cove', '-d', 'cove',
                            '--no-owner', '--no-acl', '--exit-on-error'], stdin=stream, stdout=subprocess.DEVNULL)
            sql = """SET standard_conforming_strings=on;
UPDATE operations SET state='failed',updated_at=now() WHERE state IN ('queued','running');
DO $$ BEGIN
IF to_regclass('panel_recovery_jobs') IS NOT NULL THEN
 EXECUTE 'UPDATE panel_recovery_jobs SET state=''failed'',finished_at=now(),last_error=''Interrupted by panel snapshot recovery'' WHERE state IN (''queued'',''running'')';
END IF;
IF to_regclass('environment_previews') IS NOT NULL THEN
 EXECUTE 'UPDATE environment_previews SET pinned=true,state=CASE WHEN state IN (''creating'',''updating'',''deleting'') THEN ''failed'' ELSE state END,detail=''Pinned for review after panel snapshot recovery''';
END IF;
END $$;
ALTER ROLE cove PASSWORD '""" + password.replace("'", "''") + "';"
            command([*self.compose, 'exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-v', 'ON_ERROR_STOP=1'], input=sql.encode())
            journal['phase'] = 'restore_files'
            self.save_journal(journal)
        if journal['phase'] == 'restore_files':
            for name in files:
                destination = self.installation / name
                if destination.is_symlink() or destination.parent.resolve().relative_to(self.installation) != destination.parent.relative_to(self.installation):
                    raise RuntimeError('Installation files must not use symlinks during recovery')
                destination.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
                temporary = destination.with_name(destination.name + '.cove-restore')
                with (source / name).open('rb') as input_stream:
                    if temporary.exists():
                        temporary.unlink()
                    backup.copy_bounded(input_stream, temporary, plan['config']['max_bytes'])
                os.replace(temporary, destination)
            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
            with os.fdopen(descriptor, 'w') as output:
                output.write(config_text)
                output.flush()
                os.fsync(output.fileno())
            command([*self.compose, 'stop', '-t', '30', 'proxy'])
            proxy = command([*self.compose, 'ps', '-aq', 'proxy']).decode().strip()
            if not proxy or '\n' in proxy:
                raise RuntimeError('Expected exactly one panel proxy')
            command(['docker', 'cp', str(source / 'acme.json'), proxy + ':/data/traefik-acme.json'])
            private_json(self.root / 'updates-paused.json', {'snapshot_id': snapshot, 'restored_at': time.time()})
            journal['phase'] = 'start_restored'
            self.save_journal(journal)
        if journal['phase'] == 'start_restored':
            restored_args = [part for name in compose_files for part in ('-f',str(self.installation/name))]
            command(['docker', 'compose', '--project-directory', str(self.installation),*restored_args, '-f', str(target),
                     'up', '-d', '--no-build', '--pull', 'never', '--wait', '--wait-timeout', '180'])
            journal['phase'] = 'restored'
            self.save_journal(journal)
        self.prune(plan['config'], protected=(snapshot, journal['safety_id']))

    def pending_remote(self):
        return read_json(self.root / 'remote-pending.json')

    def refresh_backup_plan(self, journal):
        old = self.read_plan(journal)['plan']
        self.inspect()
        try:
            envelope = self.api('claim', {'agent_id': self.agent_id, 'updates_paused': self.paused(), 'resume_job_id': old['job_id'], 'pending_recovery': (self.root / 'journal.json').exists()})
        except urllib.error.HTTPError as error:
            if error.code in (404,405):
                return self.read_plan(journal)
            raise
        decoded = self.crypto(envelope, 'open')
        plan = decoded['plan']
        if plan['config']['storage_id'] != old['config']['storage_id']:
            raise RuntimeError('Restore the pending offsite destination before continuing')
        journal['sealed'] = self.crypto(plan, 'seal', local=True)['sealed']
        return decoded

    def reconcile_remote(self):
        pending = self.pending_remote()
        if pending is None:
            return ''
        try:
            decoded = self.refresh_backup_plan(pending)
            private_json(self.root / 'remote-pending.json', pending)
            self.copy_offsite(decoded, pending['snapshot_id'])
            (self.root / 'remote-pending.json').unlink()
            try:
                self.report(decoded['plan'], 'succeeded', pending=(self.root / 'journal.json').exists())
            except urllib.error.HTTPError as error:
                if error.code not in (404,405):
                    raise
            return ''
        except (OSError, ValueError, KeyError, RuntimeError, urllib.error.URLError, subprocess.SubprocessError):
            return 'Offsite copy is pending; local backups remain retained. Correct credentials for the same destination or inspect the host.'

    def discard_unpublished(self, plan):
        if (self.repository / 'config').exists():
            snapshots = json.loads(self.restic('snapshots', '--tag', 'cove-panel-job:' + plan['job_id'], '--json'))
            if len(snapshots)>1:
                raise RuntimeError('Unexpected pending panel snapshot count')
            retained = {item['id'] for item in self.inventory()}
            for snapshot in snapshots:
                if snapshot['id'] not in retained:
                    self.restic('forget',snapshot['id'])
            self.restic('prune')
            self.remove_image_holders(retained)
        (self.root / 'journal.json').unlink(missing_ok=True)

    def run(self, force_backup=False):
        journal = read_json(self.root / 'journal.json')
        had_journal = journal is not None
        if journal is None:
            (self.root / 'restore-compose.json').unlink(missing_ok=True)
        self.cleanup_helpers()
        self.clear_stage()
        remote_error = self.reconcile_remote()
        if journal is None:
            self.inspect()
            try:
                envelope = self.api('claim', {'agent_id': self.agent_id, 'updates_paused': self.paused(), 'force_backup': force_backup, 'pending_recovery': False})
            except urllib.error.HTTPError as error:
                if error.code not in (404, 405):
                    raise
                if not force_backup:
                    return
                journal = self.bootstrap_journal()
                decoded = self.read_plan(journal)
                envelope = None
            if envelope is None:
                plan = decoded['plan']
                self.save_journal(journal)
            elif not envelope.get('sealed'):
                if remote_error:
                    pending = self.pending_remote()
                    plan = self.read_plan(pending)['plan']
                    self.report(plan, 'failed', remote_error)
                else:
                    self.report({'job_id': '', 'action': '', 'actor_id': 'host-idle'}, 'succeeded')
                return
            else:
                decoded = self.crypto(envelope, 'open')
                plan = decoded['plan']
                if force_backup and plan['action'] != 'backup':
                    raise RuntimeError('A pending recovery action prevents panel update')
                journal = {'image': self.image, 'sealed': self.crypto(plan, 'seal', local=True)['sealed'], 'phase': 'prepared'}
                self.save_journal(journal)
        else:
            decoded = self.read_plan(journal)
            plan = decoded['plan']
            self.image = journal['image']
            if force_backup and plan['action'] != 'backup':
                raise RuntimeError('Finish the pending recovery action before updating')
            if plan['action'] == 'backup' and 'bootstrap_plan' not in journal:
                decoded = self.refresh_backup_plan(journal)
                plan = decoded['plan']
                self.save_journal(journal)
        try:
            if plan['action'] == 'restore':
                self.restore(plan, journal)
            elif plan['action'] == 'resume_updates':
                (self.root / 'updates-paused.json').unlink(missing_ok=True)
            else:
                self.inspect()
                if not journal.get('snapshot_id'):
                    journal['snapshot_id'] = self.create_snapshot(plan)
                    self.save_journal(journal)
                self.prune(plan['config'], protected=(journal['snapshot_id'],))
                if plan.get('storage') and not remote_error:
                    private_json(self.root / 'remote-pending.json', journal)
                    try:
                        self.copy_offsite(decoded, journal['snapshot_id'])
                        (self.root / 'remote-pending.json').unlink()
                    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError):
                        remote_error = 'Offsite copy is pending; the verified local snapshot is retained and the host retries.'
            state = 'failed' if remote_error and plan['action'] == 'backup' else 'succeeded'
            private_json(self.root / 'receipt.json', {'job_id': plan['job_id'], 'actor_id': plan['actor_id'], 'action': plan['action'],
                                                     'snapshot_id': plan.get('snapshot_id',journal.get('snapshot_id','')), 'state': state, 'finished_at': time.time()})
            try:
                self.report(plan, state, remote_error)
            except urllib.error.HTTPError as error:
                if error.code not in (404,405) or not ('bootstrap_plan' in journal or plan['action']=='restore' and journal['phase']=='restored'):
                    raise
            (self.root / 'journal.json').unlink()
            (self.root / 'restore-compose.json').unlink(missing_ok=True)
            self.clear_stage()
        except BaseException as failure:
            pending = True
            if plan['action']=='backup' and not journal.get('snapshot_id'):
                try:
                    self.discard_unpublished(plan)
                    pending = False
                except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError):
                    private_json(self.root / 'last-error.json', {'job_id':plan['job_id'],'phase':journal['phase'],'cleanup_deferred':True,'at':time.time()})
            try:
                detail = 'The host retries its private recovery journal.' if pending else 'Unpublished backup data was removed; correct the cause and run another backup.'
                cause = str(failure)[:200].replace('\n',' ').replace('\r',' ') if isinstance(failure,RuntimeError) else 'Host command or transport failed'
                self.report(plan, 'failed', 'Host phase ' + journal['phase'] + ': ' + cause + '. ' + detail, pending=pending)
            except (OSError, ValueError, KeyError, RuntimeError, urllib.error.URLError, subprocess.SubprocessError):
                private_json(self.root / 'last-error.json', {'job_id': plan['job_id'], 'phase': journal['phase'], 'report_deferred': True, 'at': time.time()})
            raise
        if force_backup and remote_error:
            raise RuntimeError('Pre-update offsite snapshot copy has not completed')
        if force_backup and had_journal:
            self.run(force_backup=True)

    def remove_image_holders(self, retained):
        names = command(['docker', 'ps', '-aq', '--filter', 'label=dev.cove.panel-agent=' + self.agent_id,
                         '--filter', 'label=dev.cove.panel-snapshot']).decode().split()
        if len(names) > 832:
            raise RuntimeError('Panel image holder count exceeds its cap')
        for name in names:
            info = json.loads(command(['docker', 'inspect', name]))[0]
            labels = info['Config'].get('Labels') or {}
            if labels.get('dev.cove.panel-agent') == self.agent_id and labels.get('dev.cove.panel-snapshot') not in retained:
                if info['State']['Running']:
                    raise RuntimeError('A panel image holder was unexpectedly started')
                command(['docker', 'rm', '-v', name])

    def prune(self, config, protected=()):
        remote = self.pending_remote()
        protected = set(protected)
        if remote:
            protected.add(remote['snapshot_id'])
        items = sorted(self.inventory(), key=lambda item: item['created_at'], reverse=True)
        cutoff = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=config['keep_days'])
        retained = []
        for index, item in enumerate(items):
            created = datetime.datetime.fromisoformat(item['created_at'])
            if item['id'] in protected or index < config['keep_last'] and created >= cutoff or index == 0:
                retained.append(item)
            else:
                self.restic('forget', item['id'])
        self.restic('prune')
        private_json(self.root / 'snapshots.json', retained)
        self.remove_image_holders({item['id'] for item in retained})

    def remote(self, decoded, *args, source=False):
        transport = decoded['transport']
        plan = decoded['plan']
        if not plan.get('storage'):
            raise RuntimeError('Offsite panel storage is not configured')
        build = self.stage / 'remote-helper'
        build.mkdir(mode=0o700, exist_ok=True)
        (build / 'Dockerfile').write_text(transport['dockerfile'])
        (build / 'entrypoint.sh').write_text(transport['entrypoint'])
        exists = subprocess.run(['docker', 'image', 'inspect', transport['image']], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if exists.returncode:
            command(['docker', 'build', '-q', '-t', transport['image'], str(build)])
        env = os.environ.copy()
        env.update(value.split('=', 1) for value in transport['environment'])
        env['RESTIC_PASSWORD'] = plan['config']['password']
        env.update({'RCLONE_CONTIMEOUT': '10s', 'RCLONE_TIMEOUT': '30s', 'RCLONE_RETRIES': '1', 'RCLONE_LOW_LEVEL_RETRIES': '1'})
        destination = transport['repository'].rstrip('/') + '/cove-panel/' + self.agent_id
        run = ['docker', 'run', '--rm', '--read-only', '--log-driver', 'none', '--tmpfs', '/tmp:rw,size=64m',
               '--memory', '1g', '--pids-limit', '128', '--cpus', '2',
               '--label', 'dev.cove.panel-agent=' + self.agent_id, '--label', 'dev.cove.panel-helper=true']
        for key in [*(value.split('=', 1)[0] for value in transport['environment']), 'RESTIC_PASSWORD', 'RCLONE_CONTIMEOUT', 'RCLONE_TIMEOUT', 'RCLONE_RETRIES', 'RCLONE_LOW_LEVEL_RETRIES']:
            run += ['-e', key]
        for key, target in [('ca_certificate', '/storage-ca.pem'), ('known_hosts', '/storage-known-hosts')]:
            if transport.get(key):
                path = build / key
                path.write_text(transport[key])
                run += ['-v', str(path) + ':' + target + ':ro']
        if source:
            run += ['-v', str(self.repository) + ':/source', '-v', str(self.password) + ':/source-password:ro']
        return command([*run, transport['image'], '-r', destination, '--no-cache', '--retry-lock', '15s', *args], environment=env)

    def copy_offsite(self, decoded, snapshot):
        try:
            self.remote(decoded, 'snapshots', '--json')
        except RuntimeError:
            self.remote(decoded, 'init')
        self.remote(decoded, 'prune')
        self.remote(decoded, 'copy', '--from-repo', '/source', '--from-password-file', '/source-password', snapshot, source=True)
        self.remote(decoded, 'check')
        snapshots = json.loads(self.remote(decoded, 'snapshots', '--json'))
        copied = [item['id'] for item in snapshots if 'cove-panel-job:' + decoded['plan']['job_id'] in item.get('tags', [])]
        if len(copied) != 1:
            raise RuntimeError('Offsite panel snapshot identity could not be reconciled')
        config = decoded['plan']['config']
        snapshots.sort(key=lambda item: item['time'], reverse=True)
        cutoff = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=config['keep_days'])
        remove = [item['id'] for index, item in enumerate(snapshots)
                  if index and item['id'] != copied[0] and (index >= config['keep_last'] or datetime.datetime.fromisoformat(item['time'].replace('Z', '+00:00')) < cutoff)]
        if remove:
            self.remote(decoded, 'forget', *remove)
        self.remote(decoded, 'prune')
        items = self.inventory()
        for item in items:
            if item['id'] == snapshot:
                item['offsite'] = True
        private_json(self.root / 'snapshots.json', items)

    def paused(self):
        return (self.root / 'updates-paused.json').exists()

    def report(self, plan, state, error='', pending=False):
        self.inspect()
        remote = self.pending_remote()
        remote_plan = self.read_plan(remote)['plan'] if remote else None
        return self.api('report', {'pending_recovery': pending, 'pending_remote_job': remote_plan['job_id'] if remote_plan else '', 'agent_id': self.agent_id, 'job_id': plan['job_id'], 'action': plan['action'],
                                   'actor_id': plan['actor_id'], 'snapshot_id': plan.get('snapshot_id', ''), 'state': state, 'error': error,
                                   'updates_paused': self.paused(), 'snapshots': self.inventory()})


def main():
    import fcntl
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['tick', 'before-update', 'check', 'resume-updates'])
    parser.add_argument('--installation', type=Path, default=Path('/opt/cove'))
    parser.add_argument('--lock-fd', type=int)
    args = parser.parse_args()
    if sys.platform != 'linux' or os.geteuid() != 0:
        parser.error('Run as root on the Linux panel host')
    os.umask(0o077)
    installation = root_installation(args.installation)
    descriptor = os.open(installation / '.cove-update.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, 'a') as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
            parser.error('Panel host lock must be a private root-owned regular file')
        if args.command == 'before-update':
            if args.lock_fd is None or os.fstat(args.lock_fd).st_ino != info.st_ino or os.fstat(args.lock_fd).st_dev != info.st_dev:
                parser.error('Pre-update snapshots require the updater lock descriptor')
            fcntl.flock(args.lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        else:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                if args.command == 'tick':
                    return
                raise RuntimeError('Another panel host operation currently holds the recovery lock')
        recovery = Recovery(installation)
        if args.command == 'check':
            recovery.inspect()
            print('Panel host recovery prerequisites verified; preserve the private repository password independently.')
        elif args.command == 'resume-updates':
            if (recovery.root / 'journal.json').exists():
                raise RuntimeError('Finish the pending recovery journal before resuming updates')
            (recovery.root / 'updates-paused.json').unlink(missing_ok=True)
            plan = {'job_id': secrets.token_hex(16), 'actor_id': 'host-cli', 'action': 'resume_updates'}
            private_json(recovery.root / 'receipt.json', dict(plan, state='succeeded', finished_at=time.time()))
            try:
                recovery.report(plan, 'succeeded')
            except urllib.error.HTTPError as error:
                if error.code not in (404,405):
                    raise
        else:
            recovery.run(force_backup=args.command == 'before-update')


if __name__ == '__main__':
    try:
        main()
    except RuntimeError as error:
        sys.exit(str(error) + '. Inspect the private host recovery journal; credentials were not printed.')
    except (OSError, ValueError, KeyError, subprocess.SubprocessError, urllib.error.URLError):
        sys.exit('Panel recovery failed. Inspect the private host recovery journal; credentials were not printed.')
