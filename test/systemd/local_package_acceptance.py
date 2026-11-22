"""Verify the shipped Compose main-node worker and shared ingress in the test VM."""
import base64
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.request


def main():
    if os.geteuid()!=0 or Path('/etc/hostname').read_text().strip()!='cove-acceptance':
        raise SystemExit('This test requires the dedicated cove-acceptance VM.')
    os.umask(0o077)
    root=Path(tempfile.mkdtemp(prefix='cove-package-'))
    log=(root/'acceptance.log').open('wb')
    source=Path(__file__).parent
    for name in ['compose.yaml','compose.local.yaml']:
        shutil.copyfile(source/name,root/name)
    (root/'deploy').mkdir()
    for name in ['traefik.yaml','traefik-panel.yaml']:
        shutil.copyfile(source/name,root/'deploy'/name)
    (root/'images.yaml').write_text('services:\n  cove:\n    image: cove:development\n  worker:\n    image: cove-worker:acceptance\n')
    env=os.environ | {'POSTGRES_PASSWORD':secrets.token_hex(24),'COVE_ADMIN_PASSWORD':secrets.token_hex(24),
        'COVE_ADMIN_TOKEN':secrets.token_hex(32),'COVE_ENCRYPTION_KEY':base64.b64encode(secrets.token_bytes(32)).decode(),'COVE_DOMAIN':'panel.localhost'}
    compose=['docker','compose','-p','cove-package-acceptance','-f',str(root/'compose.yaml'),'-f',str(root/'compose.local.yaml'),'-f',str(root/'images.yaml')]

    def run(*args,**kwargs):
        return subprocess.run(args,check=True,env=env,stdout=log,stderr=log,**kwargs)

    for resource in ['container','network','volume']:
        if subprocess.run(['docker',resource,'inspect','cove-ingress'],capture_output=True).returncode==0:
            raise SystemExit('Refusing to replace existing cove-ingress '+resource)
    run('systemctl','stop','cove-worker')
    native=Path('/var/lib/cove')
    if native.is_symlink(): raise SystemExit('Unexpected worker state symlink')
    native.rename(root/'native-state')
    native.mkdir(mode=0o700)
    workload=None
    original_dns=socket.getaddrinfo
    socket.getaddrinfo=lambda host,*args,**kwargs: original_dns('127.0.0.1' if host in ('panel.localhost','app.localhost') else host,*args,**kwargs)
    try:
        cert=root/'cert.pem'
        run('openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','2',
            '-keyout',str(root/'key.pem'),'-out',str(cert),'-subj','/CN=panel.localhost',
            '-addext','subjectAltName=DNS:panel.localhost,DNS:app.localhost')
        with (root/'deploy/traefik-panel.yaml').open('a') as config:
            config.write('\ntls:\n  certificates:\n    - certFile: /fixture/cert.pem\n      keyFile: /fixture/key.pem\n')
        with (root/'images.yaml').open('a') as config:
            config.write('  proxy:\n    volumes:\n      - '+str(root)+':/fixture:ro\n')
        run(*compose,'up','-d','--no-build')
        trust=ssl.create_default_context(cafile=str(cert))

        def api(method,path,body=None):
            request=urllib.request.Request('https://panel.localhost'+path,method=method,
                data=json.dumps(body).encode() if body is not None else None,
                headers={'Authorization':'Bearer '+env['COVE_ADMIN_TOKEN'],'Content-Type':'application/json'})
            with urllib.request.urlopen(request,context=trust,timeout=60) as response: return json.load(response)

        for _ in range(90):
            try: node=api('POST','/api/nodes/local')['id'];break
            except OSError: time.sleep(1)
        else: raise RuntimeError('Packaged local worker did not become ready')
        print('PASS: unprivileged dashboard reaches the main worker through its protected Unix socket',flush=True)
        definition={'name':'Main-node fixture','node_id':node,'kind':'service',
            'compose':'services:\n  web:\n    image: busybox:1.37\n    command: [sh, -c, "mkdir -p /www; echo main-worker-ok > /www/index.html; httpd -f -p 8080 -h /www"]\n',
            'routes':[{'domain':'app.localhost','service':'web','port':8080}]}
        project=api('POST','/api/projects',{'name':'Package acceptance'})['id']
        workload=api('POST','/api/workloads',definition | {'project_id':project})['id']
        operation=api('POST','/api/workloads/'+workload+'/operations',{'action':'deploy'})['id']
        for _ in range(120):
            state=api('GET','/api/operations/'+operation)['state']
            if state not in ('queued','running'):
                if state!='succeeded': raise RuntimeError(api('GET','/api/operations/'+operation+'/logs'))
                break
            time.sleep(1)
        else: raise RuntimeError('Packaged deployment timed out')

        def app():
            with urllib.request.urlopen('https://app.localhost',context=trust,timeout=20) as response: assert response.read().strip()==b'main-worker-ok'

        for _ in range(60):
            try: app();break
            except OSError: time.sleep(1)
        else: raise RuntimeError('Application TLS route unavailable')
        api('GET','/api/nodes')
        print('PASS: dashboard and local application share managed HTTPS ingress',flush=True)
        started=subprocess.check_output(['docker','inspect','--format','{{.State.StartedAt}}','cove-ingress'])
        application=subprocess.check_output(['docker','ps','-q','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
        identity=subprocess.check_output(['docker','inspect','--format','{{.Id}} {{.State.StartedAt}}',*application])
        for routes in ([],definition['routes']):
            changed=api('PUT','/api/workloads/'+workload+'/routes',{'routes':routes})['id']
            for _ in range(120):
                state=api('GET','/api/operations/'+changed)['state']
                if state not in ('queued','running'):
                    if state!='succeeded': raise RuntimeError(api('GET','/api/operations/'+changed+'/logs'))
                    break
                time.sleep(1)
            else: raise RuntimeError('Route update timed out')
            api('GET','/api/nodes')
            if routes: app()
            else:
                try: urllib.request.urlopen('https://app.localhost',context=trust,timeout=20)
                except urllib.error.HTTPError as error: assert error.code==404
                else: raise RuntimeError('Removed route is still serving')
        assert subprocess.check_output(['docker','inspect','--format','{{.State.StartedAt}}','cove-ingress'])==started
        assert subprocess.check_output(['docker','inspect','--format','{{.Id}} {{.State.StartedAt}}',*application])==identity
        print('PASS: live route removal and restoration keep the application and ingress running without restarts',flush=True)

        run(*compose,'stop','cove');app()
        run(*compose,'start','cove')
        for _ in range(60):
            try: api('GET','/api/nodes');break
            except OSError: time.sleep(1)
        run(*compose,'restart','worker');app()
        print('PASS: control-plane outage and local worker restart leave the application serving',flush=True)
    finally:
        socket.getaddrinfo=original_dns
        run(*compose,'stop','worker')
        if workload:
            ids=subprocess.check_output(['docker','ps','-aq','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
            if ids: run('docker','rm','-f',*ids)
            networks=subprocess.check_output(['docker','network','ls','-q','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
            if networks: run('docker','network','rm',*networks)
        run(*compose,'logs','--no-color')
        run(*compose,'down','--volumes')
        if native.resolve()!=Path('/var/lib/cove') or native.is_symlink(): raise RuntimeError('Unexpected fixture state path')
        shutil.rmtree(native)
        (root/'native-state').rename(native)
        run('chown','cove:cove','/run/cove')
        run('systemctl','start','cove-worker')
        log.close()
        print('Acceptance log: '+str(root/'acceptance.log'),flush=True)


if __name__=='__main__': main()
