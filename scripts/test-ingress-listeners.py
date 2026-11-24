from pathlib import Path
import os
import importlib.util
import argparse
import secrets
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("pattern", nargs="?", default="^TestIngress(Proxy|Exposure)")
    selection = parser.parse_args().pattern
    root = Path(__file__).resolve().parent.parent
    scratch = Path(tempfile.mkdtemp(prefix='cove-proxy-replay-'))
    owner = 'cove-proxy-replay-' + secrets.token_hex(6)
    network = owner + '-net'
    daemon = owner + '-daemon'
    client = owner + '-client'
    label = 'cove.proxy-isolation=' + owner
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
        made.append(daemon)
        run('docker', 'run', '-d', '--name', daemon, '--privileged', '--sysctl', 'net.ipv6.conf.all.disable_ipv6=0', '--sysctl', 'net.ipv6.conf.default.disable_ipv6=0', '--label', label, '--network', network, '--network-alias', 'daemon', '-e', 'DOCKER_TLS_CERTDIR=', 'docker:29-dind', '--tls=false', '--label', 'cove.test-proxy-recovery=' + owner, stdout=subprocess.DEVNULL)
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
            result = subprocess.run(('docker', 'run', '--rm', '--name', client, '--label', label, '--network', network, '-e', 'DOCKER_HOST=tcp://daemon:2375', '-e', 'DOCKER_TLS_CERTDIR=', '-e', 'COVE_TEST_PROXY_SNAPSHOT_DOCKER=1', '-e', 'COVE_TEST_PROXY_SNAPSHOT_REPLAY=1', '-e', 'COVE_TEST_PROXY_RECOVERY_DOCKER=1', '-e', 'COVE_TEST_HTTP3_CLIENT=/tests/ingress-client', '--mount', 'type=bind,source=' + str(scratch) + ',target=/tests,readonly', 'docker:29-cli', '/tests/worker.test', '-test.v', '-test.run', selection, '-test.timeout', '4m'), cwd=root, stdout=out, stderr=subprocess.STDOUT)
        print((scratch / 'result.log').read_text(errors='replace'))
        print('Artifact:', scratch)
        if result.returncode:
            raise RuntimeError('isolated listener acceptance failed; see the artifact above')
    finally:
        failures = []
        for name in reversed(made):
            existing = subprocess.run(['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$'], capture_output=True, text=True, check=True).stdout.strip()
            if not existing:
                continue
            actual = subprocess.run(['docker', 'inspect', '--format', '{{index .Config.Labels "cove.proxy-isolation"}}', name], capture_output=True, text=True, check=True).stdout.strip()
            if actual != owner:
                failures.append('ownership mismatch ' + name)
                continue
            if subprocess.run(['docker', 'rm', '-f', '-v', name], stdout=subprocess.DEVNULL).returncode:
                failures.append('cleanup failed ' + name)
        if net:
            actual = subprocess.run(['docker', 'network', 'inspect', '--format', '{{index .Labels "cove.proxy-isolation"}}', network], capture_output=True, text=True, check=True).stdout.strip()
            if actual != owner:
                failures.append('network ownership mismatch')
            elif subprocess.run(['docker', 'network', 'rm', network], stdout=subprocess.DEVNULL).returncode:
                failures.append('network cleanup failed')
        if failures:
            raise RuntimeError('; '.join(failures))


if __name__ == "__main__":
    spec = importlib.util.spec_from_file_location("cove_shared_browser_harness", Path(__file__).resolve().parent / "test-browser.py")
    harness = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(harness)
    with harness.browser_fixture_lock():
        main()
