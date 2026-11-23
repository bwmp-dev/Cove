"""Exercise node SSH operations against disposable PostgreSQL and OpenSSH containers."""
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time

ROOT=Path(__file__).resolve().parent.parent

def run(*args,**kwargs):
    return subprocess.run(args,cwd=ROOT,check=True,**kwargs)

def main():
    name="cove-node-ssh-"+secrets.token_hex(5)
    image=name+":test"
    database=name+"-db"
    created=False
    built=False
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        fixture=Path(directory)
        try:
            run("go","build","-o",str(fixture/"cove-worker"),"./cmd/cove-worker",env=os.environ|{"GOOS":"linux","GOARCH":"amd64","CGO_ENABLED":"0"})
            (fixture/"entrypoint.sh").write_text("#!/bin/sh\nset -eu\nsu -s /bin/sh cove -c '/usr/local/bin/cove-worker daemon' &\nexec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no -o AllowTcpForwarding=local\n",newline="\n")
            (fixture/"Dockerfile").write_text("FROM docker:29-cli\nRUN apk add --no-cache openssh && adduser -D -h /var/lib/cove cove && adduser -D covejump && passwd -u cove && passwd -u covejump && mkdir -p /run/sshd /run/cove /var/lib/cove/.ssh /home/covejump/.ssh && chown -R cove:cove /run/cove /var/lib/cove && chown -R covejump:covejump /home/covejump && ssh-keygen -A\nCOPY cove-worker /usr/local/bin/cove-worker\nCOPY entrypoint.sh /entrypoint.sh\nRUN chmod 755 /entrypoint.sh /usr/local/bin/cove-worker\nENTRYPOINT [\"/entrypoint.sh\"]\n",newline="\n")
            run("docker","build","-t",image,str(fixture),stdout=subprocess.DEVNULL)
            built=True
            password=secrets.token_hex(24)
            env=os.environ|{"POSTGRES_PASSWORD":password}
            run("docker","run","-d","--name",database,"--label","cove.test=true","-e","POSTGRES_PASSWORD","-e","POSTGRES_DB=cove_test","-p","127.0.0.1::5432","postgres:17-alpine",env=env,stdout=subprocess.DEVNULL)
            created=True
            port=run("docker","port",database,"5432/tcp",capture_output=True,text=True).stdout.strip().rsplit(":",1)[1]
            for _ in range(30):
                if subprocess.run(["docker","exec",database,"pg_isready","-U","postgres"],capture_output=True).returncode==0: break
                time.sleep(1)
            else: raise RuntimeError("Disposable PostgreSQL did not start")
            env.update(COVE_TEST_DATABASE_URL=f"postgres://postgres:{password}@127.0.0.1:{port}/cove_test?sslmode=disable",COVE_TEST_DATABASE_CONTAINER=database,COVE_TEST_NODE_SSH_IMAGE=image)
            run("go","test","./internal/control","-run",sys.argv[1] if len(sys.argv)>1 else "TestBastion|TestHostKey|TestSSHIdentity","-v","-count=1","-timeout","10m",env=env)
        finally:
            if created: run("docker","rm","-f","-v",database,stdout=subprocess.DEVNULL)
            if built: run("docker","image","rm",image,stdout=subprocess.DEVNULL)

if __name__=="__main__":
    main()
