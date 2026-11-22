"""Real Docker rejection and replacement checks in the dedicated acceptance VM."""
import copy
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time


def main():
    assert os.geteuid() == 0 and Path('/etc/hostname').read_text().strip() == 'cove-acceptance'
    name = 'guard-' + secrets.token_hex(6)
    network, volume = name + '-db', name + '-data'
    root = Path(tempfile.mkdtemp(prefix='cove-guard-'))
    log = (root / 'acceptance.log').open('wb')

    def docker(*args):
        return subprocess.check_output(['docker', *args], stderr=log).decode().strip()

    def rpc(body):
        conn = http.client.HTTPConnection('localhost', timeout=30)
        conn.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.sock.settimeout(30)
        conn.sock.connect('/run/cove/worker.sock')
        try:
            conn.request('POST', '/rpc', json.dumps(body), {'Content-Type': 'application/json'})
            result = json.load(conn.getresponse())
            assert not result.get('error'), result
            return result
        finally:
            conn.close()

    def deploy(compose, hooks=None):
        request = {'id': secrets.token_hex(16), 'workload': name, 'action': 'deploy', 'compose': json.dumps(compose), 'kind': 'service'}
        if hooks:
            request['hooks'] = hooks
        rpc({'method': 'submit', 'request': request})
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            result = rpc({'method': 'status', 'id': request['id']})['operation']
            if result['state'] not in ('running', 'queued'):
                return result
            time.sleep(.25)
        raise RuntimeError('guarded fixture timed out')

    def state(service):
        ids = docker('ps', '-aq', '--filter', 'label=com.docker.compose.project=cove-' + name, '--filter', 'label=com.docker.compose.service=' + service).split()
        assert len(ids) == 1
        c = json.loads(docker('inspect', ids[0]))[0]
        return c['Id'], c['State']['StartedAt'], c['State']['Running']

    worker = None
    try:
        docker('network', 'create', '--internal', '--label', 'cove.guard.fixture=' + name, network)
        docker('volume', 'create', '--label', 'cove.guard.fixture=' + name, volume)
        image = docker('image', 'inspect', '--format', '{{.Id}}', 'busybox:1.37')
        compose = {'services': {
            'database': {'image': image, 'command': ['sleep', '3600'], 'healthcheck': {'test': ['CMD', 'true'], 'interval': '1s'}, 'networks': ['db'], 'volumes': [{'type': 'volume', 'source': 'data', 'target': '/data'}]},
            'app': {'image': image, 'command': ['sleep', '3600'], 'network_mode': 'none'},
            'receipt': {'image': image, 'profiles': ['checks'], 'network_mode': 'none'},
            'compatibility': {'image': image, 'profiles': ['checks'], 'networks': ['db']},
        }, 'networks': {'db': {'name': network, 'external': True}}, 'volumes': {'data': {'name': volume, 'external': True}}}
        subprocess.run(['systemctl', 'stop', 'cove-worker'], check=True)
        worker = subprocess.Popen([str(Path(__file__).parent / 'cove-worker-linux-amd64'), 'daemon'], stdout=log, stderr=log)
        for _ in range(60):
            try:
                assert rpc({'method': 'probe'})['probe']['guarded_hooks']
                break
            except (OSError, AssertionError):
                time.sleep(.25)
        else:
            raise RuntimeError('fixture worker unavailable')
        assert deploy(compose)['state'] == 'succeeded'
        old_app, old_db = state('app'), state('database')
        hooks = {'assertions': {
            'volumes': [{'name': volume, 'labels': {'cove.guard.fixture': name}}],
            'networks': [{'name': network, 'internal': True, 'labels': {'cove.guard.fixture': name}}],
            'services': [{'service': 'database', 'image': image, 'networks': [network], 'mounts': [{'volume': volume, 'target': '/data', 'read_only': False}]}],
        }, 'before_steps': [{'service': 'receipt', 'command': 'test ! -d /sys/class/net/eth0'}, {'service': 'compatibility', 'command': 'test -d /sys/class/net/eth0'}]}
        candidate = copy.deepcopy(compose)
        candidate['services']['app']['command'] = ['sleep', '3601']
        for failure in ('volume', 'network', 'image', 'mount', 'recipe', 'compatibility', 'planned-mount'):
            check, proposed = copy.deepcopy(hooks), copy.deepcopy(candidate)
            if failure == 'volume': check['assertions']['volumes'][0]['labels']['cove.guard.fixture'] = 'foreign'
            if failure == 'network': check['assertions']['networks'][0]['internal'] = False
            if failure == 'image': check['assertions']['services'][0]['image'] = 'sha256:' + '0' * 64
            if failure == 'mount': check['assertions']['services'][0]['mounts'][0]['read_only'] = True
            if failure == 'recipe': check['before_steps'][0]['command'] = 'exit 23'
            if failure == 'compatibility': check['before_steps'][1]['command'] = 'exit 24'
            if failure == 'planned-mount': proposed['services']['database']['volumes'][0]['read_only'] = True
            assert deploy(proposed, check)['state'] == 'failed', failure
            assert state('app') == old_app and state('database') == old_db, failure
            print('PASS rejected ' + failure + ' without container replacement', flush=True)
        assert deploy(candidate, hooks)['state'] == 'succeeded'
        assert state('app')[0] != old_app[0] and state('database') == old_db
        assert not docker('ps', '-aq', '--filter', 'name=cove-hook-')
        print('PASS ordered isolated checks; application replaced; database unchanged', flush=True)
    finally:
        if worker:
            worker.terminate()
            worker.wait(timeout=30)
        ids = docker('ps', '-aq', '--filter', 'label=com.docker.compose.project=cove-' + name).split()
        if ids: docker('rm', '-f', '-v', *ids)
        docker('network', 'rm', network)
        docker('volume', 'rm', volume)
        subprocess.run(['systemctl', 'start', 'cove-worker'], check=True)
        log.close()


if __name__ == '__main__':
    main()
