from pathlib import Path
import os
import argparse
import secrets
import subprocess
import tempfile
import time
parser = argparse.ArgumentParser()
parser.add_argument("pattern", nargs="?", default="^TestCloudflareTunnelLifecycleDocker$")
selection = parser.parse_args().pattern
root = Path(__file__).resolve().parent.parent
scratch = Path(tempfile.mkdtemp(prefix='cove-tunnel-replay-'))
owner = 'cove-tunnel-replay-' + secrets.token_hex(6)
network = owner + '-net'
daemon = owner + '-daemon'
client = owner + '-client'
label = 'cove.tunnel-isolation=' + owner
root_volume = owner + '-root'
volume_created = False
connector_image = 'docker.io/cloudflare/cloudflared:2026.11.1@sha256:e613aadbc2d9511785d2e67a86b0c8ac18dd63a3f9af0d06faf04cbfdcb35f2d'
made = []
net = False

def run(*args, **kw):
    return subprocess.run(args, cwd=root, check=True, **kw)
try:
    env = os.environ | {'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}
    run('go', '-C', 'test/ingress-client', 'build', '-o', str(scratch / 'ingress-client'), '.', env=env)
    run('go', 'test', '-c', '-o', str(scratch / 'worker.test'), './internal/worker', env=env)
    run('docker', 'image', 'inspect', 'docker:29-dind', 'docker:29-cli', 'traefik:v3.7.19', 'busybox:1.37', stdout=subprocess.DEVNULL)
    run('docker', 'save', '-o', str(scratch / 'images.tar'), 'traefik:v3.7.19', 'busybox:1.37')
    run('docker', 'network', 'create', '--internal', '--label', label, network, stdout=subprocess.DEVNULL)
    net = True
    run('docker','image','inspect',connector_image,stdout=subprocess.DEVNULL)
    run('docker','volume','create','--label',label,root_volume,stdout=subprocess.DEVNULL)
    volume_created=True
    made.append(daemon)
    run('docker', 'run', '-d', '--name', daemon, '--privileged', '--sysctl', 'net.ipv6.conf.all.disable_ipv6=0', '--sysctl', 'net.ipv6.conf.default.disable_ipv6=0', '--label', label, '--network', network, '--network-alias', 'daemon', '--mount', 'type=volume,source='+root_volume+',target=/fixture-root', '-e', 'DOCKER_TLS_CERTDIR=', 'docker:29-dind', '--tls=false', '--label', 'cove.test-tunnel-recovery=' + owner, stdout=subprocess.DEVNULL)
    for attempt in range(60):
        if subprocess.run(['docker', 'exec', daemon, 'docker', 'info'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
            break
        time.sleep(1)
    else:
        raise RuntimeError('owned isolated Docker daemon did not become ready')
    with (scratch / 'images.tar').open('rb') as images:
        run('docker', 'exec', '-i', daemon, 'docker', 'load', stdin=images, stdout=subprocess.DEVNULL)
    made.append(client)
    with (scratch / 'result.log').open('wb') as out:
        result = subprocess.run(('docker', 'run', '--rm', '--name', client, '--label', label, '--network', network, '-e', 'DOCKER_HOST=tcp://daemon:2375', '-e', 'DOCKER_TLS_CERTDIR=', '-e', 'COVE_TEST_CLOUDFLARE_TUNNEL_DOCKER=1', '--mount', 'type=volume,source='+root_volume+',target=/fixture-root', '-e', 'COVE_TEST_PROXY_SNAPSHOT_DOCKER=1', '-e', 'COVE_TEST_PROXY_SNAPSHOT_REPLAY=1', '-e', 'COVE_TEST_PROXY_RECOVERY_DOCKER=1', '-e', 'COVE_TEST_HTTP3_CLIENT=/tests/ingress-client', '--mount', 'type=bind,source=' + str(scratch) + ',target=/tests,readonly', 'docker:29-cli', '/tests/worker.test', '-test.v', '-test.run', selection, '-test.timeout', '5m'), cwd=root, stdout=out, stderr=subprocess.STDOUT)
    print((scratch / 'result.log').read_text(errors='replace'))
    print('Artifact:', scratch)
    if result.returncode:
        raise RuntimeError('isolated Tunnel lifecycle acceptance failed; see the artifact above')
    checker=owner+'-offline-config'
    made.append(checker)
    run('docker','run','--rm','--pull','never','--name',checker,'--label',label,'--network','none','--user','0:0','--read-only','--cap-drop','ALL','--security-opt','no-new-privileges:true','--mount','type=volume,source='+root_volume+',target=/config,volume-subpath=offline-connector,readonly',connector_image,'tunnel','--config','/config/config.json','ingress','validate')
    print('Official pinned cloudflared offline configuration validated')
finally:
    failures = []
    for name in reversed(made):
        existing = subprocess.run(['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$'], capture_output=True, text=True, check=True).stdout.strip()
        if not existing:
            continue
        actual = subprocess.run(['docker', 'inspect', '--format', '{{index .Config.Labels "cove.tunnel-isolation"}}', name], capture_output=True, text=True, check=True).stdout.strip()
        if actual != owner:
            failures.append('ownership mismatch ' + name)
            continue
        if subprocess.run(['docker', 'rm', '-f', '-v', name], stdout=subprocess.DEVNULL).returncode:
            failures.append('cleanup failed ' + name)
    if net:
        actual = subprocess.run(['docker', 'network', 'inspect', '--format', '{{index .Labels "cove.tunnel-isolation"}}', network], capture_output=True, text=True, check=True).stdout.strip()
        if actual != owner:
            failures.append('network ownership mismatch')
        elif subprocess.run(['docker', 'network', 'rm', network], stdout=subprocess.DEVNULL).returncode:
            failures.append('network cleanup failed')
    if volume_created:
        actual = subprocess.run(['docker','volume','inspect','--format','{{index .Labels "cove.tunnel-isolation"}}',root_volume],capture_output=True,text=True,check=True).stdout.strip()
        if actual != owner:
            failures.append('volume ownership mismatch')
        elif subprocess.run(['docker','volume','rm',root_volume],stdout=subprocess.DEVNULL).returncode:
            failures.append('volume cleanup failed')
    if failures:
        raise RuntimeError('; '.join(failures))
