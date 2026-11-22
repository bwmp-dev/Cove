"""Exercise encrypted remote backups and PTY shells against disposable Linux services."""
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def main():
    name = "cove-infra-" + secrets.token_hex(5)
    remote = "/tmp/" + name
    containers = []
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        fixture = Path(directory)
        run("go", "test", "-c", "-o", str(fixture / "worker.test"), "./internal/worker",
            env=os.environ | {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"})
        openssl = shutil.which("openssl") or "C:/Program Files/Git/usr/bin/openssl.exe"
        (fixture / "openssl.cnf").write_text("[req]\ndistinguished_name=dn\n[dn]\n")
        run(openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
            "-config", str(fixture / "openssl.cnf"), "-keyout", str(fixture / "key.pem"),
            "-out", str(fixture / "cert.pem"), "-subj", "/CN=localhost",
            "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        ssh_keygen = shutil.which("ssh-keygen")
        run(ssh_keygen, "-t", "ed25519", "-N", "", "-f", str(fixture / "host-key"), stdout=subprocess.DEVNULL)
        ca = (fixture / "cert.pem").read_text()
        ports = dict(zip(("s3", "sftp", "ftps", "webdav"), range(22180, 22184)))
        locations = []
        for protocol, port in ports.items():
            scheme = {"s3": "https", "webdav": "https", "sftp": "sftp", "ftps": "ftps"}[protocol]
            location = {"protocol": protocol, "endpoint": f"{scheme}://127.0.0.1:{port}", "path": "backups"}
            if protocol == "s3":
                location.update(access_key="fixture", secret_key="fixture-password", region="us-east-1", ca_certificate=ca)
            else:
                location.update(username="fixture", password="fixture-password")
                if protocol == "sftp":
                    location["host_key"] = (fixture / "host-key.pub").read_text().strip()
                else:
                    location["ca_certificate"] = ca
            if protocol == "ftps":
                location["tls_mode"] = "explicit"
            locations.append(location)
        (fixture / "locations.json").write_text(json.dumps(locations))
        (fixture / "ftp.py").write_text("""from pyftpdlib.authorizers import DummyAuthorizer
from pyftpdlib.handlers import TLS_FTPHandler
from pyftpdlib.servers import FTPServer
import sys
root=sys.argv[1]
a=DummyAuthorizer();a.add_user('fixture','fixture-password',root+'/server-ftps',perm='elradfmwMT')
h=TLS_FTPHandler;h.authorizer=a;h.certfile=root+'/cert.pem';h.keyfile=root+'/key.pem';h.tls_control_required=True;h.tls_data_required=True;h.passive_ports=range(22200,22221)
FTPServer(('127.0.0.1',22182),h).serve_forever()
""")
        try:
            run("docker", "run", "-d", "--name", name, "--network", "host", "-v", "/tmp:/tmp",
                "-v", "/var/run/docker.sock:/var/run/docker.sock", "docker:29-cli", "sleep", "600", stdout=subprocess.DEVNULL)
            containers.append(name)
            run("docker", "exec", name, "mkdir", "-p", remote)
            run("docker", "cp", str(fixture) + "/.", name + ":" + remote)
            run("docker", "exec", name, "cp", remote + "/worker.test", "/usr/local/bin/worker.test")
            run("docker", "exec", name, "chmod", "700", "/usr/local/bin/worker.test")
            for protocol, port in ports.items():
                data = remote + "/server-" + protocol
                run("docker", "exec", name, "mkdir", "-p", data + "/backups")
                args = ["serve", "ftp" if protocol == "ftps" else protocol, data, "--addr", f"127.0.0.1:{port}"]
                if protocol == "sftp":
                    args += ["--key", remote + "/host-key", "--user", "fixture", "--pass", "fixture-password"]
                else:
                    args += ["--cert", remote + "/cert.pem", "--key", remote + "/key.pem"]
                    args += ["--auth-key", "fixture,fixture-password"] if protocol == "s3" else ["--user", "fixture", "--pass", "fixture-password"]
                if protocol == "ftps":
                    args += ["--passive-port", "22200-22220", "--public-ip", "127.0.0.1"]
                container = name + "-" + protocol
                if protocol == "ftps":
                    run("docker", "run", "-d", "--name", container, "--network", "host", "-v", "/tmp:/tmp", "python:3.12-alpine", "sh", "-c", "pip install -q pyftpdlib pyopenssl && exec python " + remote + "/ftp.py " + remote, stdout=subprocess.DEVNULL)
                else:
                    run("docker", "run", "-d", "--name", container, "--network", "host", "-v", "/tmp:/tmp", "rclone/rclone:1.76.0", *args, stdout=subprocess.DEVNULL)
                containers.append(container)
            auth=run("docker","run","--rm","httpd:2-alpine","htpasswd","-Bbn","fixture","fixture-password",capture_output=True,text=True).stdout
            run("docker","exec","-i",name,"sh","-c","cat > "+remote+"/htpasswd",input=auth,text=True)
            registry=name+"-registry"
            run("docker","run","-d","--name",registry,"--network","host","-v","/tmp:/tmp","-e","REGISTRY_HTTP_ADDR=127.0.0.1:22184","-e","REGISTRY_AUTH=htpasswd","-e","REGISTRY_AUTH_HTPASSWD_REALM=Cove fixture","-e","REGISTRY_AUTH_HTPASSWD_PATH="+remote+"/htpasswd","registry:2",stdout=subprocess.DEVNULL)
            containers.append(registry)
            for _ in range(60):
                logs=subprocess.run(["docker","logs",name+"-ftps"],capture_output=True,text=True)
                if "starting FTPS" in logs.stderr: break
                time.sleep(1)
            else: raise RuntimeError("FTPS fixture did not start")
            run("docker", "exec", "-e", "COVE_STORAGE_FIXTURE=" + remote + "/locations.json", name,
                "/usr/local/bin/worker.test", "-test.v", "-test.run", "TestRemoteStorageRoundTrip|TestTerminalShell|TestPrivateRegistryDeployment", "-test.timeout", "12m")
        finally:
            for container in containers[1:]:
                subprocess.run(["docker", "rm", "-f", container], stdout=subprocess.DEVNULL)
            if name in containers:
                # Only the exact random directory created above is removed.
                run("docker", "exec", name, "rm", "-rf", remote)
                run("docker", "rm", "-f", name, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
