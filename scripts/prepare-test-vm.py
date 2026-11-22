"""Provision an isolated QEMU/systemd acceptance VM; requires QEMU and pycdlib."""
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import urllib.request

import pycdlib


def main():
    root = Path.home() / ".cove-acceptance" / "vm"
    root.mkdir(parents=True, exist_ok=True)
    state = root / "instance.json"
    if state.exists():
        raise SystemExit("A VM record already exists; inspect it before creating another instance.")
    qemu = Path.home() / "scoop/apps/qemu/current/qemu-system-x86_64.exe"
    image_tool = qemu.with_name("qemu-img.exe")
    if not qemu.exists():
        raise SystemExit("Install QEMU before preparing the test VM.")
    base = root / "ubuntu-noble.img"
    url = "https://cloud-images.ubuntu.com/noble/current/"
    filename = "noble-server-cloudimg-amd64.img"
    sums = urllib.request.urlopen(url + "SHA256SUMS", timeout=60).read().decode()
    expected = next(line.split()[0] for line in sums.splitlines() if line.split()[-1].lstrip("*") == filename)
    if not base.exists():
        print("Downloading Ubuntu 24.04 cloud image", flush=True)
        partial = base.with_suffix(".partial")
        with urllib.request.urlopen(url + filename, timeout=60) as response, partial.open("wb") as output:
            shutil.copyfileobj(response, output)
        partial.replace(base)
    with base.open("rb") as image:
        if hashlib.file_digest(image, "sha256").hexdigest() != expected:
            raise SystemExit("Ubuntu image checksum mismatch; refusing to boot.")
    key = root / "id_ed25519"
    if not key.exists():
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)], check=True)
    public_key = key.with_suffix(".pub").read_text().strip()
    user_data = ("#cloud-config\nhostname: cove-acceptance\nusers:\n"
                 "  - name: cove-test\n    groups: [sudo]\n    shell: /bin/bash\n"
                 "    sudo: ALL=(ALL) NOPASSWD:ALL\n    lock_passwd: true\n"
                 f"    ssh_authorized_keys:\n      - {public_key}\n"
                 "ssh_pwauth: false\ndisable_root: true\n")
    iso = pycdlib.PyCdlib()
    iso.new(interchange_level=3, joliet=3, rock_ridge="1.09", vol_ident="cidata")
    for name, content in [("user-data", user_data), ("meta-data", "instance-id: cove-acceptance-1\nlocal-hostname: cove-acceptance\n")]:
        data = content.encode()
        iso.add_fp(io.BytesIO(data), len(data), iso_path="/" + name.upper().replace("-", "_") + ";1", rr_name=name, joliet_path="/"+name)
    seed = root / "seed.iso"
    iso.write(str(seed))
    iso.close()
    disk = root / "disk.qcow2"
    if disk.exists():
        raise SystemExit("VM disk already exists; refusing to overwrite.")
    subprocess.run([str(image_tool), "create", "-f", "qcow2", "-F", "qcow2", "-b", str(base), str(disk), "24G"], check=True)
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    args = [str(qemu), "-name", "cove-acceptance", "-machine", "q35", "-accel", "tcg,thread=multi",
            "-m", "4096", "-smp", "2", "-display", "none", "-monitor", "none",
            "-serial", "file:"+str(root / "serial.log"),
            "-drive", f"file={disk},if=virtio,format=qcow2", "-drive", f"file={seed},media=cdrom,readonly=on",
            "-netdev", f"user,id=net0,hostfwd=tcp:127.0.0.1:{port}-:22", "-device", "virtio-net-pci,netdev=net0"]
    with (root / "qemu.log").open("wb") as log:
        process = subprocess.Popen(args, stdout=log, stderr=log, creationflags=subprocess.CREATE_NO_WINDOW)
    state.write_text(json.dumps({"pid": process.pid, "ssh_port": port, "root": str(root), "executable": str(qemu)}, indent=2))
    print(f"Started isolated VM PID {process.pid}; SSH on 127.0.0.1:{port}; state: {state}", flush=True)


if __name__ == "__main__":
    main()
