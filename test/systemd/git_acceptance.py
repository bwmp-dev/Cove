"""Private HTTPS Git credentials, pinned commits, builds and rollback in the test VM."""
import base64
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlsplit


def main():
    if os.geteuid() != 0 or Path('/etc/hostname').read_text().strip() != 'cove-acceptance':
        raise SystemExit('This test requires the dedicated cove-acceptance VM.')
    os.umask(0o077)
    root = Path(tempfile.mkdtemp(prefix='cove-git-'))
    log = (root/'acceptance.log').open('wb')

    def run(*args, **kwargs):
        return subprocess.run(args,check=True,stdout=log,stderr=log,**kwargs)

    repo = root/'repo'
    repo.mkdir()
    run('git','init','--initial-branch=main',str(repo))
    run('git','-C',str(repo),'config','user.name','Cove acceptance fixture')
    run('git','-C',str(repo),'config','user.email','fixture@example.invalid')
    (repo/'compose.yaml').write_text('services:\n  web:\n    build: .\n')
    (repo/'Dockerfile').write_text('FROM busybox:1.37\nCOPY message /www/index.html\nUSER 65534:65534\nCMD ["httpd","-f","-p","8080","-h","/www"]\n')
    (repo/'message').write_text('first-commit\n')
    run('git','-C',str(repo),'add','.')
    run('git','-C',str(repo),'commit','-m','First test fixture')
    first = subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD']).decode().strip()
    (repo/'message').write_text('second-commit\n')
    run('git','-C',str(repo),'commit','-am','Second test fixture')
    run('git','clone','--bare',str(repo),str(root/'private.git'))
    run('git','-C',str(root/'private.git'),'config','uploadpack.allowReachableSHA1InWant','true')
    run('openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','2','-keyout',str(root/'key.pem'),'-out',str(root/'ca.pem'),'-subj','/CN=localhost','-addext','subjectAltName=DNS:localhost')
    token = secrets.token_hex(32)
    expected = 'Basic '+base64.b64encode(('x-access-token:'+token).encode()).decode()
    git_backend = subprocess.check_output(['git','--exec-path']).decode().strip()+'/git-http-backend'

    class GitHandler(BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.1'
        def log_message(self, *args):
            pass

        def serve(self):
            if self.headers.get('Authorization') != expected:
                self.send_response(401)
                self.send_header('WWW-Authenticate','Basic realm="Cove fixture"')
                self.send_header('Content-Length','0')
                self.end_headers()
                return
            parsed = urlsplit(self.path)
            body = self.rfile.read(int(self.headers.get('Content-Length','0')))
            result = subprocess.run([git_backend],input=body,capture_output=True,check=True,env=os.environ | {
                'GIT_PROJECT_ROOT':str(root),'GIT_HTTP_EXPORT_ALL':'1','PATH_INFO':parsed.path,
                'QUERY_STRING':parsed.query,'REQUEST_METHOD':self.command,'REMOTE_USER':'fixture',
                'CONTENT_TYPE':self.headers.get('Content-Type',''),'CONTENT_LENGTH':str(len(body)),
            })
            headers,content = result.stdout.split(b'\r\n\r\n',1)
            values = [line.decode().split(':',1) for line in headers.split(b'\r\n')]
            status = next((int(value.strip().split()[0]) for key,value in values if key.lower()=='status'),200)
            self.send_response(status)
            for key,value in values:
                if key.lower()!='status': self.send_header(key,value.strip())
            self.send_header('Content-Length',str(len(content)))
            self.end_headers()
            self.wfile.write(content)

        do_GET = serve
        do_POST = serve

    server = ThreadingHTTPServer(('127.0.0.1',0),GitHandler)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(root/'ca.pem',root/'key.pem')
    server.socket = tls.wrap_socket(server.socket,server_side=True)
    threading.Thread(target=server.serve_forever,daemon=True).start()
    run('systemctl','stop','cove-worker')
    worker = subprocess.Popen([str(Path(__file__).parent/'cove-worker-linux-amd64'),'daemon'],env=os.environ | {'GIT_SSL_CAINFO':str(root/'ca.pem')},stdout=log,stderr=log)
    workload = 'git-'+secrets.token_hex(6)
    try:
        def rpc(body):
            conn = http.client.HTTPConnection('localhost',timeout=60)
            conn.sock = socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
            conn.sock.settimeout(60)
            conn.sock.connect('/run/cove/worker.sock')
            conn.request('POST','/rpc',json.dumps(body),{'Content-Type':'application/json'})
            result = json.load(conn.getresponse());conn.close()
            if result.get('error'): raise RuntimeError(result['error'])
            return result

        for _ in range(60):
            try: rpc({'method':'probe'});break
            except OSError: time.sleep(1)

        def operation(action,**fields):
            request = {'id':secrets.token_hex(16),'workload':workload,'kind':'service','action':action} | fields
            rpc({'method':'submit','request':request})
            for _ in range(300):
                op = rpc({'method':'status','id':request['id']})['operation']
                if op['state'] not in ('queued','running'):
                    output = rpc({'method':'logs','id':request['id']}).get('logs','')
                    assert token not in output
                    if op['state']!='succeeded': raise RuntimeError(output)
                    return op
                time.sleep(1)
            raise RuntimeError('Git deployment timed out')

        source = {'repository':f'https://localhost:{server.server_port}/private.git','branch':'main','compose_path':'compose.yaml','token':token,'revision':first}
        release = operation('deploy',source=source)
        assert release['revision']==first

        def content():
            container = rpc({'method':'containers','id':workload})['containers'][0]['id']
            return subprocess.check_output(['docker','exec',container,'cat','/www/index.html']).decode().strip()

        assert content()=='first-commit'
        print('PASS: authenticated private HTTPS checkout builds the exact requested commit',flush=True)
        source.pop('compose_path')
        source['inline_compose'] = True
        inline = 'services:\n  web:\n    build: .\n'
        inline_release = operation('deploy', source=source, compose=inline)
        assert inline_release['revision'] == first and content() == 'first-commit'
        print('PASS: panel-managed Compose builds the pinned private repository', flush=True)
        source.pop('revision')
        operation('deploy',source=source,compose=inline)
        assert content()=='second-commit'
        server.shutdown()
        operation('rollback',release=release['id'])
        assert content()=='first-commit'
        print('PASS: retained-image rollback works while the Git provider is unavailable',flush=True)
    finally:
        worker.terminate();worker.wait(timeout=40)
        server.shutdown();server.server_close()
        ids = subprocess.check_output(['docker','ps','-aq','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
        if ids: run('docker','rm','-f',*ids)
        networks = subprocess.check_output(['docker','network','ls','-q','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
        if networks: run('docker','network','rm',*networks)
        run('chown','-R','cove:cove','/var/lib/cove')
        run('chown','cove:cove','/run/cove')
        run('systemctl','start','cove-worker')
        log.close()
        print('Acceptance log: '+str(root/'acceptance.log'),flush=True)


if __name__ == '__main__':
    main()
