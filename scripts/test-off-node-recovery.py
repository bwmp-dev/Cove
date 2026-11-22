"""Prove recovery on two SSH Docker hosts using an isolated loopback-only S3 fixture."""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import time

ROOT=Path(__file__).resolve().parent.parent

def main():
 parser=argparse.ArgumentParser()
 parser.add_argument('--source',required=True)
 parser.add_argument('--destination',required=True)
 parser.add_argument('--known-hosts',required=True)
 args=parser.parse_args()
 if args.source==args.destination:raise SystemExit('Two distinct hosts are required')
 options=['-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+args.known_hosts]
 name='cove-recovery-'+secrets.token_hex(5)
 remote='/tmp/'+name
 tunnels=[]
 created=[]
 def ssh(host,script):
  result=subprocess.run(['ssh',*options,host,'sudo -n python3 -'],input=script.encode(),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
  if result.returncode:
   print(result.stdout.decode(errors='replace'))
   raise RuntimeError('Remote acceptance command failed: '+result.stderr.decode(errors='replace'))
  print(result.stdout.decode(errors='replace'),end='',flush=True)
 def copy(host,path):subprocess.run(['scp',*options,str(path),host+':'+remote+'/'+path.name],check=True,stdout=subprocess.DEVNULL)
 with tempfile.TemporaryDirectory(prefix=name) as directory:
  fixture=Path(directory)
  subprocess.run(['go','test','-c','-o',str(fixture/'worker.test'),'./internal/worker'],cwd=ROOT,env=os.environ|{'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
  (fixture/'openssl.cnf').write_text('[req]\ndistinguished_name=dn\n[dn]\n')
  openssl=shutil.which('openssl') or 'C:/Program Files/Git/usr/bin/openssl.exe'
  subprocess.run([openssl,'req','-x509','-newkey','rsa:2048','-nodes','-days','2','-config',str(fixture/'openssl.cnf'),'-keyout',str(fixture/'key.pem'),'-out',str(fixture/'cert.pem'),'-subj','/CN=localhost','-addext','subjectAltName=DNS:localhost,IP:127.0.0.1'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
  access=secrets.token_hex(12);password=secrets.token_hex(24)
  backup={'repository':'s3:https://127.0.0.1:22180/backups/'+name,'password':secrets.token_hex(24),'access_key':access,'secret_key':password,'ca_certificate':(fixture/'cert.pem').read_text(),'keep_last':3}
  try:
   for host in (args.source,args.destination):
    subprocess.run(['ssh',*options,host,'mkdir -m 700 '+remote],check=True)
    created.append(host)
    copy(host,fixture/'worker.test')
    (fixture/'fixture.json').write_text(json.dumps({'Workload':name,'Phase':'backup' if host==args.source else 'restore','Backup':backup}))
    copy(host,fixture/'fixture.json')
   copy(args.destination,fixture/'cert.pem');copy(args.destination,fixture/'key.pem')
   server=['docker','run','-d','--name',name+'-storage','--network','host','-v',remote+':/fixture','rclone/rclone:1.76.0','serve','s3','/fixture/storage','--addr','127.0.0.1:22180','--cert','/fixture/cert.pem','--key','/fixture/key.pem','--auth-key',access+','+password]
   ssh(args.destination,"import os,subprocess\nos.makedirs("+repr(remote+'/storage/backups')+",exist_ok=True)\nr=subprocess.run("+repr(server)+",stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)\nif r.returncode:raise SystemExit('Storage fixture did not start')\n")
   with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
   for host,forward in [(args.destination,['-L',f'127.0.0.1:{port}:127.0.0.1:22180']),(args.source,['-R',f'127.0.0.1:22180:127.0.0.1:{port}'])]:
    proc=subprocess.Popen(['ssh',*options,'-o','ExitOnForwardFailure=yes','-o','ServerAliveInterval=10','-N',*forward,host],stdout=subprocess.DEVNULL,stderr=subprocess.PIPE)
    tunnels.append(proc)
    time.sleep(2)
    if proc.poll() is not None:raise RuntimeError('SSH fixture tunnel failed')
   for host in (args.source,args.destination):
    print('Running recovery phase on '+host,flush=True)
    ssh(host,"import os,subprocess\nos.chmod("+repr(remote+'/worker.test')+",0o700)\nr=subprocess.run(["+repr(remote+'/worker.test')+",'-test.run','^TestOffNodeRecovery$','-test.v','-test.timeout','10m'],env=os.environ|{'COVE_RECOVERY_FIXTURE':"+repr(remote+'/fixture.json')+"})\nraise SystemExit(r.returncode)\n")
  finally:
   for proc in reversed(tunnels):
    proc.terminate()
    try:proc.wait(timeout=10)
    except subprocess.TimeoutExpired:proc.kill();proc.wait()
   for host in created:
    ssh(host,"import subprocess,shutil\nsubprocess.run(['docker','rm','-f',"+repr(name+'-storage')+"],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)\nshutil.rmtree("+repr(remote)+",ignore_errors=False)\n")
if __name__=='__main__':main()
