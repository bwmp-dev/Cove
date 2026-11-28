"""Run rendered worker installation against private host-service fixtures."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
WORKER = "#!" + sys.executable + "\nimport json,sys\nr=json.load(sys.stdin)\nprint(json.dumps({'probe':{'ready':True},'maintenance':{'draining':r['method']=='drain','active':0}}))\n"
HELPER = r'''
import json,os,pathlib,shutil,subprocess,sys
root=pathlib.Path(os.environ['FIXTURE']);name=pathlib.Path(sys.argv[0]).name;args=sys.argv[1:]
with (root/'calls').open('a') as out: out.write(json.dumps([name]+args)+'\n')
if name=='id':
 if args==['-u']:print('0')
 else:sys.exit(0 if (root/'user-created').exists() else 1)
elif name=='uname': print('Linux' if args==['-s'] else os.environ['ARCH'])
elif name=='cosign':
 assert args[0]=='verify-blob'
 assert args[args.index('--certificate-identity')+1]=='https://github.com/bwmp-dev/Cove/.github/workflows/images.yml@refs/heads/main'
 assert args[args.index('--certificate-oidc-issuer')+1]=='https://token.actions.githubusercontent.com'
 assert pathlib.Path(args[args.index('--bundle')+1]).is_file()
 sys.exit(1 if os.environ.get('BAD_SIGNATURE') else 0)
elif name in ['apt-get','dnf']:
 if 'docker-ce' in args: shutil.copy(root/'helper',root/'bin/docker')
 if '--add-repo' in args:(root/'repos/docker-ce.repo').write_text('fixture signed Docker repository')
elif name=='dpkg':print('arm64' if os.environ['ARCH']=='aarch64' else 'amd64')
elif name=='getent':sys.exit(1)
elif name=='useradd':(root/'user-created').touch()
elif name in ['usermod','groupadd','semanage','restorecon']:pass
elif name=='selinuxenabled':sys.exit(0 if os.environ['DISTRO'] in ['rhel','almalinux','rocky'] else 1)
elif name=='matchpathcon':print('system_u:object_r:var_lib_t:s0')
elif name=='ssh-keygen':(root/'ssh/ssh_host_ed25519_key.pub').write_text('ssh-ed25519 synthetic-public-fixture\n')
elif name=='systemctl':
 unit=args[-1]
 if args[0]=='is-active':sys.exit(0 if (root/('active-'+unit)).exists() else 1)
 if args[0] in ['start','enable']:(root/('active-'+unit)).touch()
 if args[0]=='stop':(root/('active-'+unit)).unlink(missing_ok=True)
elif name=='install':
 clean=[];index=0
 while index<len(args):
  if args[index] in ['-o','-g']:index+=2
  else:clean.append(args[index]);index+=1
 sys.exit(subprocess.run(['/usr/bin/install',*clean]).returncode)
elif name=='curl':
 url=next(a for a in args if a.startswith('https://'))
 if '-o' in args:
  target=pathlib.Path(args[args.index('-o')+1])
  target.write_bytes((root/'download-worker').read_bytes() if '/bootstrap/worker/' in url else b'fixture public signing key')
 else:
  payload=pathlib.Path(args[args.index('--data-binary')+1][1:])
  assert json.loads(payload.read_text())['host_key'].startswith('ssh-ed25519 ')
  print('{"ready":true}')
elif name=='jq':
 if '-n' in args:print(json.dumps({'host_key':args[args.index('--arg')+2]}))
 else:
  d=json.loads(pathlib.Path(args[-1]).read_text())
  assert d.get('probe',{}).get('ready') or d.get('maintenance',{}).get('active')==0
elif name!='docker':raise RuntimeError(name)
'''


@unittest.skipUnless(os.name == "posix", "Requires a disposable Linux shell")
class WorkerInstallerTests(unittest.TestCase):
    def exercise(self, distro, arch="x86_64", existing_docker=False, unmanaged=False, bad_signature=False):
        with tempfile.TemporaryDirectory(prefix="cove-worker-installer-") as directory:
            root = Path(directory)
            for name in ["bin", "state", "ssh", "units", "run", "config", "sbin", "apt/keyrings", "apt/sources.list.d", "repos"]:
                (root / name).mkdir(parents=True, exist_ok=True)
            (root / "os-release").write_text(f"ID={distro}\nVERSION_ID=9.6\nVERSION_CODENAME=bookworm\n")
            (root / "download-worker").write_text(WORKER)
            (root / "helper").write_text("#!" + sys.executable + "\n" + HELPER)
            (root / "helper").chmod(0o755)
            commands = ["id", "uname", "getent", "useradd", "usermod", "groupadd", "cosign", "curl", "git", "jq", "sshd", "ssh-keygen", "systemctl", "install", "apt-get", "dpkg", "dnf", "selinuxenabled", "matchpathcon", "semanage", "restorecon"]
            for command in commands:
                shutil.copy(root / "helper", root / "bin" / command)
            if existing_docker:
                shutil.copy(root / "helper", root / "bin/docker")
                (root / "active-docker").touch()
            if unmanaged:
                (root / "user-created").touch()
            script = (ROOT / "internal/control/install.sh.tmpl").read_text()
            include = (ROOT / "internal/control/worker-update-install.sh.tmpl").read_text()
            include = include.replace('{{template "worker-update.sh.tmpl" .}}', (ROOT / "internal/control/worker-update.sh.tmpl").read_text())
            script = script.replace('{{template "worker-update-install.sh.tmpl" .}}', include)
            digest = hashlib.sha256(WORKER.encode()).hexdigest()
            for key, value in {"URL": "https://panel.example.com", "ID": "fixture-node", "Token": "synthetic-enrollment", "PublicKey": "ssh-ed25519 synthetic-panel-public", "AMD64": digest, "ARM64": digest}.items():
                script = script.replace("{{." + key + "}}", value)
            for source, destination in [("/usr/local/bin", "bin"), ("/usr/local/sbin", "sbin"), ("/etc/cove", "config"), ("/etc/ssh", "ssh"), ("/etc/systemd/system", "units"), ("/var/lib/cove", "state"), ("/run/cove-worker-update.lock", "run/update.lock"), ("/etc/os-release", "os-release"), ("/etc/apt", "apt"), ("/etc/yum.repos.d", "repos")]:
                script = script.replace(source, str(root / destination))
            self.assertNotIn("{{", script)
            result = subprocess.run(["sh"], input=script, text=True, capture_output=True, env={**os.environ, "FIXTURE": str(root), "ARCH": arch, "DISTRO": distro, "BAD_SIGNATURE": "1" if bad_signature else "", "TMPDIR": str(root), "PATH": str(root / "bin") + ":" + os.environ["PATH"]}, timeout=30)
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            if unmanaged:
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("unmanaged", result.stderr)
                self.assertFalse(any(call[0] in ["apt-get", "dnf", "useradd"] for call in calls))
                return
            if bad_signature:
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(any(call[0] == "cosign" for call in calls))
                self.assertFalse((root / "bin/cove-worker").exists())
                self.assertFalse(any(call[0] == "curl" and "--data-binary" in call for call in calls))
                return
            self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
            self.assertTrue(any(call[0] == "cosign" and call[1] == "verify-blob" for call in calls))
            self.assertEqual((root / "bin/cove-worker").read_text(), WORKER)
            self.assertEqual((root / "config/node-id").read_text().strip(), "fixture-node")
            self.assertIn('restrict,command="', (root / "state/.ssh/authorized_keys").read_text())
            self.assertIn("User=cove", (root / "units/cove-worker.service").read_text())
            self.assertTrue((root / "units/cove-worker-update.timer").is_file())
            expected_arch = "arm64" if arch == "aarch64" else "amd64"
            self.assertTrue(any(call[0] == "curl" and "https://panel.example.com/bootstrap/worker/" + expected_arch in call for call in calls))
            package_calls = [call for call in calls if call[0] in ["apt-get", "dnf"]]
            if existing_docker:
                self.assertEqual(package_calls, [])
                self.assertFalse(any(call[0] == "systemctl" and "restart" in call and "docker" in call for call in calls))
            elif distro in ["ubuntu", "debian"]:
                self.assertTrue(any(call[0] == "apt-get" and "docker-compose-plugin" in call for call in package_calls))
            else:
                source = "rhel" if distro == "rhel" else "centos"
                self.assertIn(["dnf", "config-manager", "--add-repo", f"https://download.docker.com/linux/{source}/docker-ce.repo"], calls)
                self.assertTrue(any(call[0] == "semanage" and "ssh_home_t" in call for call in calls))
                self.assertTrue(any(call[0] == "restorecon" for call in calls))
            self.assertFalse(any("--nogpgcheck" in call or "setenforce" in call or "remove" in call for call in calls))

    def test_apt_distributions_on_both_architectures(self):
        for distro in ["ubuntu", "debian"]:
            for arch in ["x86_64", "aarch64"]:
                with self.subTest(distro=distro, arch=arch): self.exercise(distro, arch)

    def test_rpm_distributions_on_both_architectures(self):
        for distro in ["rhel", "almalinux", "rocky"]:
            for arch in ["x86_64", "aarch64"]:
                with self.subTest(distro=distro, arch=arch): self.exercise(distro, arch)

    def test_signature_failure_prevents_worker_install_and_enrollment(self):
        self.exercise("rocky", existing_docker=True, bad_signature=True)

    def test_existing_docker_and_unmanaged_account_are_preserved(self):
        self.exercise("rocky", existing_docker=True)
        self.exercise("debian", unmanaged=True)


if __name__ == "__main__":
    unittest.main()
