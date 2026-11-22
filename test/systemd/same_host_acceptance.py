"""Destructive fixture: only run inside the dedicated isolated migration Docker daemon."""
import importlib.util
import json
import os
import pwd
from pathlib import Path
import subprocess
import time
import urllib.request
import ssl


def main():
    if not Path('/cove-isolated-migration-fixture').exists():
        raise SystemExit('Dedicated fixture required')
    spec = importlib.util.spec_from_file_location('migration', '/fixture/migrate-same-host.py')
    migration = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(migration)
    run = migration.run
    protected = {'rule': 'Host(`private.example.test`)', 'middlewares': ['auth@file']}
    assert migration.route_issues(protected, {'auth@file': {'basicAuth': {'users': ['fixture-only']}}})
    assert not migration.route_issues({'rule': 'Host(`public.example.test`) && PathPrefix(`/`)'}, {})
    fallback = {'rule': 'PathPrefix(`/`)', 'issues': ['middleware errors'], 'priority': 1, 'entrypoints': ['http', 'https']}
    exact = {'rule': 'Host(`public.example.test`) && PathPrefix(`/`)', 'issues': [], 'priority': 50, 'entrypoints': ['http', 'https']}
    assert not migration.policy_applies(fallback, 'public.example.test', [fallback, exact])
    assert migration.policy_applies(fallback, 'other.example.test', [fallback, exact])
    assert migration.policy_applies(fallback | {'priority': 100}, 'public.example.test', [fallback, exact])
    assert migration.policy_applies(fallback, 'public.example.test', [fallback, exact | {'entrypoints': ['https']}])
    assert migration.policy_applies(fallback | {'issues': ['custom TLS policy']}, 'public.example.test', [fallback, exact])
    wildcard = {'rule': r'HostRegexp(`^([a-z0-9-]+\.)?private\.test$`) && PathPrefix(`/`)', 'issues': ['middleware auth']}
    assert not migration.policy_applies(wildcard, 'public.example.test', [wildcard])
    assert migration.policy_applies(wildcard, 'app.private.test', [wildcard])
    assert migration.policy_applies(wildcard | {'rule': wildcard['rule'] + ' || Path(`/admin`)'}, 'public.example.test', [wildcard])
    assert not migration.policy_applies(wildcard | {'rule': wildcard['rule'] + ' && (Path(`/a`) || Path(`/b`))'}, 'public.example.test', [wildcard])
    assert migration.policy_applies({'rule': r'HostRegexp(`\p{L}+`)', 'issues': ['middleware auth']}, 'public.example.test', [])
    assert migration.policy_applies({'rule': 'Host(`public.example.test`) && PathPrefix(`/admin`)', 'issues': ['middleware auth']}, 'public.example.test', [])
    root = Path('/fixture')
    worker_owned = root / 'worker-owned'
    worker_owned.mkdir()
    account = pwd.getpwnam('nobody')
    os.chown(worker_owned, account.pw_uid, account.pw_gid)
    migration.atomic(worker_owned / 'ingress-mode.json', {'staged': True, 'tls_challenge': True})
    owned_stat = (worker_owned / 'ingress-mode.json').stat()
    assert (owned_stat.st_uid, owned_stat.st_gid, owned_stat.st_mode & 0o777) == (account.pw_uid, account.pw_gid, 0o600)
    def worker_identity():
        os.setgid(account.pw_gid)
        os.setuid(account.pw_uid)
    assert subprocess.check_output(['python3', '-c', 'from pathlib import Path; print(Path("/fixture/worker-owned/ingress-mode.json").read_text())'], preexec_fn=worker_identity)
    fake_worker = root / 'identity-worker'
    fake_worker.write_text('#!/usr/bin/env python3\nfrom pathlib import Path\nPath("/fixture/worker-owned/prepared").write_text("prepared")\n')
    fake_worker.chmod(0o755)
    actual_root = migration.ROOT
    try:
        migration.ROOT = worker_owned
        migration.prepare_ingress(str(fake_worker))
    finally:
        migration.ROOT = actual_root
    assert (worker_owned / 'prepared').stat().st_uid == account.pw_uid
    dynamic = Path('/legacy/dynamic')
    dynamic.mkdir(parents=True)
    Path('/legacy/uploads').mkdir()
    Path('/legacy/uploads/upload.txt').write_text('preserved-upload')
    run('docker', 'network', 'create', 'legacy')
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-keyout', '/fixture/key.pem',
        '-out', '/fixture/ca.pem', '-subj', '/CN=pebble', '-addext', 'subjectAltName=DNS:pebble,DNS:localhost')
    (root / 'pebble.json').write_text(json.dumps({'pebble': {'listenAddress': '0.0.0.0:14000',
        'managementListenAddress': '0.0.0.0:15000', 'certificate': '/fixture/ca.pem', 'privateKey': '/fixture/key.pem',
        'httpPort': 80, 'tlsPort': 443}}))
    (root / 'key.pem').chmod(0o644)
    run('docker', 'run', '-d', '--name', 'pebble', '--network', 'legacy', '--add-host', 'app.cove.test:host-gateway',
        '-p', '127.0.0.1:15000:15000', '-e', 'PEBBLE_VA_NOSLEEP=1', '-v', '/fixture:/fixture:ro',
        'ghcr.io/letsencrypt/pebble:latest', '-config', '/fixture/pebble.json')
    run('docker', 'run', '-d', '--name', 'legacy-proxy', '--label', 'coolify.managed=true', '--restart', 'unless-stopped', '--network', 'legacy',
        '-p', '80:80', '-p', '443:443', '-v', '/legacy/dynamic:/dynamic', 'traefik:v3.7.19',
        '--entrypoints.http.address=:80', '--entrypoints.https.address=:443', '--api.dashboard=true', '--api.insecure=false',
        '--providers.file.directory=/dynamic', '--providers.file.watch=true')
    run('docker', 'run', '-d', '--name', 'legacy-db', '-e', 'POSTGRES_PASSWORD=fixture-only',
        '-v', 'legacy-db:/var/lib/postgresql/data', 'postgres:17-alpine')
    run('docker', 'run', '-d', '--name', 'legacy-app', '--network', 'legacy', '-v', 'legacy-files:/www', '-v', '/legacy/uploads:/uploads',
        'busybox:1.37', 'sh', '-ec', 'echo preserved-file > /www/index.html; chmod 640 /www/index.html; httpd -f -p 80 -h /www')
    old_http = {'http': {'routers': {'legacy': {'rule': 'Host(`unmigrated.cove.test`)', 'entryPoints': ['http'], 'service': 'legacy'}},
                        'services': {'legacy': {'loadBalancer': {'servers': [{'url': 'http://legacy-app:80'}]}}}}}
    old_http['http']['routers']['source'] = {'rule': 'Host(`app.cove.test`)', 'entryPoints': ['http', 'https'], 'service': 'legacy', 'priority': 50}
    old_http['http']['routers']['fallback'] = {'rule': 'PathPrefix(`/`)', 'entryPoints': ['http', 'https'], 'service': 'legacy', 'priority': -1000}
    old_http['http']['routers']['unrelated'] = {'rule': r'HostRegexp(`^([a-z0-9-]+\.)?private\.test$`) && (Path(`/a`) || Path(`/b`))', 'entryPoints': ['http'], 'service': 'legacy'}
    migration.atomic(dynamic / 'legacy.yaml', old_http)
    for _ in range(60):
        try:
            run('docker', 'exec', 'legacy-db', 'pg_isready', '-U', 'postgres')
            break
        except RuntimeError:
            time.sleep(1)
    run('docker', 'exec', 'legacy-db', 'psql', '-U', 'postgres', '-c', "create table marker(v text); insert into marker values ('source-data');")
    proxy_id = migration.inspect('legacy-proxy')['Id']
    source_id = migration.inspect('legacy-db')['Id']
    app_id = migration.inspect('legacy-app')['Id']
    source_started = migration.inspect(source_id)['State']['StartedAt']
    proxy_started = migration.inspect(proxy_id)['State']['StartedAt']
    env = os.environ | {'COVE_ACME_DIRECTORY': 'https://pebble:14000/dir', 'COVE_ACME_ROOT': '/fixture/ca.pem'}
    # The CA network is attached after stage; no certificate is requested until deploy.
    for _ in range(30):
        try:
            with urllib.request.urlopen('https://localhost:15000/roots/0', context=ssl.create_default_context(cafile='/fixture/ca.pem')) as response:
                (root / 'issued-root.pem').write_bytes(response.read())
            break
        except OSError:
            time.sleep(1)

    def command(*args, fail=False, worker='/usr/local/bin/cove-worker'):
        proc = subprocess.run(['python3', '/fixture/migrate-same-host.py', '--worker', worker, '--legacy-manager-paused', '--legacy-cleanup-paused', '--legacy-patching-paused', *args],
                              env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if (proc.returncode == 0) == fail:
            raise RuntimeError(proc.stdout.decode())
        if not fail:
            print(proc.stdout.decode().strip(), flush=True)

    command('stage', '--legacy-proxy', proxy_id, '--dynamic-directory', str(dynamic), '--ca', '/fixture/issued-root.pem')
    assert migration.inspect(proxy_id)['State']['StartedAt'] == proxy_started
    assert not migration.inspect('cove-ingress')['HostConfig']['PortBindings']
    denied = subprocess.run(['docker', 'exec', 'legacy-app', 'wget', '-T', '5', '-qO-', '--header=Host: 127.0.0.1',
                             'http://legacy-proxy/cove-migration-control/api/http/routers'], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    assert denied.returncode and b'403' in denied.stderr, 'Migration API route accessible outside proxy loopback'
    # Existing traffic remains served without restarting its proxy.
    assert run('wget', '-qO-', '--header=Host: unmigrated.cove.test', 'http://127.0.0.1') == 'preserved-file'
    run('docker', 'network', 'connect', 'cove-ingress', 'pebble')
    command('copy-volume', '--source', 'legacy-db', '--destination', 'copied-db', '--source-container', source_id, fail=True)
    run('docker', 'stop', source_id, app_id)
    del old_http['http']['routers']['source']
    migration.atomic(dynamic / 'legacy.yaml', old_http)
    command('copy-volume', '--source', 'legacy-db', '--destination', 'copied-db', '--source-container', source_id)
    command('copy-volume', '--source', 'legacy-files', '--destination', 'copied-files', '--source-container', app_id)
    command('copy-bind', '--source', '/legacy/uploads', '--destination', 'copied-uploads', '--source-container', app_id)
    assert run('docker', 'run', '--rm', '-v', 'copied-uploads:/data:ro', 'busybox:1.37', 'cat', '/data/upload.txt') == 'preserved-upload'
    assert Path('/legacy/uploads/upload.txt').read_text() == 'preserved-upload'
    command('copy-volume', '--source', 'legacy-db', '--destination', 'copied-db', '--source-container', source_id, fail=True)
    assert run('docker', 'run', '--rm', '-v', 'copied-files:/data:ro', 'busybox:1.37', 'stat', '-c', '%a', '/data/index.html') == '640'
    worker_log = open('/fixture/worker.log', 'wb')
    worker = subprocess.Popen(['/usr/local/bin/cove-worker', 'daemon'], env=env, stdout=worker_log, stderr=worker_log)
    try:
        for _ in range(30):
            if Path('/run/cove/worker.sock').exists():
                break
            time.sleep(1)
        compose = '''services:
  database:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: fixture-only
    volumes: [database:/var/lib/postgresql/data]
  app:
    image: busybox:1.37
    command: [httpd, -f, -p, '80', -h, /www]
    volumes: [files:/www]
volumes:
  database:
    external: true
    name: copied-db
  files:
    external: true
    name: copied-files
'''
        request = {'id': 'migration-deploy', 'workload': 'migration-app', 'action': 'deploy', 'kind': 'service',
                   'compose': compose, 'routes': [{'domain': 'app.cove.test', 'service': 'app', 'port': 80}]}
        def rpc(body):
            proc = subprocess.run(['/usr/local/bin/cove-worker', 'rpc'], input=json.dumps(body).encode(), stdout=subprocess.PIPE, check=True)
            data = json.loads(proc.stdout)
            if data.get('error'):
                raise RuntimeError(data['error'])
            return data
        probe = rpc({'method': 'probe'})['probe']
        assert probe['shared_host']['coolify_detected'] and not probe['shared_host']['cleanup_verified']
        assert probe['metrics']['memory_total'] > 0 and probe['metrics']['disk_total'] > 0
        assert 0 <= probe['metrics']['cpu_percent'] <= 100
        rpc({'method': 'submit', 'request': request})
        for _ in range(150):
            op = rpc({'method': 'status', 'id': request['id']})['operation']
            if op['state'] == 'succeeded':
                break
            if op['state'] not in ('queued', 'running'):
                raise RuntimeError(str(rpc({'method': 'logs', 'id': request['id']})))
            time.sleep(1)
        else:
            raise RuntimeError('Deployment timeout')
        command('bridge', '--domain', 'app.cove.test', '--source-container', app_id, '--source-container', source_id)
        command('handover', fail=True)  # Incomplete checklist blocks takeover.
        for check in migration.CHECKS:
            command('check', '--domain', 'app.cove.test', '--check', check, '--confirmed')
        command('handover', fail=True)  # Unmigrated route blocks takeover.
        (dynamic / 'legacy.yaml').unlink()
        time.sleep(3)
        target_db = 'cove-migration-app-database-1'
        target_app = 'cove-migration-app-app-1'
        assert run('docker', 'exec', target_db, 'psql', '-U', 'postgres', '-Atc', 'select v from marker') == 'source-data'
        app_before = migration.inspect(target_app)['State']['StartedAt']
        db_before = migration.inspect(target_db)['State']['StartedAt']
        run('docker', 'exec', target_db, 'psql', '-U', 'postgres', '-c', "insert into marker values ('new-cove-write');")
        command('handover', worker='/bin/false', fail=True)  # Failure after old proxy stops must restore it.
        assert migration.inspect(proxy_id)['State']['Running']
        migration.check_site('app.cove.test', '/fixture/issued-root.pem')
        command('handover')
        assert not migration.inspect(proxy_id)['State']['Running']
        assert migration.inspect('cove-ingress')['HostConfig']['PortBindings']
        command('rollback-proxy')
        assert migration.inspect(proxy_id)['State']['Running']
        assert run('docker', 'exec', target_db, 'psql', '-U', 'postgres', '-Atc', 'select count(*) from marker') == '2'
        assert migration.inspect(target_app)['State']['StartedAt'] == app_before
        assert migration.inspect(target_db)['State']['StartedAt'] == db_before
        assert not migration.inspect(source_id)['State']['Running']
        assert migration.inspect(source_id)['State']['StartedAt'] == source_started
        # Simulate process death after replacement starts but before the commit is persisted.
        original_health = migration.healthy
        health_calls = 0
        def crash_after_start(state, wait=1):
            nonlocal health_calls
            health_calls += 1
            original_health(state, wait)
            if health_calls == 2:
                raise KeyboardInterrupt('simulated process death')
        migration.healthy = crash_after_start
        from types import SimpleNamespace
        previous_env = os.environ.copy()
        os.environ.update(env)
        try:
            with migration.drained():
                migration.handover(SimpleNamespace(worker='/usr/local/bin/cove-worker'), json.loads(migration.STATE.read_text()))
        except KeyboardInterrupt:
            pass
        finally:
            migration.healthy = original_health
            os.environ.clear()
            os.environ.update(previous_env)
        assert json.loads(migration.STATE.read_text())['phase'] == 'handover'
        command('recover')
        assert not (migration.ROOT / 'draining.json').exists()
        assert migration.inspect(target_app)['State']['StartedAt'] == app_before
        # Independently open the preserved source volume: it contains no Cove-side write.
        run('docker', 'start', source_id)
        for _ in range(30):
            try:
                assert run('docker', 'exec', source_id, 'psql', '-U', 'postgres', '-Atc', 'select count(*) from marker') == '1'
                break
            except (RuntimeError, AssertionError):
                time.sleep(1)
        else:
            raise RuntimeError('Source rollback data did not match')
        run('docker', 'stop', source_id)
        command('handover')
        print('PASS: cold volume copy, source preservation, TLS passthrough/ACME, existing routes, guarded handover, failed handover recovery, proxy rollback with new writes and unchanged app containers', flush=True)
    finally:
        worker.terminate()
        worker.wait(timeout=30)
        worker_log.close()


if __name__ == '__main__':
    main()
