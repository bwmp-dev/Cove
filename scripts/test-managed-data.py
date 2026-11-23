"""Exercise managed data APIs and native engines inside test-browser.py's owned fixture."""
import datetime
import json
import os
from pathlib import Path
import re
import secrets
import socket
import ssl
import subprocess
import struct
import shutil
import tempfile
import time
import urllib.error
import urllib.request


def safe_detail(value):
    value = re.sub(r"(?i)(mongodb|postgresql|mysql|redis|clickhouse)://[^\s]+", r"\1://[redacted]", value)
    return re.sub(r"[a-f0-9]{64}", "[redacted]", value)


def docker(*args, env=None, input=None, check=True):
    result = subprocess.run(["docker", *args], env=env, input=input, capture_output=True, text=True)
    if check and result.returncode:
        raise RuntimeError("Owned Docker fixture command failed: " + args[0])
    return result


def main():
    origin = os.environ["COVE_BROWSER_URL"]
    worker = os.environ["COVE_BROWSER_WORKER"]
    if not worker.startswith("cove-browser-") or not os.environ.get("COVE_BROWSER_CA"):
        raise RuntimeError("Run through python scripts/test-browser.py --managed-data")
    context = ssl.create_default_context(cafile=os.environ["COVE_BROWSER_CA"])

    def api(method, path, body=None, status=200):
        request = urllib.request.Request(origin + path, method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": "Bearer " + os.environ["COVE_ADMIN_TOKEN"], "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, context=context, timeout=120) as response:
                if response.status != status:
                    raise RuntimeError("Unexpected API status: " + path)
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code == status:
                return None
            detail = safe_detail(error.read(4096).decode(errors="replace"))
            raise RuntimeError(f"API {method} {path}: HTTP {error.code}: {detail}") from None

    def operation(resource, action="deploy"):
        task = api("POST", f"/api/workloads/{resource}/operations", {"action": action}, 202)["id"]
        wait(task)

    def wait(task):
        deadline = time.monotonic() + 300
        while time.monotonic() < deadline:
            result = api("GET", "/api/operations/" + task)
            if result["state"] == "succeeded":
                return
            if result["state"] in ("failed", "cancelled", "interrupted"):
                log = api("GET", "/api/operations/" + task + "/logs")["logs"]
                raise RuntimeError("Native data operation failed: " + task + "\n" + safe_detail(str(result.get("result",{}).get("error",""))) + "\n" + safe_detail(log[-4000:]))
            time.sleep(1)
        raise RuntimeError("Native data operation timed out: " + task)

    node = next(n["id"] for n in api("GET", "/api/nodes") if n["name"] == "ready-browser-worker")
    project = api("POST", "/api/projects", {"name": "Managed data acceptance"}, 201)["id"]
    engines = api("GET", "/api/database-engines")["engines"]
    if any(engine.get("minimum",{}).get("cpus",0)<=0 for engine in engines):
        raise RuntimeError("Database engine sizing API omitted minimum.cpus")

    def create(body):
        return api("POST", "/api/workloads", {"node_id": node, "project_id": project, **body}, 201)["id"]

    def container(resource, service):
        ids = docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-" + resource,
                     "--filter", "label=com.docker.compose.service=" + service).stdout.split()
        if len(ids) != 1:
            raise RuntimeError("Expected one owned native database container")
        return ids[0]

    commands = {
        "postgresql": 'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -w -At -c "SELECT 1"',
        "mysql": 'MYSQL_PWD="$COVE_PASSWORD" mysql -h 127.0.0.1 -u app -sN -e "SELECT 1"',
        "mariadb": 'MYSQL_PWD="$COVE_PASSWORD" mariadb -h 127.0.0.1 -u app -sN -e "SELECT 1"',
        "mongodb": 'mongosh --quiet --eval \'if(db.getSiblingDB("admin").auth("app",process.env.COVE_PASSWORD).ok!==1)quit(1);print("1")\'',
        "redis": 'REDISCLI_AUTH="$COVE_PASSWORD" redis-cli --no-auth-warning ping | grep -x PONG',
        "valkey": 'REDISCLI_AUTH="$COVE_PASSWORD" valkey-cli --no-auth-warning ping | grep -x PONG',
        "clickhouse": 'clickhouse-client --host 127.0.0.1 --user app --password "$COVE_PASSWORD" --query "SELECT 1"',
    }

    def native(resource, engine, service, password, command=None, accepted=True):
        result = docker("exec", "-e", "COVE_PASSWORD", container(resource, service), "sh", "-ec", command or commands[engine],
                        env=os.environ | {"COVE_PASSWORD": password}, check=False)
        good = result.returncode == 0 and (command is not None or result.stdout.strip() in ("1", "PONG"))
        if good != accepted:
            raise RuntimeError("Native credential verification disagreed: " + engine)
        return result.stdout.strip()

    def maintenance(resource, kind, **fields):
        if kind=="upgrade":fields["confirm_writes_stopped"]=True
        details = api("GET", f"/api/workloads/{resource}/database")
        task = api("POST", f"/api/workloads/{resource}/database/maintenance",
                   {"kind": kind, "revision": details["revision"], "confirm": True, **fields}, 202)
        wait(task["id"])
        return task["resource_id"]

    databases = {}
    selected = os.environ.get("COVE_MANAGED_DATA_CASES", "")
    if selected:
        if selected == "pooler":
            return test_pooler(api, create, operation, native, container, engines)
        if selected == "tls":
            return test_database_tls(api, create, operation, native, container, engines, maintenance)
        if selected == "tools":
            return test_database_tools(api, create, operation, container, worker, engines)
        if selected == "upgrades":
            return test_database_copies(api, create, operation, native, engines, maintenance)
        if selected == "extensions":
            return test_database_extensions(api, create, operation, native, worker, engines, maintenance)
        if selected == "buckets":
            return test_buckets(api, create, operation, container, worker)
        raise RuntimeError("Unsupported focused data acceptance case")
    test_database_copies(api,create,operation,native,engines,maintenance)
    test_pooler(api,create,operation,native,container,engines)
    test_database_tls(api,create,operation,native,container,engines,maintenance)
    test_database_tools(api,create,operation,container,worker,engines)
    consumer_compose = json.dumps({"services":{"app":{"image":"busybox:1.37","environment":{"DATABASE_URL":"${DATABASE_URL}","DB_HOST":"${DB_HOST}","DB_PORT":"${DB_PORT}","LITERAL":"${COVE_LITERAL}"},"command":["sh","-ec",'printf "%s" "$$LITERAL" > /tmp/cove-literal; test -n "$$DATABASE_URL"; sleep 600']}}})
    for engine in engines:
        print("Checking native managed engine: " + engine["engine"], flush=True)
        body = {"engine": engine["engine"], "version": "17.12-alpine" if engine["engine"] == "postgresql" else engine["version"],
                "template_revision": engine["template_revision"]}
        resource = create({"name": "Native " + engine["name"], "kind": "database", "database": body})
        operation(resource)
        before = api("GET", f"/api/workloads/{resource}/database/credentials")
        native(resource, engine["engine"], engine["service"], before["password"])
        maintenance(resource, "rotate")
        after = api("GET", f"/api/workloads/{resource}/database/credentials")
        if before["password"] == after["password"]:
            raise RuntimeError("Rotation did not change the generated secret")
        native(resource, engine["engine"], engine["service"], after["password"])
        native(resource, engine["engine"], engine["service"], before["password"], accepted=False)
        consumer = create({"name": "Bound " + engine["name"], "compose": consumer_compose,
                           "environment": {"DATABASE_URL": "{{resource.CONNECTION}}","DB_HOST":"{{resource.HOST}}","DB_PORT":"{{resource.PORT}}","COVE_LITERAL":"literal$$two$one"}})
        api("PUT", f"/api/workloads/{consumer}/variable-bindings", {"name": "CONNECTION", "source_id": resource, "source_key": "DB_URL"})
        for name,key in [("HOST","DB_HOST"),("PORT","DB_PORT")]:
            api("PUT",f"/api/workloads/{consumer}/variable-bindings",{"name":name,"source_id":resource,"source_key":key})
        metadata=api("GET",f"/api/workloads/{resource}/database")
        connection=api("POST","/api/private-connections",{"source_id":consumer,"source_service":"app","target_id":resource,"target_service":engine["service"],"port":engine["port"],"alias":metadata["host"]},201)["id"]
        operation(consumer)
        environment = json.loads(docker("inspect", "--format", "{{json .Config.Env}}", container(consumer, "app")).stdout)
        if "DATABASE_URL=" + after["url"] not in environment:
            raise RuntimeError("Bound URL did not reach the real consumer")
        if docker("exec", container(consumer, "app"), "cat", "/tmp/cove-literal").stdout != "literal$$two$one":
            raise RuntimeError("Compose changed a literal dollar or shell variable escape")
        for _ in range(60):
            connected=docker("exec",container(consumer,"app"),"sh","-ec",'nc -z -w 2 "$DB_HOST" "$DB_PORT"',check=False)
            if connected.returncode==0:
                break
            time.sleep(1)
        else:
            raise RuntimeError("Private database binding is unreachable: "+engine["engine"])
        api("DELETE","/api/private-connections/"+connection,status=202)
        operation(consumer, "stop")
        api("DELETE", f"/api/workloads/{consumer}")
        databases[engine["engine"]] = (resource, engine["service"], after["password"])
        if engine["engine"] != "postgresql":
            operation(resource, "stop")
            api("DELETE", f"/api/workloads/{resource}")
        print("PASS native create/bind/rotate/old-password-rejection: " + engine["engine"], flush=True)

    postgres, service, password = databases["postgresql"]
    native(postgres, "postgresql", service, password,
           'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -v ON_ERROR_STOP=1 -c "CREATE TABLE cove_copy_probe(value integer); INSERT INTO cove_copy_probe VALUES(42)"')
    maintenance(postgres, "extension", extension="pg_trgm")
    copied = maintenance(postgres, "upgrade", version="18.7-alpine", name="PostgreSQL 18 copied data")
    copied_password = api("GET", f"/api/workloads/{copied}/database/credentials")["password"]
    query = 'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -w -At -c "SELECT value FROM cove_copy_probe"'
    if native(copied, "postgresql", service, copied_password, query) != "42" or native(postgres, "postgresql", service, password, query) != "42":
        raise RuntimeError("Copy upgrade lost data or changed its source")
    extension_query = 'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -w -At -c "SELECT count(*) FROM pg_extension WHERE extname=\'pg_trgm\'"'
    if native(copied, "postgresql", service, copied_password, extension_query) != "1":
        raise RuntimeError("Copy upgrade lost the enabled extension")
    print("PASS PostgreSQL extension and 17-to-18 logical copy with retained source/data", flush=True)

    # The containerized fixture needs host-like access to bridge addresses for the real SSH tunnel.
    attached = "cove-" + postgres + "_default"
    docker("network", "connect", attached, worker)
    try:
        with tempfile.TemporaryDirectory(prefix="cove-data-access-") as directory:
            identity = Path(directory) / "key"
            subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(identity)], check=True, capture_output=True)
            lease_id = secrets.token_hex(16)
            expiry = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=12)
            request = {"method": "tunnel_open", "tunnel": {"id": lease_id, "workload": postgres, "service": service,
                       "port": 8080, "tool": "adminer", "public_key": identity.with_suffix(".pub").read_text(), "expires_at": expiry.isoformat()}}
            reply = json.loads(docker("exec", "-i", worker, "/usr/local/bin/cove-worker", "rpc", input=json.dumps(request)).stdout)
            if reply.get("error"):
                raise RuntimeError("Private Adminer fixture was refused")
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            host_key = docker("exec", worker, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").stdout.strip()
            client = subprocess.Popen([os.environ["COVE_BROWSER_CLIENT"], "connect", "--host", "127.0.0.1", "--ssh-port", os.environ["COVE_BROWSER_SSH_PORT"],
                "--local-port", str(port), "--identity", str(identity), "--host-key", host_key, "--expires", (expiry + datetime.timedelta(seconds=60)).isoformat()], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                for _ in range(30):
                    try:
                        with urllib.request.urlopen(f"http://127.0.0.1:{port}", timeout=1) as response:
                            if "Adminer" in response.read().decode():
                                break
                    except (OSError, urllib.error.URLError):
                        time.sleep(.2)
                else:
                    raise RuntimeError("Private Adminer tunnel did not serve its login page")
                time.sleep(max(0, expiry.timestamp() - time.time()) + 1)
                for _ in range(30):
                    if docker("inspect","cove-dbtool-"+lease_id,check=False).returncode!=0:
                        break
                    time.sleep(.2)
                else:
                    raise RuntimeError("Expired Adminer helper remains")
                if client.poll() is not None:
                    raise RuntimeError("Client expired before the server lease could be checked")
                try:
                    urllib.request.urlopen(f"http://127.0.0.1:{port}", timeout=1)
                except (OSError, urllib.error.URLError):
                    pass
                else:
                    raise RuntimeError("Expired Adminer access remains usable")
            finally:
                if client.poll() is None:
                    client.terminate()
                client.wait(timeout=10)
    finally:
        docker("network", "disconnect", attached, worker)
    print("PASS real private Adminer HTTP tunnel expiry and helper removal", flush=True)
    for resource in (copied,postgres):
        operation(resource,"stop")
        api("DELETE",f"/api/workloads/{resource}")
    test_buckets(api, create, operation, container, worker)


def test_database_extensions(api, create, operation, native, worker, engines, maintenance):
    engine = next(item for item in engines if item["engine"] == "postgresql")
    resource = create({"name": "Extension boundary", "kind": "database", "database": {
        "engine": "postgresql", "version": engine["version"], "template_revision": engine["template_revision"]}})
    operation(resource)
    password = api("GET", f"/api/workloads/{resource}/database/credentials")["password"]
    compose = api("GET", f"/api/workloads/{resource}")["compose"]
    for extension in ("pg_trgm|hstore", "hstore|uuid-ossp", "pg_stat_statements|vector|postgis"):
        metadata = api("GET", f"/api/workloads/{resource}/database")
        api("POST", f"/api/workloads/{resource}/database/maintenance", {
            "kind": "extension", "extension": extension, "enabled": False,
            "confirm": True, "revision": metadata["revision"]}, 400)
        task = secrets.token_hex(16)
        inner = {"id": task, "workload": resource, "action": "deploy", "compose": compose,
                 "environment": {"POSTGRES_PASSWORD": password}}
        request = {"method": "submit", "request": {**inner, "database_maintenance": {
            "id": task, "kind": "extension", "engine": "postgresql", "service": engine["service"],
            "extension": extension, "disable_extension": True, "request": inner}}}
        reply = json.loads(docker("exec", "-i", worker, "/usr/local/bin/cove-worker", "rpc", input=json.dumps(request)).stdout)
        if reply.get("error") != "unsupported PostgreSQL extension" or reply.get("operation"):
            raise RuntimeError("Worker did not reject the combined extension name at validation")
        docker("exec", worker, "test", "!", "-e", f"/var/lib/cove/operations/{task}.json")
        api("GET", f"/api/workloads/{resource}/database/credentials")
    print("PASS combined extension names rejected by real API and worker without a journal or lock", flush=True)

    def sql(query):
        return native(resource, "postgresql", engine["service"], password,
                      'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -v ON_ERROR_STOP=1 -At -c "' + query + '"')

    for extension in ("pg_trgm", "pg_stat_statements"):
        maintenance(resource, "extension", extension=extension)
        if sql("SELECT count(*) FROM pg_extension WHERE extname='" + extension + "'") != "1":
            raise RuntimeError("Allowed extension was not installed")
        if extension == "pg_stat_statements" and sql("SELECT count(*) >= 0 FROM pg_stat_statements") != "t":
            raise RuntimeError("PostgreSQL preload was not configured")
        maintenance(resource, "extension", extension=extension, enabled=False)
        if sql("SELECT count(*) FROM pg_extension WHERE extname='" + extension + "'") != "0":
            raise RuntimeError("Allowed extension was not disabled")
    sql("CREATE EXTENSION hstore; CREATE TABLE cove_ext_dependency(v hstore)")
    metadata = api("GET", f"/api/workloads/{resource}/database")
    if "hstore" in metadata["database"].get("extensions", []):
        raise RuntimeError("Native hstore was incorrectly recorded as managed")
    api("POST", f"/api/workloads/{resource}/database/maintenance", {
        "kind": "extension", "extension": "hstore", "enabled": False,
        "confirm": True, "revision": metadata["revision"]}, 409)
    api("GET", f"/api/workloads/{resource}/database/credentials")
    sql("DROP TABLE cove_ext_dependency")
    maintenance(resource, "extension", extension="hstore", enabled=False)
    if sql("SELECT count(*) FROM pg_extension WHERE extname='hstore'") != "0":
        raise RuntimeError("Unmanaged extension was not safely disabled")
    operation(resource, "stop")
    api("DELETE", f"/api/workloads/{resource}")
    print("PASS real extension enable/disable, preload, dependency refusal and unmanaged hstore RESTRICT removal", flush=True)


def test_database_copies(api, create, operation, native, engines, maintenance):
    sources={"postgresql":"17.12-alpine","mysql":"8.0.46","mariadb":"10.11.19","mongodb":"7.0.48-jammy"}
    for engine in engines:
        engine_id=engine["engine"]
        selected=os.environ.get("COVE_MANAGED_COPY_ENGINES","").split(",")
        if selected!=[""] and engine_id not in selected:continue
        if engine_id not in sources: continue
        source=create({"name":"Copy source "+engine_id,"kind":"database","database":{"engine":engine_id,"version":sources[engine_id],"template_revision":engine["template_revision"]}})
        operation(source)
        source_password=api("GET",f"/api/workloads/{source}/database/credentials")["password"]
        if engine_id=="postgresql":
            native(source,engine_id,engine["service"],source_password,'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -v ON_ERROR_STOP=1 -c "CREATE EXTENSION hstore; CREATE TABLE cove_ext_dependency(v hstore)"')
            details=api("GET",f"/api/workloads/{source}/database")
            api("POST",f"/api/workloads/{source}/database/maintenance",{"kind":"extension","extension":"hstore","enabled":False,"confirm":True,"revision":details["revision"]},409)
            api("GET",f"/api/workloads/{source}/database/credentials")
            native(source,engine_id,engine["service"],source_password,'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -v ON_ERROR_STOP=1 -c "DROP TABLE cove_ext_dependency"')
            maintenance(source,"extension",extension="hstore",enabled=False)
            installed=native(source,engine_id,engine["service"],source_password,'PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -At -c "SELECT count(*) FROM pg_extension WHERE extname=\'hstore\'"')
            if installed!="0":raise RuntimeError("Unmanaged extension was not safely disabled")
            print("PASS unmanaged extension dependency refusal without a lock, then RESTRICT-only removal",flush=True)
            write='PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -v ON_ERROR_STOP=1 -c "CREATE TABLE cove_copy_probe(value integer); INSERT INTO cove_copy_probe VALUES(42)"'
            query='PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -At -c "SELECT value FROM cove_copy_probe"'
            for extension in ("pg_trgm","pg_stat_statements"):
                maintenance(source,"extension",extension=extension)
            probe='PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -At -c "SELECT count(*) >= 0 FROM pg_stat_statements"'
            if native(source,engine_id,engine["service"],source_password,probe)!="t": raise RuntimeError("PostgreSQL preload missing")
            maintenance(source,"extension",extension="pg_stat_statements",enabled=False)
            probe='PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -At -c "SELECT count(*) FROM pg_extension WHERE extname=\'pg_stat_statements\'"'
            if native(source,engine_id,engine["service"],source_password,probe)!="0": raise RuntimeError("PostgreSQL extension disable failed")
            targets=[engine["version"],"18.7-vector0.8.7","18.6-postgis3.6"]
        elif engine_id in ("mysql","mariadb"):
            client="mysql" if engine_id=="mysql" else "mariadb"
            prefix='MYSQL_PWD="$COVE_PASSWORD" '+client+' -h 127.0.0.1 -u app app '
            write=prefix+'-e "CREATE TABLE cove_copy_probe(value integer); INSERT INTO cove_copy_probe VALUES(42)"'
            query=prefix+'-sN -e "SELECT value FROM cove_copy_probe"'
            targets=[engine["version"]]
        else:
            prefix='mongosh --quiet --eval \'if(db.getSiblingDB("admin").auth("app",process.env.COVE_PASSWORD).ok!==1)quit(1);'
            write=prefix+'db.getSiblingDB("app").cove_copy_probe.insertOne({value:42})\''
            query=prefix+'print(db.getSiblingDB("app").cove_copy_probe.findOne().value)\''
            targets=[engine["version"]]
        native(source,engine_id,engine["service"],source_password,write)
        for version in targets:
            details=api("GET",f"/api/workloads/{source}/database")
            api("POST",f"/api/workloads/{source}/database/preflight",{"revision":details["revision"],"version":version})
            target=maintenance(source,"upgrade",version=version,name="Verified copy "+version)
            target_password=api("GET",f"/api/workloads/{target}/database/credentials")["password"]
            for resource,password in [(source,source_password),(target,target_password)]:
                if native(resource,engine_id,engine["service"],password,query)!="42": raise RuntimeError("Copy or retained source lost data: "+engine_id)
            if engine_id=="postgresql" and "-vector" in version:
                probe='PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -At -c "SELECT vector_dims(\'[1,2,3]\'::vector)"'
                if native(target,engine_id,engine["service"],target_password,probe)!="3": raise RuntimeError("pgvector not functional")
                maintenance(target,"extension",extension="vector",enabled=False)
            if engine_id=="postgresql" and "-postgis" in version:
                probe='PGPASSWORD="$COVE_PASSWORD" psql -h "$HOSTNAME" -U app -d app -At -c "SELECT ST_X(ST_Point(42,1))"'
                if native(target,engine_id,engine["service"],target_password,probe)!="42": raise RuntimeError("PostGIS not functional")
                maintenance(target,"extension",extension="postgis",enabled=False)
            operation(target,"stop")
            api("DELETE",f"/api/workloads/{target}")
            print("PASS verified separate "+engine_id+" copy "+sources[engine_id]+" -> "+version+" with source retained",flush=True)
        if engine_id=="postgresql":
            test_copy_recovery(api,operation,native,source,source_password,query)
        operation(source,"stop")
        api("DELETE",f"/api/workloads/{source}")


def test_copy_recovery(api,operation,native,source,password,query):
    import hashlib
    worker=os.environ["COVE_BROWSER_WORKER"]
    owner=worker.removesuffix("-worker")
    api("PUT","/api/teams/default/deployment-policy",{"approval":True})
    try:
        for discard in (False,True):
            name="Deliberately interrupted copy" if discard else "Retry verified copy"
            revision=api("GET",f"/api/workloads/{source}/database")["revision"]
            copy=api("POST",f"/api/workloads/{source}/database/maintenance",{"kind":"upgrade","revision":revision,"confirm":True,"confirm_writes_stopped":True,"version":"18.7-alpine","name":name},202)
            task,target=copy["id"],copy["resource_id"]
            checksum=hashlib.sha256(b"deliberately-invalid-archive").hexdigest() if discard else "0"*64
            command='test "$(cat /var/lib/cove/.browser-owner)" = "$1"; umask 077; mkdir -p /var/lib/cove/database-maintenance; printf deliberately-invalid-archive > "/var/lib/cove/database-maintenance/$2.dump"; printf %s "$3" > "/var/lib/cove/database-maintenance/$2.dump.sha256"'
            docker("exec",worker,"sh","-ec",command,"cove",owner,task,checksum)
            api("POST",f"/api/operations/{task}/approve",{})
            for _ in range(180):
                state=api("GET",f"/api/operations/{task}")["state"]
                if state=="failed":break
                if state in ("succeeded","cancelled"):raise RuntimeError("Faulted copy did not fail closed")
                time.sleep(1)
            else:raise RuntimeError("Faulted copy did not settle")
            api("GET",f"/api/workloads/{source}/database/credentials")
            if native(source,"postgresql","postgres",password,query)!="42":raise RuntimeError("Failed copy changed its source")
            api("GET",f"/api/workloads/{target}/database/credentials",status=409)
            if discard:
                running=docker("ps","-q","--filter","label=com.docker.compose.project=cove-"+target).stdout.strip()
                if running:raise RuntimeError("Failed restore target was not stopped automatically")
                api("POST",f"/api/workloads/{source}/database/maintenance/{task}/discard",{"confirm":True,"name":name})
                api("POST",f"/api/workloads/{source}/database/maintenance/{task}/discard",{"confirm":True,"name":name})
                if docker("ps","-aq","--filter","label=com.docker.compose.project=cove-"+target).stdout.strip():raise RuntimeError("Discard left target containers")
                if docker("volume","ls","-q","--filter","label=com.docker.compose.project=cove-"+target).stdout.strip():raise RuntimeError("Discard left target volumes")
            else:
                retry=f"/api/workloads/{source}/database/maintenance/{task}/retry"
                api("POST",retry,{},400)
                busy=api("POST",f"/api/workloads/{source}/operations",{"action":"deploy"},202)["id"]
                api("POST",retry,{"confirm_writes_stopped":True},409)
                api("POST",f"/api/operations/{busy}/cancel",{},202)
                for _ in range(30):
                    if api("GET",f"/api/operations/{busy}")["state"]=="cancelled":break
                    time.sleep(.2)
                else:raise RuntimeError("Owned blocked deployment did not cancel")
                docker("exec",worker,"sh","-ec",'test "$(cat /var/lib/cove/.browser-owner)" = "$1"; rm -f "/var/lib/cove/database-maintenance/$2.dump" "/var/lib/cove/database-maintenance/$2.dump.sha256"',"cove",owner,task)
                api("POST",retry,{"confirm_writes_stopped":True},202)
                await_task(api,task)
                target_password=api("GET",f"/api/workloads/{target}/database/credentials")["password"]
                if native(target,"postgresql","postgres",target_password,query)!="42":raise RuntimeError("Retried immutable copy lost data")
                operation(target,"stop")
            api("DELETE",f"/api/workloads/{target}")
        print("PASS native snapshot rejection, source recovery, same-copy retry, busy refusal, failed-restore rollback and idempotent discard",flush=True)
    finally:
        api("PUT","/api/teams/default/deployment-policy",{"approval":False})


def test_database_tools(api, create, operation, container, worker, engines):
    for engine_id, tool, tool_port in [("mongodb", "mongo-express", 8081), ("redis", "redisinsight", 5540)]:
        engine = next(e for e in engines if e["engine"] == engine_id)
        resource = create({"name": "Private " + tool, "kind": "database", "database": {"engine":engine_id,"version":engine["version"],"template_revision":engine["template_revision"]}})
        operation(resource)
        credentials = api("GET", f"/api/workloads/{resource}/database/credentials")
        network = "cove-" + resource + "_default"
        docker("network", "connect", network, worker)
        try:
            with tempfile.TemporaryDirectory(prefix="cove-private-tool-") as directory:
                identity = Path(directory) / "key"
                subprocess.run(["ssh-keygen","-q","-t","ed25519","-N","","-f",str(identity)],check=True,capture_output=True)
                lease_id = secrets.token_hex(16)
                expiry = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=120)
                request = {"method":"tunnel_open","tunnel":{"id":lease_id,"workload":resource,"service":engine["service"],"port":tool_port,"tool":tool,"public_key":identity.with_suffix(".pub").read_text(),"expires_at":expiry.isoformat(),"credentials":{"engine":engine_id,"host":engine["service"],"port":engine["port"],"user":credentials["user"],"password":credentials["password"],"database":credentials["database"]}}}
                reply = json.loads(docker("exec","-i",worker,"/usr/local/bin/cove-worker","rpc",input=json.dumps(request)).stdout)
                if reply.get("error"):
                    raise RuntimeError("Private tool fixture was refused: " + tool)
                with socket.socket() as reservation:
                    reservation.bind(("127.0.0.1",0))
                    port = reservation.getsockname()[1]
                host_key = docker("exec",worker,"cat","/etc/ssh/ssh_host_ed25519_key.pub").stdout.strip()
                client = subprocess.Popen([os.environ["COVE_BROWSER_CLIENT"],"connect","--host","127.0.0.1","--ssh-port",os.environ["COVE_BROWSER_SSH_PORT"],"--local-port",str(port),"--identity",str(identity),"--host-key",host_key,"--expires",expiry.isoformat()],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
                try:
                    import base64
                    authorization = base64.b64encode((credentials["user"]+":"+credentials["password"]).encode()).decode()
                    for _ in range(90):
                        try:
                            req = urllib.request.Request(f"http://127.0.0.1:{port}/",headers={"Authorization":"Basic "+authorization})
                            with urllib.request.urlopen(req,timeout=2) as response:
                                if response.status==200 and ("Mongo Express" if tool=="mongo-express" else "Redis Insight").lower().replace(" ","") in response.read().decode().lower().replace(" ",""):
                                    break
                        except (OSError,urllib.error.URLError):
                            pass
                        time.sleep(.5)
                    else:
                        raise RuntimeError("Private tool did not serve over authenticated SSH: " + tool)
                    if tool=="mongo-express":
                        try:
                            urllib.request.urlopen(f"http://127.0.0.1:{port}/",timeout=2)
                        except urllib.error.HTTPError as error:
                            if error.code!=401: raise
                        else:
                            raise RuntimeError("Mongo Express basic authentication missing")
                    else:
                        with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/settings/agreements/spec",timeout=3) as response:
                            agreement_spec=json.load(response)
                        agreements={name:name in ("eula","encryption") for name in agreement_spec["agreements"]}
                        agreement_request=urllib.request.Request(f"http://127.0.0.1:{port}/api/settings",data=json.dumps({"agreements":agreements}).encode(),headers={"Content-Type":"application/json"},method="PATCH")
                        with urllib.request.urlopen(agreement_request,timeout=10) as response:
                            response.read()
                        with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/databases",timeout=3) as response:
                            databases=json.load(response)
                        if not databases or not any(d.get("host")==engine["service"] for d in databases):
                            raise RuntimeError("Redis Insight did not preload its scoped database")
                        forbidden=urllib.request.Request(f"http://127.0.0.1:{port}/api/databases",data=json.dumps({"host":"unrelated.invalid","port":6379,"name":"Unrelated"}).encode(),headers={"Content-Type":"application/json"},method="POST")
                        try:
                            urllib.request.urlopen(forbidden,timeout=3)
                        except urllib.error.HTTPError as error:
                            if error.code!=403: raise
                        else: raise RuntimeError("Redis Insight permits unrelated connections")
                    published=json.loads(docker("inspect","--format","{{json .HostConfig.PortBindings}}","cove-dbtool-"+lease_id).stdout)
                    if published: raise RuntimeError("Admin tool published a host port")
                    reply=json.loads(docker("exec","-i",worker,"/usr/local/bin/cove-worker","rpc",input=json.dumps({"method":"tunnel_close","id":lease_id})).stdout)
                    if reply.get("error") or docker("inspect","cove-dbtool-"+lease_id,check=False).returncode==0:
                        raise RuntimeError("Revoked private tool remains")
                    try:
                        urllib.request.urlopen(f"http://127.0.0.1:{port}/",timeout=1)
                    except (OSError,urllib.error.URLError): pass
                    else: raise RuntimeError("Revoked tool still reachable")
                    request["tunnel"]["id"] = lease_id = secrets.token_hex(16)
                    request["tunnel"]["expires_at"]=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=4)).isoformat()
                    reply=json.loads(docker("exec","-i",worker,"/usr/local/bin/cove-worker","rpc",input=json.dumps(request)).stdout)
                    if reply.get("error"): raise RuntimeError("Expiring tool refused")
                    for _ in range(40):
                        if docker("inspect","cove-dbtool-"+lease_id,check=False).returncode!=0: break
                        time.sleep(.25)
                    else: raise RuntimeError("Expired private tool remains")
                finally:
                    if client.poll() is None: client.terminate()
                    client.wait(timeout=10)
        finally:
            docker("network","disconnect",network,worker)
        operation(resource,"stop")
        api("DELETE",f"/api/workloads/{resource}")
        print("PASS private SSH tool, database preload, revocation, expiry and no host ports: "+tool,flush=True)


def test_database_tls(api, create, operation, native, container, engines, maintenance):
    openssl = shutil.which("openssl") or "C:/Program Files/Git/usr/bin/openssl.exe"
    selected = os.environ.get("COVE_MANAGED_TLS_ENGINES", "").split(",")
    engines = [engine for engine in engines if selected == [""] or engine["engine"] in selected]
    if not engines:
        raise RuntimeError("No TLS engines selected")
    with tempfile.TemporaryDirectory(prefix="cove-native-tls-") as directory:
        root = Path(directory)
        (root / "openssl.cnf").write_text("[req]\ndistinguished_name=dn\n[dn]\n")
        subprocess.run([openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-config", str(root / "openssl.cnf"),
            "-keyout", str(root / "key.pem"), "-out", str(root / "cert.pem"), "-subj", "/CN=data.example.com", "-addext", "subjectAltName=DNS:data.example.com"],
            check=True, capture_output=True)
        for engine in engines:
            name = engine["engine"]
            print("Checking public TLS: " + name, flush=True)
            resource = create({"name": "TLS " + engine["name"], "kind": "database", "database": {
                "engine": name, "version": engine["version"], "template_revision": engine["template_revision"]}})
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            def setting(enabled):
                metadata = api("GET", f"/api/workloads/{resource}/database")
                request = {"enabled": enabled, "confirm": True, "revision": metadata["revision"]}
                if enabled:
                    request.update(hostname="data.example.com", bind_address="127.0.0.1", port=port,
                        certificate=(root / "cert.pem").read_text(), private_key=(root / "key.pem").read_text())
                task = api("POST", f"/api/workloads/{resource}/database/public-access", request, 202)
                await_task(api, task["id"])
            setting(True)
            credentials = api("GET", f"/api/workloads/{resource}/database/credentials")
            if name == "postgresql":
                query = 'PGPASSWORD="$COVE_PASSWORD" PGSSLMODE=verify-full PGSSLROOTCERT=/run/secrets/cove_db_ca psql -h data.example.com -U app -d app -w -At -c "SELECT 1"'
            elif name == "mysql":
                query = 'MYSQL_PWD="$COVE_PASSWORD" mysql -h data.example.com --ssl-mode=VERIFY_IDENTITY --ssl-ca=/run/secrets/cove_db_ca -u app -sN -e "SELECT 1"'
            elif name == "mariadb":
                query = 'MYSQL_PWD="$COVE_PASSWORD" mariadb -h data.example.com --ssl --ssl-verify-server-cert --ssl-ca=/run/secrets/cove_db_ca -u app -sN -e "SELECT 1"'
            elif name == "mongodb":
                query = 'mongosh --tls --tlsCAFile /run/secrets/cove_db_ca --host data.example.com --quiet --eval \'if(db.getSiblingDB("admin").auth("app",process.env.COVE_PASSWORD).ok!==1)quit(1);print("1")\''
            elif name in ("redis", "valkey"):
                command = "redis-cli" if name == "redis" else "valkey-cli"
                query = 'REDISCLI_AUTH="$COVE_PASSWORD" ' + command + ' --tls --cacert /run/secrets/cove_db_ca --sni data.example.com -h data.example.com -p 6380 --no-auth-warning ping | grep -x PONG'
            else:
                query = '''printf '%s' '<clickhouse><openSSL><client><caConfig>/run/secrets/cove_db_ca</caConfig><verificationMode>strict</verificationMode><extendedVerification>true</extendedVerification><loadDefaultCAFile>false</loadDefaultCAFile></client></openSSL></clickhouse>' > /tmp/cove-client.xml; clickhouse-client --config-file /tmp/cove-client.xml --secure --host data.example.com --port 9440 --user app --password "$COVE_PASSWORD" --query 'SELECT 1' '''
            native(resource, name, engine["service"], credentials["password"], query)
            verify_public_tls(name, port, root / "cert.pem")
            if name == "postgresql":
                plain = 'PGPASSWORD="$COVE_PASSWORD" PGSSLMODE=disable psql -h data.example.com -U app -d app -w -At -c "SELECT 1"'
            elif name == "mysql":
                plain = 'MYSQL_PWD="$COVE_PASSWORD" mysql -h data.example.com --ssl-mode=DISABLED -u app -sN -e "SELECT 1"'
            elif name == "mariadb":
                plain = 'MYSQL_PWD="$COVE_PASSWORD" mariadb -h data.example.com --skip-ssl -u app -sN -e "SELECT 1"'
            elif name == "mongodb":
                plain = 'mongosh --host data.example.com --quiet --eval \'if(db.getSiblingDB("admin").auth("app",process.env.COVE_PASSWORD).ok!==1)quit(1)\''
            elif name in ("redis", "valkey"):
                command = "redis-cli" if name == "redis" else "valkey-cli"
                plain = 'REDISCLI_AUTH="$COVE_PASSWORD" ' + command + ' -h data.example.com -p 6380 --no-auth-warning ping | grep -x PONG'
            else:
                plain = 'clickhouse-client --host data.example.com --port 9440 --user app --password "$COVE_PASSWORD" --query "SELECT 1"'
            native(resource, name, engine["service"], credentials["password"], plain, accepted=False)
            if name in ("postgresql", "mongodb"):
                test_bound_database_ca(api, create, operation, container, engine, resource, credentials["host"])
            maintenance(resource, "rotate")
            rotated = api("GET", f"/api/workloads/{resource}/database/credentials")
            native(resource, name, engine["service"], rotated["password"], query)
            native(resource, name, engine["service"], credentials["password"], query, accepted=False)
            setting(False)
            exposed = json.loads(docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", container(resource, engine["service"])).stdout)
            if any(value for value in (exposed or {}).values()):
                raise RuntimeError("Disabled TLS access left a published port")
            operation(resource, "stop")
            api("DELETE", f"/api/workloads/{resource}")
            print("PASS TLS published handshake/CA verification, authenticated query, rotation and private recovery: " + name, flush=True)


def test_bound_database_ca(api, create, operation, container, engine, resource, hostname):
    if engine["engine"] == "postgresql":
        query = 'psql "$$DATABASE_URL" -w -At -c "SELECT 1"'
    else:
        query = 'mongosh "$$DATABASE_URL" --quiet --eval \'print(db.adminCommand({ping:1}).ok)\''
    command = 'for attempt in $$(seq 1 60); do '+query+' > /tmp/cove-tls-bound && break; sleep 1; done; test "$$(cat /tmp/cove-tls-bound)" = 1; sleep 600'
    compose = json.dumps({"services": {"app": {"image": engine["image"], "environment": {"DATABASE_URL": "${DATABASE_URL}"}, "command": ["sh", "-ec", command]}}})
    consumer = create({"name": "Bound TLS consumer", "compose": compose, "environment": {"DATABASE_URL": "{{resource.DATABASE}}"}})
    api("PUT", f"/api/workloads/{consumer}/variable-bindings", {"name": "DATABASE", "source_id": resource, "source_key": "DB_URL"})
    connection = api("POST", "/api/private-connections", {"source_id": consumer, "source_service": "app", "target_id": resource,
        "target_service": engine["service"], "port": engine["port"], "alias": hostname}, 201)["id"]
    operation(consumer)
    if docker("exec", container(consumer, "app"), "cat", "/tmp/cove-tls-bound").stdout.strip() != "1":
        raise RuntimeError("Bound consumer did not verify the delivered database CA")
    api("DELETE", "/api/private-connections/" + connection, status=202)
    operation(consumer, "stop")
    api("DELETE", f"/api/workloads/{consumer}")
    print("PASS bound consumer verifies automatically delivered CA: " + engine["engine"], flush=True)


def await_task(api, task):
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        operation = api("GET", "/api/operations/" + task)
        if operation["state"] == "succeeded":
            return
        if operation["state"] in ("failed", "cancelled", "interrupted"):
            raise RuntimeError("Data configuration failed: " + safe_detail(api("GET", "/api/operations/" + task + "/logs")["logs"][-4000:]))
        time.sleep(1)
    raise RuntimeError("Data configuration timed out")


def verify_public_tls(engine, port, ca):
    def handshake(context):
        connection = socket.create_connection(("127.0.0.1", port), timeout=5)
        try:
            if engine == "postgresql":
                connection.sendall(struct.pack("!II", 8, 80877103))
                if connection.recv(1) != b"S":
                    raise RuntimeError("PostgreSQL refused TLS negotiation")
            elif engine in ("mysql", "mariadb"):
                header = connection.recv(4)
                if len(header) != 4:
                    raise RuntimeError("SQL TLS handshake missing")
                remaining = int.from_bytes(header[:3], "little")
                while remaining:
                    data = connection.recv(remaining)
                    if not data:
                        raise RuntimeError("SQL handshake was truncated")
                    remaining -= len(data)
                request = struct.pack("<IIB23s", 0x200 | 0x800 | 0x8000, 16777216, 45, b"\0" * 23)
                connection.sendall(len(request).to_bytes(3, "little") + b"\x01" + request)
            with context.wrap_socket(connection, server_hostname="data.example.com"):
                pass
        finally:
            connection.close()
    handshake(ssl.create_default_context(cafile=str(ca)))
    try:
        handshake(ssl.create_default_context())
    except ssl.SSLCertVerificationError:
        pass
    else:
        raise RuntimeError("Untrusted TLS certificate was accepted")


def test_pooler(api, create, operation, native, container, engines):
    engine = next(item for item in engines if item["engine"] == "postgresql")
    resource = create({"name": "Pooled PostgreSQL", "kind": "database", "database": {
        "engine": "postgresql", "version": engine["version"], "template_revision": engine["template_revision"]}})
    operation(resource)
    def change(enabled):
        metadata = api("GET", f"/api/workloads/{resource}/database")
        task = api("POST", f"/api/workloads/{resource}/database/pooler", {
            "enabled": enabled, "confirm": True, "revision": metadata["revision"]}, 202)
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            current = api("GET", "/api/operations/" + task["id"])
            if current["state"] == "succeeded":
                return
            if current["state"] == "failed":
                raise RuntimeError("Pooler configuration failed: " + safe_detail(api("GET", "/api/operations/" + task["id"] + "/logs")["logs"][-3000:]))
            time.sleep(1)
        raise RuntimeError("Pooler configuration timed out")
    change(True)
    credentials = api("GET", f"/api/workloads/{resource}/database/credentials")
    query = 'PGPASSWORD="$COVE_PASSWORD" psql -h pgbouncer -p 6432 -U app -d app -w -At -c "SELECT 1"'
    native(resource, "postgresql", "postgres", credentials["password"], query)
    native(resource, "postgresql", "postgres", secrets.token_hex(32), query, accepted=False)
    exposed = json.loads(docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", container(resource, "pgbouncer")).stdout)
    if any(value for value in (exposed or {}).values()):
        raise RuntimeError("Private pooler published a host port")
    metadata = api("GET", f"/api/workloads/{resource}/database")
    task = api("POST", f"/api/workloads/{resource}/database/maintenance", {
        "kind": "rotate", "revision": metadata["revision"], "confirm": True}, 202)
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        state = api("GET", "/api/operations/" + task["id"])["state"]
        if state == "succeeded":
            break
        if state == "failed":
            raise RuntimeError("Pooled password rotation failed: " + safe_detail(api("GET", "/api/operations/" + task["id"] + "/logs")["logs"][-3000:]))
        time.sleep(1)
    else:
        raise RuntimeError("Pooled password rotation timed out")
    updated = api("GET", f"/api/workloads/{resource}/database/credentials")
    native(resource, "postgresql", "postgres", updated["password"], query)
    native(resource, "postgresql", "postgres", credentials["password"], query, accepted=False)
    with tempfile.TemporaryDirectory(prefix="cove-pooler-tls-") as directory:
        root=Path(directory)
        (root/"openssl.cnf").write_text("[req]\ndistinguished_name=dn\n[dn]\n")
        openssl=shutil.which("openssl") or "C:/Program Files/Git/usr/bin/openssl.exe"
        subprocess.run([openssl,"req","-x509","-newkey","rsa:2048","-nodes","-days","2","-config",str(root/"openssl.cnf"),"-keyout",str(root/"key.pem"),"-out",str(root/"cert.pem"),"-subj","/CN=data.example.com","-addext","subjectAltName=DNS:data.example.com"],check=True,capture_output=True)
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1",0))
            public_port=reservation.getsockname()[1]
        metadata=api("GET",f"/api/workloads/{resource}/database")
        task=api("POST",f"/api/workloads/{resource}/database/public-access",{"enabled":True,"revision":metadata["revision"],"confirm":True,"hostname":"data.example.com","bind_address":"127.0.0.1","port":public_port,"certificate":(root/"cert.pem").read_text(),"private_key":(root/"key.pem").read_text()},202)
        await_task(api,task["id"])
        secure_query='PGPASSWORD="$COVE_PASSWORD" psql -h pgbouncer -p 6432 -U app -d app -w -At -c "SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()"'
        if native(resource,"postgresql","postgres",updated["password"],secure_query)!="t":raise RuntimeError("PgBouncer upstream is not using verified native TLS")
        metadata=api("GET",f"/api/workloads/{resource}/database")
        task=api("POST",f"/api/workloads/{resource}/database/public-access",{"enabled":False,"revision":metadata["revision"],"confirm":True},202)
        await_task(api,task["id"])
        native(resource,"postgresql","postgres",updated["password"],query)
    print("PASS PgBouncer upstream verified TLS and private recovery",flush=True)
    change(False)
    remaining = docker("ps", "-aq", "--filter", "label=com.docker.compose.project=cove-" + resource,
                       "--filter", "label=com.docker.compose.service=pgbouncer").stdout.strip()
    if remaining:
        raise RuntimeError("Disabled pooler container remains")
    print("PASS real PgBouncer SCRAM query, password rotation, wrong-password rejection, private exposure and removal", flush=True)
    operation(resource,"stop")
    api("DELETE",f"/api/workloads/{resource}")


def test_buckets(api, create, operation, container, worker):
    catalogue = api("GET", "/api/templates")
    # API catalogue entries include origin/revision alongside the format definition.
    entries = catalogue["templates"]
    entry = next(t for t in entries if t["template"]["id"] == "garage" and t["origin"] == "builtin")
    draft = api("POST", "/api/templates/draft", {"id": "garage", "origin": "builtin", "revision": entry["revision"]}, 200)
    storage = create({"name": "Scoped Garage", **draft, "routes": [{"domain": "garage.localhost", "service": "garage", "port": 3900}]})
    operation(storage)
    target = create({"name": "Object consumer", "compose": 'services:\n  app:\n    image: busybox:1.37\n    environment:\n      S3_ENDPOINT: ${S3_ENDPOINT}\n      S3_BUCKET: ${S3_BUCKET}\n      S3_ACCESS_KEY_ID: ${S3_ACCESS_KEY_ID}\n      S3_SECRET_ACCESS_KEY: ${S3_SECRET_ACCESS_KEY}\n      S3_REGION: ${S3_REGION}\n    volumes: [data:/data]\n    command: [sleep, "600"]\nvolumes:\n  data:\n'})
    network = "cove-" + storage + "_default"
    docker("network", "connect", network, worker)
    tls_container=worker.removesuffix("-worker")+"-tls"
    routes_file=Path(os.environ["COVE_BROWSER_CA"]).parent/"routes.yaml"
    original_routes=routes_file.read_text()
    docker("network","connect",network,tls_container)
    try:
        path = f"/api/workloads/{target}/object-buckets"
        api("POST", path, {"storage_resource_id": storage, "domain": "garage.localhost", "confirm_same_node": True, "backup_destination": True}, 201)
        bucket = api("GET", path)[0]
        operation(target)
        environment = json.loads(docker("inspect", "--format", "{{json .Config.Env}}", container(target, "app")).stdout)
        values = dict(v.split("=", 1) for v in environment)
        if values["S3_BUCKET"] != bucket["bucket"] or values["S3_ENDPOINT"] != bucket["endpoint"]:
            raise RuntimeError("Bucket bindings did not reach the real consumer")
        location = next(item for item in api("GET", "/api/storage-locations") if item["id"] == bucket["storage_id"])
        if "not offsite" not in location.get("warning", ""):
            raise RuntimeError("S3 destination omits same-node warning")
        routes=json.loads(original_routes)
        routes["http"]["routers"]["s3"]={"rule":"Host(`localhost`) && PathPrefix(`/"+bucket["bucket"]+"`)","priority":200,"entryPoints":["websecure"],"service":"s3","tls":{}}
        routes["http"]["services"]["s3"]={"loadBalancer":{"servers":[{"url":"http://garage:3900"}]}}
        routes_file.write_text(json.dumps(routes))
        # Docker Desktop bind mounts do not reliably notify the file provider.
        docker("restart",tls_container)
        tls_context=ssl.create_default_context(cafile=os.environ["COVE_BROWSER_CA"])
        for _ in range(60):
            try:
                urllib.request.urlopen(os.environ["COVE_BROWSER_URL"]+"/"+bucket["bucket"],context=tls_context,timeout=2).close()
            except urllib.error.HTTPError as error:
                if error.code in (400,403):break
            except (OSError,urllib.error.URLError):
                pass
            time.sleep(.2)
        else:raise RuntimeError("Owned HTTPS fixture did not route S3 to Garage")
        location_config={"name":location["name"],"storage":{"protocol":"s3","endpoint":os.environ["COVE_BROWSER_URL"],"path":bucket["bucket"],"region":"garage","access_key":values["S3_ACCESS_KEY_ID"],"secret_key":values["S3_SECRET_ACCESS_KEY"],"ca_certificate":Path(os.environ["COVE_BROWSER_CA"]).read_text()}}
        api("PUT","/api/storage-locations/"+location["id"],location_config)
        policy = {"storage_id": location["id"], "repository": "storage:" + location["id"], "password": secrets.token_hex(32), "keep_last": 2, "interval_hours": 0}
        api("PUT", f"/api/workloads/{target}/backup-policy", {"backup": policy})
        if api("GET", f"/api/workloads/{target}/backup-policy")["backup"]["storage_id"] != location["id"]:
            raise RuntimeError("S3 backup destination was not selectable")
        scoped_s3(network, values)
        docker("exec",container(target,"app"),"sh","-ec","printf verified-object-backup > /data/probe")
        time.sleep(3)
        try:
            operation(target,"backup")
        except RuntimeError:
            remote={"RCLONE_CONFIG":"/dev/null","RCLONE_CONFIG_COVE_TYPE":"s3","RCLONE_CONFIG_COVE_PROVIDER":"Other","RCLONE_CONFIG_COVE_NO_CHECK_BUCKET":"true","RCLONE_CONFIG_COVE_ENDPOINT":os.environ["COVE_BROWSER_URL"],"RCLONE_CONFIG_COVE_ACCESS_KEY_ID":values["S3_ACCESS_KEY_ID"],"RCLONE_CONFIG_COVE_SECRET_ACCESS_KEY":values["S3_SECRET_ACCESS_KEY"],"RCLONE_CONFIG_COVE_REGION":"garage","RCLONE_CA_CERT":"/storage-ca.pem"}
            options=[arg for key in remote for arg in ("-e",key)]
            diagnostic=docker("run","--rm","--network","host","--mount","type=bind,source="+os.environ["COVE_BROWSER_CA"]+",target=/storage-ca.pem,readonly",*options,"cove-backup-remote:restic0.19.1-rclone1.76.0","rclone","lsf","cove:"+bucket["bucket"],"--max-depth","1","--contimeout","10s","--timeout","15s","--retries","1",env=os.environ|remote,check=False)
            detail=diagnostic.stderr[-3000:]
            for key in ("S3_ACCESS_KEY_ID","S3_SECRET_ACCESS_KEY"):
                detail=detail.replace(values[key],"[redacted]")
            print("Garage storage diagnostic: "+safe_detail(detail),flush=True)
            raise
        backups=api("GET",f"/api/workloads/{target}/backups").get("backups",[])
        if not backups: raise RuntimeError("Encrypted Garage backup not listed")
        backup_id=backups[0]["id"]
        copy=api("POST",f"/api/workloads/{target}/restore-copy",{"name":"Garage verified recovery","backup_id":backup_id},202)
        await_task(api,copy["id"])
        operation(copy["resource_id"])
        if docker("exec",container(copy["resource_id"],"app"),"cat","/data/probe").stdout!="verified-object-backup":raise RuntimeError("Garage HTTPS backup recovery lost data")
        operation(copy["resource_id"],"stop")
        api("DELETE",f"/api/workloads/{copy['resource_id']}")
        print("PASS real CA-verified HTTPS Garage encrypted backup and separate-copy exact recovery",flush=True)
        api("DELETE", path + "/" + bucket["id"], {"confirm": True}, 409)
        api("PUT", f"/api/workloads/{target}/backup-policy", {"backup": None})
        api("DELETE", path + "/" + bucket["id"], {"confirm": True})
        scoped_s3(network, values, revoked=True)
        retained = json.loads(docker("exec", container(storage, "garage"), "/garage", "json-api", "GetBucketInfo", json.dumps({"globalAlias": bucket["bucket"]})).stdout)
        if not retained.get("id"):
            raise RuntimeError("Key revocation deleted bucket data")
        print("PASS Garage scoped S3 put/get/isolation, binding, backup selection and key revocation with retained bucket", flush=True)
    finally:
        routes_file.write_text(original_routes)
        docker("network","disconnect",network,tls_container)
        docker("network", "disconnect", network, worker)
        docker("restart",tls_container)
        context=ssl.create_default_context(cafile=os.environ["COVE_BROWSER_CA"])
        for _ in range(60):
            try:
                with urllib.request.urlopen(os.environ["COVE_BROWSER_URL"]+"/healthz",context=context,timeout=2) as response:
                    if response.status==200:break
            except (OSError,urllib.error.URLError):
                pass
            time.sleep(.2)
        else:raise RuntimeError("Owned HTTPS fixture did not recover after detaching Garage")


def scoped_s3(network, values, revoked=False):
    script = r'''
import datetime, hashlib, hmac, os, urllib.request, urllib.error
def call(method,path,data=b''):
 now=datetime.datetime.now(datetime.timezone.utc);date=now.strftime('%Y%m%d');stamp=now.strftime('%Y%m%dT%H%M%SZ')
 payload=hashlib.sha256(data).hexdigest();lf=chr(10);headers=lf.join(['host:garage:3900','x-amz-content-sha256:'+payload,'x-amz-date:'+stamp,''])
 signed='host;x-amz-content-sha256;x-amz-date';canonical=lf.join([method,path,'',headers,signed,payload]);scope=date+'/garage/s3/aws4_request'
 key=('AWS4'+os.environ['S3_SECRET_ACCESS_KEY']).encode()
 for part in [date,'garage','s3','aws4_request']:key=hmac.new(key,part.encode(),hashlib.sha256).digest()
 signature=hmac.new(key,lf.join(['AWS4-HMAC-SHA256',stamp,scope,hashlib.sha256(canonical.encode()).hexdigest()]).encode(),hashlib.sha256).hexdigest()
 auth='AWS4-HMAC-SHA256 Credential='+os.environ['S3_ACCESS_KEY_ID']+'/'+scope+', SignedHeaders='+signed+', Signature='+signature
 request=urllib.request.Request('http://garage:3900'+path,data=data,method=method,headers={'Authorization':auth,'x-amz-date':stamp,'x-amz-content-sha256':payload})
 try:
  with urllib.request.urlopen(request,timeout=10) as response:return response.status,response.read()
 except urllib.error.HTTPError as error:return error.code,b''
path='/'+os.environ['S3_BUCKET']+'/cove-probe'
if os.environ['COVE_REVOKED']=='1':assert call('GET',path)[0]==403
else:
 assert call('PUT',path,b'cove-probe')[0]==200
 assert call('GET',path)==(200,b'cove-probe')
 assert call('GET','/data/')[0] in (403,404)
 assert call('PUT','/cove-forbidden-bucket')[0]==403
'''
    options = [v for key in ("S3_SECRET_ACCESS_KEY", "S3_ACCESS_KEY_ID", "S3_BUCKET", "COVE_REVOKED") for v in ("-e", key)]
    docker("run", "--rm", "-i", "--network", network, *options, "python:3.12-alpine", "python", "-",
           env=os.environ | values | {"COVE_REVOKED": "1" if revoked else "0"}, input=script)


if __name__ == "__main__":
    main()
