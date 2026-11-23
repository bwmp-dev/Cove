"""Real panel database, encrypted repository, retention and recovery in an isolated daemon."""
import base64
import copy
import fcntl
import importlib.util
import http.server
import json
import os
from pathlib import Path
import subprocess
import ssl
import threading
import time
import urllib.error
import urllib.request


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    if not Path('/fixture/cove').is_file() or os.geteuid() != 0:
        raise SystemExit('Dedicated isolated Docker fixture required')
    os.umask(0o077)
    module = load('panel_recovery', '/fixture/panel-recovery.py')
    root = Path('/fixture/panel')
    (root / 'deploy').mkdir(parents=True)
    for name in ('.env', 'deploy/traefik.yaml', 'deploy/traefik-panel.yaml'):
        (root / name).write_text('preserved-before-restore')
    (Path('/fixture') / 'Dockerfile').write_text('''FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 cove
COPY cove /app/bin/cove
USER cove
LABEL org.opencontainers.image.version="fixture-before" org.opencontainers.image.revision="fixture-only"
ENTRYPOINT ["/app/bin/cove"]
''')
    run(['docker', 'build', '-q', '-t', 'cove-panel-recovery-fixture:local', '/fixture'], stdout=subprocess.DEVNULL)
    base = os.environ.get('COVE_RECOVERY_BASE_PATH', '')
    public_url = 'https://localhost:8443' + base
    key = base64.b64encode(os.urandom(32)).decode()
    token = os.urandom(32).hex()
    gateway = json.loads(module.command(['docker', 'network', 'inspect', 'bridge']))[0]['IPAM']['Config'][0]['Gateway']
    cert = Path('/fixture/cert.pem')
    private = Path('/fixture/key.pem')
    run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=Cove local recovery fixture',
         '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1,IP:' + gateway, '-addext', 'basicConstraints=critical,CA:TRUE',
         '-keyout', str(private), '-out', str(cert)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    Path('/usr/local/share/ca-certificates/cove-recovery-fixture.crt').write_bytes(cert.read_bytes())
    run(['update-ca-certificates'], stdout=subprocess.DEVNULL)
    compose = {
        'name': 'panel-recovery-fixture',
        'services': {
            'database': {'image': 'postgres:17-alpine', 'environment': {'POSTGRES_USER': 'cove', 'POSTGRES_DB': 'cove', 'POSTGRES_PASSWORD': 'synthetic-panel-password'},
                         'volumes': ['database:/var/lib/postgresql/data'],
                         'healthcheck': {'test': ['CMD', 'pg_isready', '-U', 'cove'], 'interval': '1s', 'timeout': '1s', 'retries': 60}},
            'cove': {'image': 'cove-panel-recovery-fixture:local', 'read_only': True, 'tmpfs': ['/tmp'],
                     'environment': {'DATABASE_URL': 'postgres://cove:synthetic-panel-password@database:5432/cove?sslmode=disable',
                                     'COVE_ENCRYPTION_KEY': key, 'COVE_ADMIN_TOKEN': token, 'COVE_PUBLIC_URL': public_url,
                                     'COVE_LISTEN': '0.0.0.0:8080'},
                     'ports': ['127.0.0.1:8081:8080'],
                     'depends_on': {'database': {'condition': 'service_healthy'}},
                     'healthcheck': {'test': ['CMD', 'wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:8080/healthz'], 'interval': '1s', 'timeout': '1s', 'retries': 60}},
            'proxy': {'image': 'busybox:1.37', 'command': ['sh', '-ec', 'mkdir -p /data; test -f /data/traefik-acme.json || echo {} > /data/traefik-acme.json; sleep 10000'], 'volumes': ['certificates:/data']},
        }, 'volumes': {'database': {}, 'certificates': {}}}
    (root / 'compose.yaml').write_text(json.dumps(compose))
    cmd = ['docker', 'compose', '--project-directory', str(root)]
    run([*cmd, 'up', '-d', '--wait', '--wait-timeout', '120'], stdout=subprocess.DEVNULL)
    recovery = module.Recovery(root)
    origin = recovery.inspect()
    info = json.loads(module.command(['docker', 'inspect', module.command([*cmd, 'ps', '-q', 'cove']).decode().strip()]))[0]
    assert info['Config']['User'] == 'cove'
    assert all(mount['Destination'] != '/var/run/docker.sock' for mount in info['Mounts'])
    assert origin == public_url
    sql = lambda value: module.command([*cmd, 'exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-Atc', value]).decode().strip()
    sql("INSERT INTO projects(id,name,team_id) VALUES('before','Preserved','default')")
    config = {'enabled': True, 'interval_hours': 24, 'keep_last': 2, 'keep_days': 30, 'max_bytes': 64 << 20, 'storage_id': ''}
    plan = {'kind': 'cove.panel-backup.v1', 'job_id': 'fixture-first', 'agent_id': recovery.agent_id, 'actor_id': 'fixture', 'action': 'backup', 'config': config}
    envelope = recovery.crypto(plan, 'seal')
    assert 'synthetic-panel-password' not in json.dumps(envelope)
    assert recovery.crypto(envelope, 'open')['plan']['job_id'] == 'fixture-first'
    recovery.clear_stage()
    first = recovery.create_snapshot(plan)
    assert len(first) == 64 and recovery.create_snapshot(plan) == first
    assert sql('SELECT panel_backup_at IS NOT NULL FROM control_settings WHERE id=1') == 't'
    assert sql('SELECT name FROM projects WHERE id=\'before\'') == 'Preserved'
    print('PASS: real non-root panel, no Docker socket, sealed plan, verified encrypted snapshot and idempotent snapshot retry', flush=True)
    snapshots = [first]
    for index in range(2):
        plan['job_id'] = 'fixture-next-' + str(index)
        snapshots.append(recovery.create_snapshot(plan))
        recovery.prune(config)
    assert len(recovery.inventory()) == 2
    assert first not in [item['id'] for item in recovery.inventory()]
    assert len(json.loads(recovery.restic('snapshots', '--json'))) == 2
    holders = module.command(['docker', 'ps', '-aq', '--filter', 'label=dev.cove.panel-agent=' + recovery.agent_id,
                              '--filter', 'label=dev.cove.panel-snapshot']).decode().split()
    assert len(holders) == 6
    print('PASS: automatic age/count retention prunes encrypted snapshots and only their owned image holders', flush=True)
    target = snapshots[-1]
    sql("INSERT INTO projects(id,name,team_id) VALUES('after','Must disappear','default')")
    (root / '.env').write_text('mutated-after-snapshot')
    restore_plan = dict(plan, job_id='fixture-restore', action='restore', snapshot_id=target)
    journal = {'image': recovery.image, 'sealed': recovery.crypto(restore_plan, 'seal', local=True)['sealed'], 'phase': 'prepared'}
    recovery.save_journal(journal)
    original_save = recovery.save_journal

    class Interrupted(Exception):
        pass

    def interrupt_after_database(value):
        original_save(value)
        if value['phase'] == 'restore_files':
            raise Interrupted()

    recovery.save_journal = interrupt_after_database
    try:
        recovery.restore(restore_plan, journal)
    except Interrupted:
        pass
    else:
        raise AssertionError('Recovery interruption was not exercised')
    assert module.read_json(recovery.root / 'journal.json')['phase'] == 'restore_files'
    resumed = module.Recovery(root)
    resumed.clear_stage()
    resumed.image = journal['image']
    decoded = resumed.crypto({'sealed': journal['sealed']}, 'open', local=True, image=journal['image'])
    resumed.restore(decoded['plan'], journal)
    assert sql("SELECT count(*) FROM projects WHERE id='after'") == '0'
    assert sql("SELECT name FROM projects WHERE id='before'") == 'Preserved'
    assert (root / '.env').read_text() == 'preserved-before-restore'
    assert resumed.paused()
    assert any(item['id'] == journal['safety_id'] for item in resumed.inventory())
    assert len(json.loads(resumed.restic('snapshots', '--json'))) <= 4
    print('PASS: verified safety snapshot, interrupted database restore replay, exact data/file recovery, pinned image restart and updates paused', flush=True)
    resumed.inspect()
    invalid = copy.deepcopy(restore_plan)
    invalid['snapshot_id'] = '../not-a-snapshot'
    try:
        resumed.restore(invalid, {'phase': 'prepared'})
    except RuntimeError:
        pass
    else:
        raise AssertionError('Invalid snapshot accepted')
    assert sql("SELECT name FROM projects WHERE id='before'") == 'Preserved'
    print('PASS: invalid restore target rejected before data changes', flush=True)

    class Proxy(http.server.BaseHTTPRequestHandler):
        backend_port = 8081
        def handle_request(self):
            size = int(self.headers.get('Content-Length', '0'))
            assert size <= 1 << 20
            body = self.rfile.read(size) if size else None
            headers = {key: value for key, value in self.headers.items() if key.lower() not in ('host', 'content-length')}
            request = urllib.request.Request('http://127.0.0.1:' + str(self.backend_port) + self.path, data=body, method=self.command, headers=headers)
            try:
                response = urllib.request.urlopen(request, timeout=60)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                payload = response.read((1 << 20) + 1)
                self.send_response(response.status)
                self.send_header('Content-Type', response.headers.get('Content-Type', 'application/json'))
                self.send_header('Content-Length', str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        do_GET = do_PUT = do_POST = handle_request

        def log_message(self, *args):
            return

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 8443), Proxy)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(cert, private)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    def api(method, path, value=None):
        request = urllib.request.Request(origin + path, data=json.dumps(value).encode() if value is not None else None,
                                         method=method, headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    resumed.report(restore_plan, 'succeeded')
    (resumed.root / 'journal.json').unlink()
    status = api('GET', '/api/settings/panel-backup')
    assert status['updates_paused'] and status['agent_id'] == resumed.agent_id
    api('POST', '/api/settings/panel-backup/actions', {'action': 'resume_updates', 'confirm': True})
    resumed.run()
    assert not resumed.paused()
    s3 = Path('/fixture/s3')
    (s3 / 'backups/panel').mkdir(parents=True)
    run(['docker', 'run', '-d', '--name', 'recovery-offsite-s3', '-p', '9910:9910',
         '-v', str(s3) + ':/data', '-v', str(cert) + ':/cert.pem:ro', '-v', str(private) + ':/key.pem:ro',
         'rclone/rclone:1.76.0', 'serve', 's3', '/data', '--addr', '0.0.0.0:9910', '--cert', '/cert.pem', '--key', '/key.pem',
         '--auth-key', 'fixture,synthetic-offsite-secret'], stdout=subprocess.DEVNULL)
    location = api('POST', '/api/storage-locations', {'name': 'Panel fixture S3', 'storage': {
        'protocol': 's3', 'endpoint': 'https://' + gateway + ':9910', 'path': 'backups/panel',
        'access_key': 'fixture', 'secret_key': 'synthetic-offsite-secret', 'region': 'us-east-1', 'ca_certificate': cert.read_text()}})
    settings = dict(config, storage_id=location['id'], password='independent-synthetic-offsite-password')
    api('PUT', '/api/settings/panel-backup', settings)
    resumed.run()
    status = api('GET', '/api/settings/panel-backup')
    assert status['password_set'] and 'password' not in status['config']
    assert any(job['state'] == 'succeeded' and job['action'] == 'backup' for job in status['jobs'])
    assert any(item['offsite'] for item in status['snapshots'])
    assert not (resumed.root / 'journal.json').exists()
    assert (s3 / 'backups/panel/cove-panel' / resumed.agent_id / 'config').is_file()
    print('PASS: HTTPS signed host claim/report, scheduled backup, sealed offsite credentials, real TLS S3 encrypted copy and update resume', flush=True)
    count = len(status['snapshots'])
    resumed.run()
    assert len(api('GET', '/api/settings/panel-backup')['snapshots']) == count
    print('PASS: schedule interval avoids duplicate backups', flush=True)
    original_copy = resumed.copy_offsite

    def lost_copy_ack(decoded, snapshot):
        original_copy(decoded, snapshot)
        raise RuntimeError('Injected lost copy acknowledgement')

    api('POST', '/api/settings/panel-backup/actions', {'action': 'backup'})
    resumed.copy_offsite = lost_copy_ack
    resumed.run()
    status = api('GET', '/api/settings/panel-backup')
    assert status['offsite_pending'] and not status['pending_recovery']
    assert (resumed.root / 'remote-pending.json').is_file() and not (resumed.root / 'journal.json').exists()
    pending_snapshot = resumed.pending_remote()['snapshot_id']
    resumed.copy_offsite = original_copy
    resumed.run()
    status = api('GET', '/api/settings/panel-backup')
    assert not status['offsite_pending'] and not resumed.pending_remote()
    assert any(item['id'] == pending_snapshot and item['offsite'] for item in status['snapshots'])
    print('PASS: interrupted offsite acknowledgement, bounded pending copy, idempotent reconciliation without duplicate snapshot', flush=True)
    original_verify = module.backup.verify
    retained_before_failure = {item['id'] for item in resumed.inventory()}

    def reject_verification(*args, **kwargs):
        raise RuntimeError('Injected isolated verifier rejection')

    module.backup.verify = reject_verification
    api('POST', '/api/settings/panel-backup/actions', {'action':'backup'})
    try:
        resumed.run()
    except RuntimeError:
        pass
    else:
        raise AssertionError('Rejected verification was published')
    finally:
        module.backup.verify = original_verify
    status = api('GET','/api/settings/panel-backup')
    assert not status['pending_recovery'] and not (resumed.root/'journal.json').exists()
    assert {item['id'] for item in resumed.inventory()} == retained_before_failure
    assert {item['id'] for item in json.loads(resumed.restic('snapshots','--json'))} == retained_before_failure
    assert sql("SELECT name FROM projects WHERE id='before'") == 'Preserved'
    print('PASS: isolated verifier rejection publishes nothing, removes unpublished encrypted data and keeps source online',flush=True)
    target = api('GET', '/api/settings/panel-backup')['snapshots'][-1]['id']
    sql("INSERT INTO projects(id,name,team_id) VALUES('api-restore-after','Must disappear','default')")
    api('POST', '/api/settings/panel-backup/actions', {'action': 'restore', 'snapshot_id': target, 'confirm': True})
    resumed.run()
    assert sql("SELECT count(*) FROM projects WHERE id='api-restore-after'") == '0'
    status = api('GET', '/api/settings/panel-backup')
    assert status['updates_paused'] and any(job['state'] == 'succeeded' and job['action'] == 'restore' for job in status['jobs'])
    assert all(job['state'] not in ('queued', 'running') for job in status['jobs'])
    print('PASS: API-queued one-click recovery, restored job cancellation, durable success receipt after database rollback', flush=True)
    tools = Path('/fixture/fake-systemd')
    tools.mkdir(mode=0o700)
    Path('/etc/systemd/system').mkdir(parents=True,exist_ok=True)
    (tools / 'systemctl').write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> /fixture/systemctl-calls\n')
    (tools / 'systemctl').chmod(0o700)
    run(['sh', '/fixture/install-panel-recovery.sh', str(root)], env=os.environ | {'PATH': str(tools) + ':' + os.environ['PATH']}, stdout=subprocess.DEVNULL)
    service = Path('/etc/systemd/system/cove-panel-recovery.service').read_text()
    assert 'WorkingDirectory=' + str(root) in service and 'TimeoutStartSec=45min' in service
    assert '/usr/local/lib/cove/panel-recovery.py tick --installation ' + str(root) in service
    assert 'OnUnitActiveSec=1min' in Path('/etc/systemd/system/cove-panel-recovery.timer').read_text()
    assert 'enable --now cove-panel-recovery.timer' in Path('/fixture/systemctl-calls').read_text()
    insecure = Path('/fixture/insecure-installation')
    insecure.mkdir(mode=0o777)
    insecure.chmod(0o777)
    try:
        module.Recovery(insecure)
    except RuntimeError:
        pass
    else:
        raise AssertionError('Writable host installation accepted')
    print('PASS: installer prerequisites, generated root-host timer/service, private metadata and unsafe path rejection', flush=True)

    legacy_root = Path('/fixture/legacy-panel')
    (legacy_root / 'deploy').mkdir(parents=True)
    for name in ('.env', 'deploy/traefik.yaml', 'deploy/traefik-panel.yaml'):
        (legacy_root / name).write_text('legacy-fixture-before')
    old_dockerfile = Path('/fixture/OldDockerfile')
    old_dockerfile.write_text(Path('/fixture/Dockerfile').read_text().replace('COPY cove /app/bin/cove', 'COPY cove-old /app/bin/cove').replace('fixture-before', 'fixture-old-api'))
    run(['docker', 'build', '-q', '-f', str(old_dockerfile), '-t', 'cove-panel-recovery-fixture:old-api', '/fixture'], stdout=subprocess.DEVNULL)
    legacy_compose = copy.deepcopy(compose)
    legacy_compose['name'] = 'legacy-panel-recovery-fixture'
    legacy_compose['services']['cove']['image'] = 'cove-panel-recovery-fixture:old-api'
    legacy_compose['services']['cove']['ports'] = ['127.0.0.1:8082:8080']
    legacy_compose['services']['cove']['environment']['COVE_PUBLIC_URL'] = 'https://localhost:8444'
    (legacy_root / 'compose.yaml').write_text(json.dumps(legacy_compose))
    legacy_cmd = ['docker', 'compose', '--project-directory', str(legacy_root)]
    run([*legacy_cmd, 'up', '-d', '--wait', '--wait-timeout', '120'], stdout=subprocess.DEVNULL)
    legacy_sql = lambda query: module.command([*legacy_cmd, 'exec', '-T', 'database', 'psql', '-U', 'cove', '-d', 'cove', '-Atc', query]).decode().strip()
    timestamp_columns = "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='control_settings' AND column_name='panel_backup_at'"
    assert legacy_sql(timestamp_columns) == '0'

    class LegacyProxy(Proxy):
        backend_port = 8082

    legacy_server = http.server.ThreadingHTTPServer(('127.0.0.1', 8444), LegacyProxy)
    legacy_server.socket = context.wrap_socket(legacy_server.socket, server_side=True)
    threading.Thread(target=legacy_server.serve_forever, daemon=True).start()
    with (legacy_root / '.cove-update.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        run(['python3', '/fixture/panel-recovery.py', 'before-update', '--installation', str(legacy_root), '--lock-fd', str(lock.fileno())], pass_fds=(lock.fileno(),))
    legacy = module.Recovery(legacy_root)
    legacy.inspect()
    old_snapshot = legacy.inventory()[0]['id']
    assert legacy.inventory()[0]['verified']
    assert legacy_sql(timestamp_columns) == '0'
    assert int(legacy_sql("SELECT count(*) FROM audit_events WHERE action='panel backup verified' AND status=200")) > 0
    assert module.read_json(legacy.root / 'receipt.json')['state'] == 'succeeded'
    assert not (legacy.root / 'journal.json').exists()
    legacy_compose['services']['cove']['image'] = 'cove-panel-recovery-fixture:local'
    (legacy_root / 'compose.yaml').write_text(json.dumps(legacy_compose))
    run([*legacy_cmd, 'up', '-d', '--wait', '--wait-timeout', '120', 'cove'], stdout=subprocess.DEVNULL)
    legacy.run()
    legacy_origin = 'https://localhost:8444'

    def legacy_api(method, path, value=None):
        request = urllib.request.Request(legacy_origin + path, data=json.dumps(value).encode() if value is not None else None,
                                         method=method, headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    legacy_status = legacy_api('GET', '/api/settings/panel-backup')
    assert any(item['id'] == old_snapshot for item in legacy_status['snapshots'])
    legacy_api('POST', '/api/settings/panel-backup/actions', {'action': 'restore', 'snapshot_id': old_snapshot, 'confirm': True})
    legacy.run()
    assert legacy.paused() and not (legacy.root / 'journal.json').exists()
    assert module.read_json(legacy.root / 'receipt.json')['action'] == 'restore'
    old_id = module.command([*legacy_cmd, 'ps', '-q', 'cove']).decode().strip()
    image = json.loads(module.command(['docker', 'inspect', old_id]))[0]['Image']
    expected = json.loads(module.command(['docker', 'image', 'inspect', 'cove-panel-recovery-fixture:old-api']))[0]['Id']
    assert image == expected
    try:
        legacy_api('GET', '/api/settings/panel-backup')
    except urllib.error.HTTPError as error:
        assert error.code == 404
    else:
        raise AssertionError('Historical binary unexpectedly exposes the new recovery API')
    run(['python3', '/fixture/panel-recovery.py', 'resume-updates', '--installation', str(legacy_root)])
    assert not legacy.paused()
    print('PASS: actual historical panel without new API/CLI, mandatory pre-update bootstrap snapshot, new-panel upgrade, one-click exact downgrade and explicit host resume', flush=True)
    legacy_server.shutdown()
    server.shutdown()


if __name__ == '__main__':
    main()
