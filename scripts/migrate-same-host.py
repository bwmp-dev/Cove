"""Root-only, resumable same-host migration tools. Never starts legacy applications."""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import pwd
import re
import socket
import ssl
import subprocess
import tempfile
import time

ROOT = Path('/var/lib/cove')
STATE = ROOT / 'same-host-migration.json'
MODE = ROOT / 'ingress-mode.json'
DOMAINS = ROOT / 'ingress-domains.json'
BRIDGE = 'cove-migration.yaml'
CONTROL = 'cove-migration-control.yaml'
IDENTIFIER = re.compile(r'^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$')


def run(*args):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        # Docker diagnostics can include configuration values; retain them locally only.
        raise RuntimeError('Command failed: ' + ' '.join(args[:2]))
    return result.stdout.decode().strip()


def inspect(name):
    return json.loads(run('docker', 'inspect', name))[0]


def exists(name):
    return bool(run('docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$'))


def atomic(path, value):
    data = json.dumps(value, indent=2).encode()
    fd, temporary = tempfile.mkstemp(prefix='.cove-', dir=path.parent)
    try:
        owner = path.parent.stat()
        os.fchown(fd, owner.st_uid, owner.st_gid)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def rpc(method):
    conn = http.client.HTTPConnection('localhost', timeout=30)
    conn.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    conn.sock.settimeout(30)
    conn.sock.connect('/run/cove/worker.sock')
    try:
        conn.request('POST', '/rpc', json.dumps({'method': method}), {'Content-Type': 'application/json'})
        result = json.load(conn.getresponse())
        if result.get('error'):
            raise RuntimeError(result['error'])
        return result
    finally:
        conn.close()


def prepare_ingress(worker):
    owner = ROOT.stat()
    account = pwd.getpwuid(owner.st_uid)
    result = subprocess.run([worker, 'ingress-prepare'], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            user=owner.st_uid, group=owner.st_gid,
                            extra_groups=os.getgrouplist(account.pw_name, owner.st_gid),
                            env=os.environ | {'HOME': account.pw_dir})
    if result.returncode:
        raise RuntimeError('Worker ingress preparation failed')


@contextmanager
def drained():
    previous = (ROOT / 'draining.json').exists()
    connected = Path('/run/cove/worker.sock').exists()
    ownership = ROOT / 'migration-drain.json'
    if not ownership.exists():
        atomic(ownership, {'resume': not previous})
    resume = json.loads(ownership.read_text())['resume']
    if connected:
        state = rpc('drain')['maintenance']
        if state['active']:
            raise RuntimeError('Worker is draining; wait for active operations, then retry. It remains drained.')
    else:
        for path in (ROOT / 'operations').glob('*.json'):
            if json.loads(path.read_text())['operation']['state'] in ('queued', 'running'):
                raise RuntimeError('Start the worker and reconcile pending operations before migration')
        atomic(ROOT / 'draining.json', True)
    try:
        yield
    finally:
        unresolved = STATE.exists() and json.loads(STATE.read_text())['phase'] in ('preparing', 'bridging', 'handover', 'recovering')
        if not unresolved:
            if resume:
                if connected:
                    rpc('resume')
                else:
                    (ROOT / 'draining.json').unlink()
            ownership.unlink()


def stopped(ids):
    if not ids:
        raise ValueError('Explicit source container IDs are required')
    for identity in ids:
        info = inspect(identity)
        if info['Id'] != identity or info['State']['Running'] or info['State'].get('Paused') or info['State'].get('Restarting'):
            raise RuntimeError('Source identity changed or a source container is still active')


def proxy_api(proxy, kind):
    return proxy_read(proxy, kind + '/routers')


def proxy_read(proxy, path):
    return json.loads(run('docker', 'exec', proxy, 'wget', '-T', '5', '-qO-', 'http://127.0.0.1/cove-migration-control/api/' + path))


def route_issues(router, middlewares):
    issues = []
    rule = re.sub(r'\s+', '', router.get('rule', ''))
    if not re.fullmatch(r'Host\(`[^`]+`\)(?:&&PathPrefix\(`/`\))?', rule):
        issues.append('nontrivial routing rule')
    if router.get('tls', {}).get('options') not in (None, '', 'default'):
        issues.append('custom TLS policy')
    for name in router.get('middlewares', []):
        config = middlewares.get(name, {})
        if 'compress' in config:
            continue
        if config.get('redirectScheme', {}).get('scheme') == 'https':
            continue
        issues.append('middleware ' + name)
    return issues


def routing_review(state):
    middlewares = {m['name']: m for m in proxy_read(state['legacy_id'], 'http/middlewares')}
    policies = []
    for router in proxy_api(state['legacy_id'], 'http'):
        if router['name'].endswith('@internal') or router['name'].startswith(('cove-bridge-', 'cove-migration-control')):
            continue
        policies.append({'rule': router['rule'], 'issues': route_issues(router, middlewares),
                         'priority': router.get('priority', len(router['rule'])),
                         'entrypoints': router.get('entryPoints', [])})
    for router in proxy_api(state['legacy_id'], 'tcp'):
        if not router['name'].startswith('cove-bridge-'):
            policies.append({'rule': router.get('rule', ''), 'issues': ['existing TCP routing']})
    return policies


def policy_applies(policy, domain, policies):
    rule = policy['rule']
    # Only a required, positive hostname predicate can prove a rule unrelated.
    predicate = re.fullmatch(r'(Host|HostRegexp)\(`([^`]+)`\)(?:\s*&&[\s\S]+)?', rule.strip())
    depth, quote, top_or = 0, None, False
    for char in rule:
        if quote:
            if char == quote:
                quote = None
        elif char in ('`', '"'):
            quote = char
        elif char == '(':
            depth += 1
        elif char == ')':
            depth -= 1
            if depth < 0:
                top_or = True
        elif char == '|' and depth == 0:
            top_or = True
    if predicate and not top_or and depth == 0 and quote is None and '!' not in rule:
        kind, pattern = predicate.groups()
        if kind == 'Host' and pattern != domain:
            return False
        if kind == 'HostRegexp' and pattern.startswith('^') and pattern.endswith('$'):
            # This subset has the same meaning in RE2 and Python for DNS names.
            plain = pattern.replace(r'\.', '')
            if re.fullmatch(r'[a-zA-Z0-9_.\-^$()+?*|\[\]]+', plain):
                try:
                    if not re.search(pattern, domain):
                        return False
                except re.error:
                    pass
    if re.sub(r'\s+', '', rule) == 'PathPrefix(`/`)' and 'custom TLS policy' not in policy['issues']:
        entrypoints = policy.get('entrypoints', [])
        priority = policy.get('priority')
        if entrypoints and priority is not None:
            # A lower-priority fallback never handled this host's requests.
            covered = set()
            for candidate in policies:
                normalized = re.sub(r'\s+', '', candidate['rule'])
                if normalized in ('Host(`' + domain + '`)', 'Host(`' + domain + '`)&&PathPrefix(`/`)'):
                    if candidate.get('priority', -1) > priority:
                        covered.update(candidate.get('entrypoints', []))
            if set(entrypoints).issubset(covered):
                return False
    return True


def control_config(entrypoint):
    return {'http': {'routers': {'cove-migration-control': {
        'rule': 'Host(`127.0.0.1`) && PathPrefix(`/cove-migration-control/`)', 'entryPoints': [entrypoint],
        'priority': 2147482000, 'service': 'api@internal', 'middlewares': ['cove-migration-local', 'cove-migration-strip']}},
        'middlewares': {'cove-migration-local': {'ipAllowList': {'sourceRange': ['127.0.0.1/32', '::1/128']}},
                        'cove-migration-strip': {'stripPrefix': {'prefixes': ['/cove-migration-control']}}}}}


def bridge_config(state):
    config = {'tcp': {'routers': {}, 'services': {}}, 'http': {'routers': {}, 'services': {}}}
    for domain in state['domains']:
        key = 'cove-bridge-' + hashlib.sha256(domain.encode()).hexdigest()[:24]
        config['tcp']['routers'][key] = {'rule': 'HostSNI(`' + domain + '`)', 'entryPoints': [state['https_entrypoint']],
                                       'service': key, 'tls': {'passthrough': True}, 'priority': 1000000}
        config['tcp']['services'][key] = {'loadBalancer': {'servers': [{'address': 'cove-ingress:443'}]}}
        config['http']['routers'][key] = {'rule': 'Host(`' + domain + '`)', 'entryPoints': [state['http_entrypoint']],
                                        'service': key, 'priority': 1000000}
        config['http']['services'][key] = {'loadBalancer': {'servers': [{'url': 'http://cove-ingress:80'}]}}
    return config if state['domains'] else {}


def write_bridge(state):
    atomic(Path(state['directory']) / BRIDGE, bridge_config(state))
    desired = {'cove-bridge-' + hashlib.sha256(d.encode()).hexdigest()[:24] + '@file' for d in state['domains']}
    for _ in range(30):
        actual = {r['name'] for r in proxy_api(state['legacy_id'], 'tcp') if r['name'].startswith('cove-bridge-') and r['status'] == 'enabled'}
        if actual == desired:
            return
        time.sleep(1)
    raise RuntimeError('Legacy proxy did not accept bridge configuration')


def check_site(domain, ca, path='/'):
    trust = ssl.create_default_context(cafile=ca)
    with socket.create_connection(('127.0.0.1', 443), timeout=5) as tcp:
        with trust.wrap_socket(tcp, server_hostname=domain) as tls:
            tls.sendall(('GET ' + path + ' HTTP/1.1\r\nHost: ' + domain + '\r\nConnection: close\r\n\r\n').encode())
            response = http.client.HTTPResponse(tls)
            response.begin()
            if not 200 <= response.status < 400:
                raise RuntimeError('HTTPS health check returned ' + str(response.status))


def healthy(state, wait=1):
    deadline = time.monotonic() + wait
    while True:
        try:
            for domain, record in state['domains'].items():
                check_site(domain, state.get('ca'), record['path'])
            return
        except (OSError, RuntimeError, http.client.HTTPException):
            if time.monotonic() >= deadline:
                raise
            time.sleep(1)


def stage(args):
    if STATE.exists() or exists('cove-ingress') or MODE.exists():
        raise RuntimeError('Migration or ingress already exists; inspect status instead of overwriting it')
    legacy = inspect(args.legacy_proxy)
    if not legacy['State']['Running'] or not legacy['Config']['Image'].startswith('traefik:'):
        raise RuntimeError('A running Traefik legacy proxy is required')
    directory = Path(args.dynamic_directory).resolve(strict=True)
    if not directory.is_dir() or (directory / BRIDGE).exists() or (directory / CONTROL).exists():
        raise RuntimeError('Dynamic directory is invalid or already has a bridge file')
    mounts = [m for m in legacy['Mounts'] if m['Type'] == 'bind' and directory.is_relative_to(Path(m['Source']).resolve())]
    if len(mounts) != 1:
        raise RuntimeError('Dynamic directory must be inside one directory bind mount of the legacy proxy')
    for entrypoint in (args.http_entrypoint, args.https_entrypoint):
        if not IDENTIFIER.fullmatch(entrypoint):
            raise ValueError('Invalid entrypoint name')
    # Prove the provider watches this directory before any application is bridged.
    state = {'phase': 'preparing', 'legacy_id': legacy['Id'], 'legacy_restart': legacy['HostConfig']['RestartPolicy'],
             'directory': str(directory), 'http_entrypoint': args.http_entrypoint, 'https_entrypoint': args.https_entrypoint,
             'attestations': {'legacy_cleanup_paused': True, 'legacy_patching_paused': True, 'recorded_at': time.time()},
             'domains': {}, 'ca': str(Path(args.ca).resolve(strict=True)) if args.ca else None}
    atomic(STATE, state)
    finish_stage(args, state)


def finish_stage(args, state):
    atomic(Path(state['directory']) / CONTROL, control_config(state['http_entrypoint']))
    for attempt in range(30):
        try:
            proxy_api(state['legacy_id'], 'tcp')
            break
        except (RuntimeError, ValueError):
            if attempt == 29:
                raise RuntimeError('Legacy file provider/API is unavailable; requires API enabled, watched directory and HTTP port 80')
            time.sleep(1)
    state['routing_review'] = routing_review(state)
    atomic(STATE, state)
    atomic(MODE, {'staged': True, 'tls_challenge': True})
    prepare_ingress(args.worker)
    if inspect('cove-ingress')['HostConfig']['PortBindings']:
        raise RuntimeError('Staged ingress unexpectedly publishes ports')
    if 'cove-ingress' not in inspect(state['legacy_id'])['NetworkSettings']['Networks']:
        run('docker', 'network', 'connect', 'cove-ingress', state['legacy_id'])
    state['phase'] = 'staged'
    atomic(STATE, state)


def bridge(args, state):
    if state['phase'] != 'staged':
        raise RuntimeError('Bridge changes require staged ingress')
    if args.domain in state['domains']:
        raise RuntimeError('Domain is already bridged')
    stopped(args.source_container)
    policies = state['routing_review'] + routing_review(state)
    for policy in policies:
        if policy['issues'] and policy_applies(policy, args.domain, policies):
            raise RuntimeError('Legacy route policy must be migrated explicitly before bridging: ' + ', '.join(policy['issues']))
    routes = json.loads((ROOT / 'ingress.json').read_text())
    found = [(w, r) for w, routes in routes.items() for r in routes if r['domain'] == args.domain]
    if len(found) != 1:
        raise RuntimeError('Domain must first have exactly one saved Cove route')
    workload, route = found[0]
    ids = run('docker', 'ps', '-q', '--filter', 'label=com.docker.compose.project=cove-' + workload,
              '--filter', 'label=com.docker.compose.service=' + route['service']).split()
    if not ids or any(inspect(i)['State'].get('Health', {}).get('Status', 'healthy') != 'healthy' for i in ids):
        raise RuntimeError('Cove target is not running and healthy')
    if not args.path.startswith('/') or any(c in args.path for c in '\r\n '):
        raise ValueError('Health path must be an absolute HTTP path without whitespace')
    previous = json.loads(json.dumps(state))
    state['domains'][args.domain] = {'sources': args.source_container, 'path': args.path}
    state['previous_domains'] = previous['domains']
    state['phase'] = 'bridging'
    atomic(STATE, state)
    try:
        write_bridge(state)
        atomic(DOMAINS, list(state['domains']))
        prepare_ingress(args.worker)
        healthy(state, wait=120)
        stopped(args.source_container)
    except Exception:
        write_bridge(previous)
        atomic(DOMAINS, list(previous['domains']))
        prepare_ingress(args.worker)
        atomic(STATE, previous)
        raise
    state['phase'] = 'staged'
    del state['previous_domains']
    atomic(STATE, state)


def source_unused(source, bind):
    if not bind:
        if run('docker', 'ps', '-q', '--filter', 'volume=' + source):
            raise RuntimeError('Another running container uses the source volume')
        return
    for identity in run('docker', 'ps', '-q').split():
        for mount in inspect(identity)['Mounts']:
            if mount['Type'] == 'bind':
                path = Path(mount['Source']).resolve()
                if path.is_relative_to(source) or Path(source).is_relative_to(path):
                    raise RuntimeError('A running container mounts this source directory or an overlapping path')


def copy_volume(args):
    bind = args.command == 'copy-bind'
    for name in ([args.destination] if bind else [args.source, args.destination]):
        if not IDENTIFIER.fullmatch(name):
            raise ValueError('Invalid volume name')
    if args.source == args.destination:
        raise ValueError('Source and destination must differ')
    stopped(args.source_container)
    if bind:
        source = Path(args.source).resolve(strict=True)
        if not Path(args.source).is_absolute() or not source.is_dir() or source == Path('/') or ':' in str(source):
            raise RuntimeError('Bind source must be an existing absolute directory, excluding the filesystem root')
        args.source = str(source)
    else:
        source = json.loads(run('docker', 'volume', 'inspect', args.source))[0]
        if source['Driver'] != 'local' or source.get('Options'):
            raise RuntimeError('Only ordinary local Docker volumes are supported')
    for identity in args.source_container:
        if not any((m['Type'] == 'bind' and str(Path(m['Source']).resolve()) == args.source) if bind else
                   m.get('Name') == args.source for m in inspect(identity)['Mounts']):
            raise RuntimeError('Source container does not mount the selected storage')
    source_unused(args.source, bind)
    if args.destination in run('docker', 'volume', 'ls', '-q').split():
        raise RuntimeError('Destination must be a new volume; existing data is never overwritten')
    record = ROOT / ('migration-volume-' + args.destination + '.json')
    if record.exists():
        raise RuntimeError('A copy journal already exists; inspect it before retrying')
    state = {'source': args.source, 'source_type': 'bind' if bind else 'volume', 'destination': args.destination,
             'sources': args.source_container, 'state': 'copying'}
    atomic(record, state)
    run('docker', 'volume', 'create', '--label', 'cove.migration=copy', args.destination)
    script = ('tar --numeric-owner --acls --xattrs --sparse -C /source -cpf - . | '
              'tar --numeric-owner --acls --xattrs --same-owner -C /target -xpf -; '
              'tar --numeric-owner --acls --xattrs --sparse -C /source -cpf - . | '
              'tar --numeric-owner --acls --xattrs -C /target --compare -f -; sync')
    run('docker', 'run', '--rm', '--network', 'none', '-v', args.source + ':/source:ro', '-v', args.destination + ':/target',
        'debian:bookworm-slim', 'bash', '-o', 'pipefail', '-ec', script)
    stopped(args.source_container)
    source_unused(args.source, bind)
    state['state'] = 'verified'
    atomic(record, state)


def handover(args, state):
    require_checklist(state)
    if state['phase'] != 'staged' or not state['domains']:
        raise RuntimeError('Handover requires a staged migration with verified domains')
    require_bridged_routes(state)
    for record in state['domains'].values():
        stopped(record['sources'])
    for kind in ('http', 'tcp', 'udp'):
        for router in proxy_api(state['legacy_id'], kind):
            if router['status'] != 'enabled' or router['name'].endswith('@internal') or router['name'].startswith('cove-bridge-') or router['name'] == 'cove-migration-control@file':
                continue
            raise RuntimeError('Legacy proxy still has routes: ' + router['name'] + '; migrate or remove them first')
    healthy(state)
    if exists('cove-ingress-staged'):
        raise RuntimeError('Reserved rollback container already exists')
    state['staged_id'] = inspect('cove-ingress')['Id']
    state['phase'] = 'handover'
    atomic(STATE, state)
    try:
        run('docker', 'update', '--restart=no', state['legacy_id'])
        run('docker', 'stop', state['legacy_id'])
        run('docker', 'update', '--restart=no', state['staged_id'])
        run('docker', 'stop', state['staged_id'])
        run('docker', 'rename', state['staged_id'], 'cove-ingress-staged')
        atomic(MODE, {'staged': False, 'tls_challenge': False})
        prepare_ingress(args.worker)
        healthy(state, wait=30)
        state['phase'] = 'active'
        atomic(STATE, state)
    except Exception:
        rollback_proxy(state)
        raise


def require_bridged_routes(state):
    saved = json.loads((ROOT / 'ingress.json').read_text())
    domains = {r['domain'] for routes in saved.values() for r in routes}
    if domains != set(state['domains']):
        raise RuntimeError('Saved Cove domains differ from verified bridges; reconcile them before switching proxies')


def rollback_proxy(state):
    if state['phase'] not in ('handover', 'active', 'recovering') or not state.get('staged_id'):
        raise RuntimeError('No proxy handover to recover')
    require_bridged_routes(state)
    state['phase'] = 'recovering'
    atomic(STATE, state)
    staged = inspect(state['staged_id'])
    if exists('cove-ingress'):
        current = inspect('cove-ingress')
        if current['Id'] != state['staged_id']:
            if current['Config']['Labels'].get('cove.managed') != 'ingress':
                raise RuntimeError('Refusing to remove an unrelated replacement proxy')
            run('docker', 'rm', '-f', current['Id'])
    if staged['Name'] != '/cove-ingress':
        run('docker', 'rename', state['staged_id'], 'cove-ingress')
    atomic(MODE, {'staged': True, 'tls_challenge': True})
    run('docker', 'update', '--restart=unless-stopped', state['staged_id'])
    run('docker', 'start', state['staged_id'])
    policy = state['legacy_restart']
    restart = policy['Name'] or 'no'
    if restart == 'on-failure' and policy.get('MaximumRetryCount'):
        restart += ':' + str(policy['MaximumRetryCount'])
    run('docker', 'update', '--restart=' + restart, state['legacy_id'])
    run('docker', 'start', state['legacy_id'])
    healthy(state, wait=30)
    state['phase'] = 'staged'
    atomic(STATE, state)


CHECKS = ('data_verified', 'webhooks_updated', 'dns_verified', 'backup_verified', 'cove_authoritative')


def preflight():
    managers = run('docker', 'ps', '-a', '--filter', 'label=coolify.managed=true', '--format', '{{.ID}}').split()
    images = run('docker', 'image', 'ls', '--filter', 'reference=cove-release/*', '--format', '{{.Repository}}:{{.Tag}}').split()
    return {'coolify_detected': bool(managers), 'rollback_tags_at_risk': len(images),
            'legacy_cleanup_verified': False, 'legacy_patching_verified': False,
            'required_actions': ['Pause legacy image, volume, network and builder cleanup',
                                 'Pause host patching that can restart Docker',
                                 'Pause legacy auto-deploy, schedules and proxy management'],
            'verification': 'Docker cannot prove manager settings. The pause flags are operator attestations.'}


def confirm_check(args, state):
    if state['phase'] not in ('staged', 'active'):
        raise RuntimeError('Resolve the pending migration operation before editing its checklist')
    if args.domain not in state['domains']:
        raise ValueError('Domain is not bridged')
    record = state['domains'][args.domain].setdefault('checks', {})
    if args.check == 'cove_authoritative' and not args.confirmed and record.get(args.check, {}).get('confirmed'):
        raise RuntimeError('Cove data ownership cannot be revoked without reconciling newer writes')
    if args.check == 'cove_authoritative' and args.confirmed and record.get(args.check, {}).get('confirmed'):
        return
    record[args.check] = {'confirmed': args.confirmed, 'recorded_at': time.time(), 'evidence': 'operator attestation'}
    atomic(STATE, state)


def require_checklist(state):
    for domain, resource in state['domains'].items():
        missing = [check for check in CHECKS if not resource.get('checks', {}).get(check, {}).get('confirmed')]
        if missing:
            raise RuntimeError('Incomplete cutover checklist for ' + domain + ': ' + ', '.join(missing))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--worker', default='/usr/local/bin/cove-worker')
    parser.add_argument('--legacy-manager-paused', action='store_true', help='Confirm legacy auto-deploy, schedules and proxy management are paused')
    parser.add_argument('--legacy-cleanup-paused', action='store_true', help='Attest legacy image, cache, volume and network cleanup is paused')
    parser.add_argument('--legacy-patching-paused', action='store_true', help='Attest legacy automatic host patching is paused')
    commands = parser.add_subparsers(dest='command', required=True)
    commands.add_parser('preflight')
    check_parser = commands.add_parser('check')
    check_parser.add_argument('--domain', required=True)
    check_parser.add_argument('--check', choices=CHECKS, required=True)
    check_parser.add_argument('--confirmed', action='store_true')
    stage_parser = commands.add_parser('stage')
    stage_parser.add_argument('--legacy-proxy', required=True)
    stage_parser.add_argument('--dynamic-directory', required=True)
    stage_parser.add_argument('--http-entrypoint', default='http')
    stage_parser.add_argument('--https-entrypoint', default='https')
    stage_parser.add_argument('--ca', help='Private CA bundle for acceptance environments only')
    route_parser = commands.add_parser('bridge')
    route_parser.add_argument('--domain', required=True)
    route_parser.add_argument('--source-container', action='append', required=True, help='Full immutable stopped source container ID')
    route_parser.add_argument('--path', default='/')
    for name in ('copy-volume', 'copy-bind'):
        copy_parser = commands.add_parser(name)
        copy_parser.add_argument('--source', required=True)
        copy_parser.add_argument('--destination', required=True)
        copy_parser.add_argument('--source-container', action='append', required=True)
    for name in ('status', 'handover', 'rollback-proxy', 'recover'):
        commands.add_parser(name)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('Run on the Docker host as root')
    if args.command not in ('status', 'preflight', 'check') and not args.legacy_manager_paused:
        parser.error('Pause legacy deployment/schedule/proxy automation, then pass --legacy-manager-paused')
    if args.command not in ('status', 'preflight', 'check', 'recover', 'rollback-proxy') and not (args.legacy_cleanup_paused and args.legacy_patching_paused):
        parser.error('Pause legacy cleanup and host patching, then pass --legacy-cleanup-paused --legacy-patching-paused')
    if args.command == 'preflight':
        print(json.dumps(preflight(), indent=2))
        return
    os.umask(0o077)
    ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (ROOT / 'migration.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.command == 'status':
            print(STATE.read_text() if STATE.exists() else 'No migration staged')
            return
        if args.command == 'check':
            confirm_check(args, json.loads(STATE.read_text()))
            print('Checklist saved as an operator attestation')
            return
        with drained():
            if args.command == 'stage':
                stage(args)
            elif args.command in ('copy-volume', 'copy-bind'):
                copy_volume(args)
            else:
                state = json.loads(STATE.read_text())
                if args.command == 'bridge':
                    bridge(args, state)
                elif args.command == 'handover':
                    handover(args, state)
                elif args.command == 'recover' and state['phase'] == 'preparing':
                    finish_stage(args, state)
                elif args.command == 'recover' and state['phase'] == 'bridging':
                    state['domains'] = state.pop('previous_domains')
                    write_bridge(state)
                    atomic(DOMAINS, list(state['domains']))
                    prepare_ingress(args.worker)
                    state['phase'] = 'staged'
                    atomic(STATE, state)
                else:
                    rollback_proxy(state)
    print(args.command + ' completed; legacy application containers and source data were preserved')


if __name__ == '__main__':
    main()
