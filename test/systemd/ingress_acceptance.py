"""Exercise managed ingress against a validating local ACME CA in the disposable VM."""
import http.client
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.request


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    if os.geteuid() != 0 or Path('/etc/hostname').read_text().strip() != 'cove-acceptance':
        raise SystemExit('This test requires the dedicated cove-acceptance VM.')
    os.umask(0o077)
    root = Path(tempfile.mkdtemp(prefix='cove-acme-'))
    log = (root / 'acceptance.log').open('wb')
    names = ['cove-ingress', 'cove-pebble']
    for name in names:
        if subprocess.check_output(['docker', 'ps', '-aq', '--filter', 'name=^/'+name+'$']).strip():
            raise SystemExit('Refusing to replace an existing test resource: '+name)
    run('systemctl', 'stop', 'cove-worker')
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2',
        '-keyout', str(root/'key.pem'), '-out', str(root/'ca.pem'), '-subj', '/CN=cove-pebble',
        '-addext', 'subjectAltName=DNS:cove-pebble,DNS:localhost', stdout=log, stderr=log)
    (root/'pebble.json').write_text(json.dumps({'pebble': {
        'listenAddress':'0.0.0.0:14000', 'managementListenAddress':'0.0.0.0:15000',
        'certificate':'/fixture/ca.pem', 'privateKey':'/fixture/key.pem',
        'httpPort':80, 'tlsPort':443,
        'profiles':{'default':{'description':'Acceptance test', 'validityPeriod':300}}
    }}))
    # Pebble runs unprivileged; only this temporary fixture contains this test key.
    root.chmod(0o755)
    for path in [root/'ca.pem', root/'key.pem', root/'pebble.json']:
        path.chmod(0o644)
    worker = None
    workload = 'acme-acceptance'
    try:
        run('docker', 'network', 'create', '--label', 'cove.managed=ingress', 'cove-ingress', stdout=log)
        run('docker', 'volume', 'create', '--label', 'cove.managed=ingress', 'cove-ingress', stdout=log)
        run('docker', 'create', '--name', 'cove-ingress', '--label', 'cove.managed=ingress',
            '--restart', 'unless-stopped', '--network', 'cove-ingress', '--network-alias', 'app.cove.test',
            '-p', '80:80', '-p', '443:443', '-p', '443:443/udp', '-v', 'cove-ingress:/data',
            '--entrypoint', 'sh', 'traefik:v3.7.19', '-ec',
            'mkdir -p /data/dynamic; exec traefik '
            '--entrypoints.web.address=:80 --entrypoints.web.http.redirections.entrypoint.to=websecure '
            '--entrypoints.web.http.redirections.entrypoint.scheme=https --entrypoints.websecure.address=:443 '
            '--entrypoints.traefik.address=127.0.0.1:8080 --api.insecure=true --api.dashboard=false '
            '--providers.file.directory=/data/dynamic --providers.file.watch=true '
            '--certificatesresolvers.cove.acme.storage=/data/traefik-acme.json '
            '--certificatesresolvers.cove.acme.httpchallenge.entrypoint=web '
            '--certificatesresolvers.cove.acme.caserver=https://cove-pebble:14000/dir '
            '--certificatesresolvers.cove.acme.cacertificates=/data/acme-root.pem '
            '--certificatesresolvers.cove.acme.certificatesduration=1', stdout=log, stderr=log)
        run('docker', 'run', '-d', '--name', 'cove-pebble', '--network', 'cove-ingress',
            '-p', '127.0.0.1:15000:15000', '-e', 'PEBBLE_VA_NOSLEEP=1',
            '-e', 'PEBBLE_AUTHZREUSE=0', '-v', str(root)+':/fixture:ro',
            'ghcr.io/letsencrypt/pebble:latest', '-config', '/fixture/pebble.json',
            '-dnsserver', '127.0.0.11:53', stdout=log, stderr=log)
        worker = subprocess.Popen([str(Path(__file__).parent/'cove-worker-linux-amd64'), 'daemon'],
            env=os.environ | {'COVE_ACME_DIRECTORY':'https://cove-pebble:14000/dir',
                              'COVE_ACME_ROOT':str(root/'ca.pem')}, stdout=log, stderr=log)

        def rpc(body):
            conn = http.client.HTTPConnection('localhost', timeout=60)
            conn.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            conn.sock.settimeout(60)
            conn.sock.connect('/run/cove/worker.sock')
            conn.request('POST', '/rpc', json.dumps(body), {'Content-Type':'application/json'})
            response = conn.getresponse()
            result = json.load(response)
            conn.close()
            if result.get('error'):
                raise RuntimeError(result['error'])
            return result

        for _ in range(60):
            try:
                rpc({'method':'probe'})
                break
            except (OSError, RuntimeError):
                time.sleep(1)
        request = {'id':'acme-'+str(time.time_ns()), 'workload':workload, 'action':'deploy', 'kind':'service',
            'compose':'services:\n  web:\n    image: busybox:1.37\n    command: [sh, -c, "mkdir -p /www; echo cove-acme-ok > /www/index.html; httpd -f -p 8080 -h /www"]\n',
            'routes':[{'domain':'app.cove.test','service':'web','port':8080}]}
        rpc({'method':'submit', 'request':request})
        for _ in range(180):
            op = rpc({'method':'status','id':request['id']})['operation']
            if op['state'] not in ('queued','running'):
                if op['state'] != 'succeeded':
                    raise RuntimeError(rpc({'method':'logs','id':request['id']}))
                break
            time.sleep(1)
        else:
            raise RuntimeError('Deployment timed out')
        management = ssl.create_default_context(cafile=str(root/'ca.pem'))
        with urllib.request.urlopen('https://localhost:15000/roots/0', context=management) as response:
            (root/'issued-root.pem').write_bytes(response.read())
        trust = ssl.create_default_context(cafile=str(root/'issued-root.pem'))

        def https():
            with socket.create_connection(('127.0.0.1',443), timeout=10) as tcp:
                with trust.wrap_socket(tcp, server_hostname='app.cove.test') as tls:
                    serial = tls.getpeercert()['serialNumber']
                    tls.sendall(b'GET / HTTP/1.1\r\nHost: app.cove.test\r\nConnection: close\r\n\r\n')
                    chunks = []
                    while chunk := tls.recv(65536):
                        chunks.append(chunk)
                    assert b'cove-acme-ok' in b''.join(chunks), chunks
                    return serial

        def wait_https(previous=None, timeout=240):
            deadline = time.monotonic()+timeout
            last = None
            while time.monotonic() < deadline:
                try:
                    serial = https()
                    if previous is None or serial != previous:
                        return serial
                except (OSError, AssertionError) as error:
                    last = error
                time.sleep(2)
            raise RuntimeError('Certificate issuance/renewal timed out: '+str(last))

        first = wait_https()
        print('PASS: actual ACME challenge validation, trusted certificate and application HTTPS', flush=True)
        conn = http.client.HTTPConnection('127.0.0.1',80)
        conn.request('GET','/',headers={'Host':'app.cove.test'})
        response = conn.getresponse()
        assert response.status in (301, 308) and response.getheader('Location') == 'https://app.cove.test/'
        conn.close()
        print('PASS: HTTP redirects to HTTPS', flush=True)
        run('docker','pause','cove-pebble',stdout=log)
        run('docker','restart','cove-ingress',stdout=log)
        assert wait_https() == first
        print('PASS: proxy restart preserves certificate and routing', flush=True)
        run('docker','unpause','cove-pebble',stdout=log)
        # A short CA lifetime exercises Traefik renewal rather than deleting its cache.
        renewed = wait_https(previous=first, timeout=720)
        assert renewed != first
        print('PASS: automatic renewal replaces the certificate', flush=True)
        run('docker','stop','cove-pebble',stdout=log)
        assert https() == renewed
        print('PASS: CA outage does not interrupt an already issued certificate', flush=True)
    finally:
        if worker:
            worker.terminate()
            worker.wait(timeout=40)
        for name in names:
            subprocess.run(['docker','logs',name],stdout=log,stderr=log)
            subprocess.run(['docker','rm','-f',name],stdout=log,stderr=log)
        ids = subprocess.check_output(['docker','ps','-aq','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
        if ids:
            run('docker','rm','-f',*ids,stdout=log)
        run('docker','network','rm','cove-ingress',stdout=log)
        run('docker','volume','rm','cove-ingress',stdout=log)
        # Remove only this test's desired routes so the standard worker can restart.
        ingress = Path('/var/lib/cove/ingress.json')
        if ingress.exists():
            routes = json.loads(ingress.read_text())
            routes.pop(workload,None)
            ingress.write_text(json.dumps(routes))
        run('chown','-R','cove:cove','/var/lib/cove')
        run('chown','cove:cove','/run/cove')
        run('systemctl','start','cove-worker')
        log.close()
        print('Acceptance log: '+str(root/'acceptance.log'),flush=True)


if __name__ == '__main__':
    main()
