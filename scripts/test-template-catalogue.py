"""Validate the shipped catalogue and optionally start isolated, disposable template stacks."""
import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
CATALOGUE = ROOT / 'internal/catalog/builtin.json'
EVIDENCE = ROOT / 'internal/catalog/validation.json'


def run(args, **kwargs):
    return subprocess.run([str(arg) for arg in args], capture_output=True, text=True, encoding='utf-8', errors='replace', **kwargs)


def values(template):
    result = {}
    for variable in template['variables']:
        value = variable['default']
        if variable['generated']:
            value = secrets.token_hex(32)
        if not value:
            value = {'email': 'admin@example.com', 'hostname': 'app.example.test', 'url': 'https://app.example.test', 'integer': '1', 'boolean': 'true'}.get(variable['type'], 'catalogue-check')
        result[variable['key']] = value
    if template['web'] and template['web']['url_variable']:
        result[template['web']['url_variable']] = 'https://app.example.test'
    return result


def check_template(template, start, timeout, client_image):
    environment = os.environ.copy()
    supplied = values(template)
    environment.update(supplied)
    for key in ('COMPOSE_FILE', 'COMPOSE_PROJECT_NAME', 'COMPOSE_PROFILES', 'DOCKER_HOST', 'DOCKER_CONTEXT'):
        if key.startswith('COMPOSE_'):
            environment.pop(key, None)
    record = {'compose_sha256': hashlib.sha256(template['compose'].encode()).hexdigest(), 'schema_validated': True,
              'compose_config': False, 'runtime': 'config_only', 'checked_at': datetime.now(timezone.utc).isoformat(), 'message': 'Runtime not requested.'}
    with tempfile.TemporaryDirectory(prefix='cove-template-') as temporary:
        directory = Path(temporary)
        (directory / 'compose.yaml').write_text(template['compose'], encoding='utf-8')
        project = 'cove-catalogue-' + uuid.uuid4().hex
        command = ['docker', 'compose', '--project-name', project, '--project-directory', directory, '--file', directory / 'compose.yaml']
        config = run([*command, 'config', '--format', 'json'], env=environment)
        if config.returncode:
            record['message'] = 'docker compose config failed; review the source Compose and typed variable declarations.'
            return record
        record['compose_config'] = True
        resolved = json.loads(config.stdout)
        for service in resolved['services'].values():
            if service.get('build') or service.get('env_file'):
                raise RuntimeError('Catalogue must be self-contained')
        if not start:
            return record
        if template['capabilities']:
            record['message'] = 'Runtime omitted: this shared local daemon has unrelated workloads; host-access templates require a dedicated acceptance worker.'
            return record
        info = run(['docker', 'info', '--format', '{{.MemTotal}}'])
        if info.returncode or template['minimum']['memory_mb'] * 1024 * 1024 > int(info.stdout.strip() or '0'):
            record['message'] = 'Runtime omitted: the Docker engine has less memory than this template requires.'
            return record
        override = {'networks': {'default': {'internal': True}}, 'services': {}}
        for name, service in resolved['services'].items():
            override['services'][name] = {'restart': 'no'}
            if service.get('ports'):
                record['message'] = 'Runtime omitted: published host ports require a dedicated fixture.'
                return record
        (directory / 'isolation.json').write_text(json.dumps(override), encoding='utf-8')
        command += ['--file', directory / 'isolation.json']
        try:
            pulled = run([*command, 'pull', '--quiet'], env=environment, timeout=300)
            if pulled.returncode:
                record['message'] = 'Runtime unavailable: the pinned image could not be pulled from its registry.'
                return record
            up = run([*command, 'up', '--detach', '--no-build', '--pull', 'never'], env=environment, timeout=120)
            if up.returncode:
                record['message'] = 'Runtime failed to start; inspect this template on a dedicated acceptance worker.'
                diagnostic = up.stderr[-1600:]
                for variable in template['variables']:
                    if variable['type'] == 'secret' and supplied.get(variable['key']):
                        diagnostic = diagnostic.replace(supplied[variable['key']], '[redacted]')
                print(template['id'] + ': Docker Compose startup failed: ' + diagnostic, flush=True)
                return record
            deadline = time.monotonic() + timeout
            rows = []
            while time.monotonic() < deadline:
                ids = run([*command, 'ps', '--all', '--quiet'], env=environment)
                if ids.returncode:
                    break
                containers = ids.stdout.split()
                if containers:
                    inspection = run(['docker', 'inspect', *containers], env=environment)
                    if inspection.returncode:
                        break
                    rows = json.loads(inspection.stdout)
                    if any(row['State']['Status'] in ('dead', 'exited') for row in rows):
                        break
                    if len(rows) == len(resolved['services']) and all(row['State']['Status'] == 'running' and row['State'].get('Health', {}).get('Status', 'healthy') == 'healthy' for row in rows):
                        break
                time.sleep(2)
            healthy = len(rows) == len(resolved['services']) and all(row['State']['Status'] == 'running' and row['State'].get('Health', {}).get('Status', 'healthy') == 'healthy' for row in rows)
            if healthy and template['id'] == 'dragonfly':
                program = '''import os,socket
connection=socket.create_connection(('127.0.0.1',6379),timeout=5)
connection.sendall(('AUTH '+os.environ['COVE_PROBE_PASSWORD']+'\\r\\n').encode())
assert connection.recv(1024).startswith(b'+OK')
connection.sendall(b'PING\\r\\n')
assert connection.recv(1024).startswith(b'+PONG')
'''
                healthy = False
                while time.monotonic() < deadline:
                    probe = run(['docker', 'run', '--rm', '--pull', 'never', '--network', 'container:' + rows[0]['Id'], '--entrypoint', 'python', '-e', 'COVE_PROBE_PASSWORD', client_image, '-c', program], env=environment | {'COVE_PROBE_PASSWORD': supplied['CACHE_PASSWORD']}, timeout=15)
                    if probe.returncode == 0:
                        healthy = True
                        break
                    time.sleep(2)
            if healthy and template['web']:
                web = template['web']
                matching = [row for row in rows if row['Config']['Labels'].get('com.docker.compose.service') == web['service']]
                if not matching:
                    healthy = False
                else:
                    probe_path = {'prometheus': '/-/ready', 'loki': '/ready', 'otel-collector': '/', 'grafana': '/api/health', 'jellyfin': '/health', 'umami': '/api/heartbeat', 'n8n': '/healthz/readiness', 'garage': '/health', 'seaweedfs': '/', 'keycloak': '/realms/master', 'pocketbase': '/api/health'}.get(template['id'], '/')
                    username = 'app'
                    password = supplied.get('WEB_PASSWORD', '')
                    authorization = base64.b64encode((username + ':' + password).encode()).decode() if password else ''
                    probe_environment = environment | {'COVE_PROBE_AUTH': authorization}
                    program = '''import os, urllib.request, urllib.error, sys
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl): return None
request = urllib.request.Request(%r, headers={'Host':'app.example.test'})
auth = os.environ.get('COVE_PROBE_AUTH')
if auth: request.add_header('Authorization', 'Basic '+auth)
try:
    response = urllib.request.build_opener(NoRedirect).open(request, timeout=5)
    code = response.status
except urllib.error.HTTPError as error:
    code = error.code
print(code)
sys.exit(0 if 200 <= code < 400 or (%r and code in (401,403)) else 1)
''' % ('http://127.0.0.1:%d%s' % (web['port'],probe_path), template['id'] in ('garage','seaweedfs'))
                    healthy = False
                    while time.monotonic() < deadline:
                        probe = run(['docker', 'run', '--rm', '--pull', 'never', '--network', 'container:' + matching[0]['Id'], '--entrypoint', 'python', '-e', 'COVE_PROBE_AUTH', client_image, '-c', program], env=probe_environment, timeout=15)
                        if probe.returncode == 0:
                            healthy = True
                            break
                        time.sleep(2)
                    if not healthy:
                        record['message'] = 'HTTP readiness did not pass within the bounded timeout.'
            if healthy:
                record['runtime'] = 'started_healthy'
                record['message'] = 'Started on an internal network; all services running, inherited/declared health checks passed' + (' and HTTP readiness verified.' if template['web'] else ' and authenticated RESP PING verified.' if template['id'] == 'dragonfly' else '.')
                record['services'] = {row['Config']['Labels']['com.docker.compose.service']: row['State'].get('Health', {}).get('Status', 'running') for row in rows}
            else:
                if record['message'] == 'Runtime not requested.':
                    record['message'] = 'One or more services exited or did not become healthy within the bounded timeout.'
        except subprocess.TimeoutExpired:
            record['message'] = 'Runtime acceptance exceeded its bounded timeout.'
        finally:
            cleanup = run([*command, 'down', '--volumes', '--remove-orphans', '--timeout', '10'], env=environment, timeout=90)
            if cleanup.returncode:
                raise RuntimeError('Owned fixture cleanup failed for ' + project)
            for key, value in supplied.items():
                if len(value) > 8 and any(variable['key'] == key and variable['type'] == 'secret' for variable in template['variables']):
                    record['message'] = record['message'].replace(value, '[redacted]')
    return record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--start', nargs='*', default=[])
    parser.add_argument('--timeout', type=int, default=180)
    parser.add_argument('--client-image', default='python:3.12-alpine')
    parser.add_argument('--output', type=Path, default=EVIDENCE)
    args = parser.parse_args()
    if args.timeout < 10 or args.timeout > 600:
        parser.error('Use a timeout between 10 and 600 seconds')
    schema = run(['go', 'test', './internal/catalog', '-run', 'TestEveryBuiltinTemplate', '-count=1'], cwd=ROOT)
    if schema.returncode:
        raise SystemExit('Catalogue schema validation failed: '+schema.stdout+schema.stderr)
    data = json.loads(CATALOGUE.read_text(encoding='utf-8'))
    evidence = json.loads(args.output.read_text(encoding='utf-8')) if args.output.exists() else {}
    selected = set(args.start)
    failure = False
    for template in data['templates']:
        previous = evidence.get(template['id'])
        digest = hashlib.sha256(template['compose'].encode()).hexdigest()
        start = 'all' in selected or template['id'] in selected
        record = check_template(template, start, args.timeout, args.client_image)
        if record['compose_config'] and not start and previous and previous.get('compose_sha256') == digest and previous.get('runtime') in ('started_healthy', 'config_only') and '\n' not in previous.get('message', ''):
            record = previous
        evidence[template['id']] = record
        failure |= not record['compose_config']
        temporary = args.output.with_suffix('.tmp')
        temporary.write_text(json.dumps(evidence, indent=2) + '\n', encoding='utf-8')
        for attempt in range(40):
            try:
                temporary.replace(args.output)
                break
            except PermissionError:
                if attempt == 39:
                    raise
                time.sleep(0.1)
        print(template['id'], 'config=' + str(record['compose_config']), record['runtime'], flush=True)
    raise SystemExit(1 if failure else 0)


if __name__ == '__main__':
    main()
