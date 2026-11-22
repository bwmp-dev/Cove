"""Cold PostgreSQL backup, exact restore and retention in the disposable VM."""
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import tempfile
import time
import sys
import urllib.request


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    if os.geteuid() != 0 or Path('/etc/hostname').read_text().strip() != 'cove-acceptance':
        raise SystemExit('This test requires the dedicated cove-acceptance VM.')
    os.umask(0o077)
    root = Path(tempfile.mkdtemp(prefix='cove-storage-'))
    log = (root/'acceptance.log').open('wb')
    workload = 'storage-'+secrets.token_hex(6)
    s3 = '--s3' in sys.argv
    policy = {'repository':'local','password':secrets.token_hex(32),'keep_last':2,'interval_hours':0}
    if s3:
        run('openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','2','-keyout',str(root/'private.key'),'-out',str(root/'public.crt'),'-subj','/CN=localhost','-addext','subjectAltName=DNS:localhost',stdout=log,stderr=log)
        access, secret = secrets.token_hex(12), secrets.token_hex(24)
        (root/'s3.json').write_text(json.dumps({'identities':[{'name':'fixture','credentials':[{'accessKey':access,'secretKey':secret}],'actions':['Admin','Read','Write','List','Tagging']}]}))
        run('docker','run','-d','--name',workload+'-s3','--user','0','--entrypoint','weed','-p','127.0.0.1:19443:9443','-v',str(root)+':/certs:ro','chrislusf/seaweedfs:latest','server','-dir=/data','-s3','-s3.port.https=9443','-s3.cert.file=/certs/public.crt','-s3.key.file=/certs/private.key','-s3.config=/certs/s3.json','-volume.max=2','-master.volumeSizeLimitMB=100',stdout=log,stderr=log)
        context = ssl.create_default_context(cafile=str(root/'public.crt'))
        for _ in range(120):
            try:
                with socket.create_connection(('127.0.0.1',19443),timeout=3) as tcp:
                    with context.wrap_socket(tcp,server_hostname='localhost'):
                        pass
                break
            except (OSError,AssertionError): time.sleep(1)
        else:
            run('docker','logs',workload+'-s3',stdout=log,stderr=log)
            run('docker','rm','-f','-v',workload+'-s3',stdout=log)
            raise RuntimeError('S3 fixture did not become ready; see '+str(root/'acceptance.log'))
        curl_config = f'url = "https://localhost:19443/cove-fixture"\nrequest = "PUT"\naws-sigv4 = "aws:amz:us-east-1:s3"\nuser = "{access}:{secret}"\ncacert = "{root}/public.crt"\n'
        run('curl','--fail','--silent','--show-error','--config','-',input=curl_config,text=True,stdout=log,stderr=log)
        policy.update(repository='s3:https://localhost:19443/cove-fixture',access_key=access,secret_key=secret,ca_certificate=(root/'public.crt').read_text())
    run('systemctl','stop','cove-worker')
    worker = subprocess.Popen([str(Path(__file__).parent/'cove-worker-linux-amd64'),'daemon'],stdout=log,stderr=log)
    try:
        def rpc(body):
            conn = http.client.HTTPConnection('localhost',timeout=60)
            conn.sock = socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
            conn.sock.settimeout(60)
            conn.sock.connect('/run/cove/worker.sock')
            conn.request('POST','/rpc',json.dumps(body),{'Content-Type':'application/json'})
            response = conn.getresponse()
            result = json.load(response)
            conn.close()
            if result.get('error'): raise RuntimeError(result['error'])
            return result

        for _ in range(60):
            try:
                rpc({'method':'probe'})
                break
            except OSError:
                time.sleep(1)

        def operation(action, **fields):
            request = {'id':secrets.token_hex(16),'workload':workload,'kind':'service','action':action} | fields
            rpc({'method':'submit','request':request})
            for _ in range(300):
                state = rpc({'method':'status','id':request['id']})['operation']['state']
                if state not in ('queued','running'):
                    if state != 'succeeded': raise RuntimeError(rpc({'method':'logs','id':request['id']}))
                    return request['id']
                time.sleep(1)
            raise RuntimeError('Operation timed out: '+action)

        password = secrets.token_hex(24)
        operation('deploy',compose='''services:
  database:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes:
      - data:/var/lib/postgresql/data
    healthcheck:
      test: [CMD-SHELL, pg_isready -h 127.0.0.1 -U postgres]
      interval: 1s
      timeout: 5s
      retries: 60
volumes:
  data:
''',environment={'DB_PASSWORD':password})
        container = rpc({'method':'containers','id':workload})['containers'][0]['id']

        def sql(statement):
            return subprocess.check_output(['docker','exec',container,'psql','-U','postgres','-At','-c',statement]).decode().strip()

        sql('CREATE TABLE acceptance(value text); INSERT INTO acceptance VALUES (\'before\')')
        first = operation('backup',backup=policy)
        assert sql('SELECT value FROM acceptance') == 'before'
        print('PASS: encrypted cold backup restarts PostgreSQL with data intact',flush=True)
        sql("INSERT INTO acceptance VALUES ('after')")
        run('docker','exec',container,'touch','/var/lib/postgresql/data/after-backup')
        operation('restore',backup=policy,backup_id=first)
        for _ in range(60):
            try:
                assert sql('SELECT value FROM acceptance') == 'before'
                break
            except (subprocess.CalledProcessError,AssertionError):
                time.sleep(1)
        else:
            raise RuntimeError('Restored database did not recover expected data')
        run('docker','exec',container,'test','!','-e','/var/lib/postgresql/data/after-backup')
        print('PASS: verified restore recovers database contents and removes later files',flush=True)
        records = rpc({'method':'backups','id':workload})['backups']
        assert len(records) == 2 and any(record['id'].endswith('-before') for record in records)
        print('PASS: pre-restore safety snapshot retained',flush=True)
        operation('backup',backup=policy)
        assert len(rpc({'method':'backups','id':workload})['backups']) == 2
        assert sql('SELECT value FROM acceptance') == 'before'
        print('PASS: retention prunes older snapshots without disturbing application data',flush=True)
    finally:
        worker.terminate()
        worker.wait(timeout=40)
        ids = subprocess.check_output(['docker','ps','-aq','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
        if ids: run('docker','rm','-f',*ids,stdout=log)
        for resource in ['volume','network']:
            ids = subprocess.check_output(['docker',resource,'ls','-q','--filter','label=com.docker.compose.project=cove-'+workload]).decode().split()
            if ids: run('docker',resource,'rm',*ids,stdout=log)
        repo = 'cove-backups-'+workload
        owned = subprocess.run(['docker','volume','inspect','--format','{{index .Labels "cove.backup"}}',repo],capture_output=True,text=True)
        if owned.returncode == 0 and owned.stdout.strip() == workload: run('docker','volume','rm',repo,stdout=log)
        if s3: run('docker','rm','-f','-v',workload+'-s3',stdout=log)
        run('chown','-R','cove:cove','/var/lib/cove')
        run('chown','cove:cove','/run/cove')
        run('systemctl','start','cove-worker')
        log.close()
        print('Acceptance log: '+str(root/'acceptance.log'),flush=True)


if __name__ == '__main__':
    main()
