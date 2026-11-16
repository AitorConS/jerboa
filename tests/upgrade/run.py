#!/usr/bin/env python3
"""Run the upgrade end-to-end test (tests/upgrade/guest.sh) in a machine made for it.

guest.sh installs Jerboa system-wide with the public installer, so it never
runs on the host. This script gives it a disposable machine and takes it away
again; everything it writes on the host is inside --work.

  --machine vm      (default) a QEMU virtual machine booted from the pinned
                    Ubuntu cloud image, with KVM when /dev/kvm is usable and
                    the host CPU passed through, so that the Jerboa inside
                    runs Firecracker where nested virtualization is on and
                    QEMU where it is not. Needs qemu-system-x86_64, ssh and
                    ssh-keygen. Its disk changes go to a temporary overlay.
  --machine docker  a privileged systemd container, for hosts without KVM
                    (a developer's Mac). No /dev/kvm inside: QEMU only.

Usage:
  make build VERSION=v0.60.0-ci     # or the binaries of a release candidate
  python3 tests/upgrade/run.py --jerboa dist/jerboa --jerboad dist/jerboad \
      --version v0.60.0-ci --work "$RUNNER_TEMP/upgrade-e2e"

The binaries must be linux/amd64. Exit status 0 means guest.sh printed PASS.
"""
import argparse
import hashlib
import http.server
import os
import shutil
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
# Pinned: a dated build that is not replaced, checked before it is booted.
IMAGE_URL = 'https://cloud-images.ubuntu.com/releases/noble/release-20261017/ubuntu-24.04-server-cloudimg-amd64.img'
IMAGE_SHA256 = '04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94'
DOCKER_IMAGE = 'jerboa-upgrade-e2e'
DOCKERFILE = '''FROM ubuntu:24.04
RUN apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \\
    systemd systemd-sysv dbus sudo curl ca-certificates jq openssl python3 iproute2 kmod procps \\
    && rm -rf /var/lib/apt/lists/*
CMD ["/sbin/init"]
'''


def log(message):
    print(f'==> {message}', flush=True)


def run(*command, **kwargs):
    return subprocess.run([str(c) for c in command], check=True, **kwargs)


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def is_linux_amd64(path):
    head = Path(path).read_bytes()[:20]
    return head[:4] == b'\x7fELF' and int.from_bytes(head[18:20], 'little') == 62


def stage_payload(args, work):
    """Everything guest.sh needs, in one directory."""
    payload = work / 'payload'
    (payload / 'voltest').mkdir(parents=True)
    for name, source in (('jerboa', args.jerboa), ('jerboad', args.jerboad)):
        if not is_linux_amd64(source):
            raise SystemExit(f'{source} is not a linux/amd64 binary')
        shutil.copyfile(source, payload / name)
    run('go', 'build', '-o', payload / 'vmstart', './tests/upgrade/vmstart', cwd=REPO,
        env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'})
    for name in ('jerboa', 'jerboad', 'vmstart'):
        (payload / name).chmod(0o755)
    shutil.copyfile(args.installer, payload / 'install.sh')
    shutil.copyfile(HERE / 'guest.sh', payload / 'guest.sh')
    shutil.copyfile(HERE / 'compose.yaml', payload / 'compose.yaml')
    for name in ('main.go', 'unikernel.toml'):
        shutil.copyfile(REPO / 'examples' / 'voltest' / name, payload / 'voltest' / name)
    (payload / 'voltest' / 'go.mod').write_text('module voltest\n\ngo 1.22\n')
    (payload / 'VERSION').write_text(args.version + '\n')
    return payload


def fetch_image(work):
    image = work / 'ubuntu.img'
    run('curl', '--proto', '=https', '--fail', '--silent', '--show-error', '--location', '--retry', '3',
        '--output', image, IMAGE_URL)
    h = hashlib.sha256()
    with open(image, 'rb') as f:
        while block := f.read(1 << 20):
            h.update(block)
    if h.hexdigest() != IMAGE_SHA256:
        raise SystemExit('the Ubuntu cloud image does not match its pinned checksum')
    return image


def console_tail(work):
    return (work / 'console.log').read_bytes()[-6000:].decode(errors='replace')


def in_vm(args, work, payload):
    for tool in ('qemu-system-x86_64', 'ssh', 'ssh-keygen', 'scp'):
        if not shutil.which(tool):
            raise SystemExit(f'{tool} is required for --machine vm')
    image = fetch_image(work)
    key = work / 'id_ed25519'
    run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', key)
    # cloud-init reads its configuration from this host over the VM's own
    # user-mode network: no ISO tool and no root needed.
    seed = work / 'seed'
    seed.mkdir()
    (seed / 'meta-data').write_text('instance-id: jerboa-upgrade-e2e\nlocal-hostname: upgrade-e2e\n')
    (seed / 'vendor-data').write_text('')
    (seed / 'user-data').write_text(
        '#cloud-config\ndisable_root: false\nssh_pwauth: false\nusers:\n  - name: root\n    ssh_authorized_keys:\n'
        f'      - {Path(str(key) + ".pub").read_text().strip()}\n')

    class Quiet(http.server.SimpleHTTPRequestHandler):
        def __init__(self, *a, **k):
            super().__init__(*a, directory=str(seed), **k)

        def log_message(self, *_):
            pass

    seed_server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Quiet)
    threading.Thread(target=seed_server.serve_forever, daemon=True).start()
    ssh_port = free_port()
    kvm = os.access('/dev/kvm', os.R_OK | os.W_OK)
    accel = ['-enable-kvm', '-cpu', 'host'] if kvm else ['-accel', 'tcg', '-cpu', 'max']
    if not kvm:
        log('no usable /dev/kvm: the machine is emulated, which is very slow')
    # Room for the packages the installer adds: an overlay larger than the
    # image, which the machine grows into at first boot. Without qemu-img
    # the image keeps its size and changes go to a temporary overlay.
    drive = f'file={image},if=virtio,format=qcow2,snapshot=on'
    if shutil.which('qemu-img'):
        disk = work / 'disk.qcow2'
        run('qemu-img', 'create', '-q', '-f', 'qcow2', '-b', image, '-F', 'qcow2', disk, '20G')
        drive = f'file={disk},if=virtio,format=qcow2'
    console = open(work / 'console.log', 'wb')
    qemu = subprocess.Popen(
        ['qemu-system-x86_64', *accel, '-smp', str(args.cpus), '-m', str(args.memory), '-display', 'none',
         '-serial', 'stdio', '-no-reboot',
         '-drive', drive,
         '-nic', f'user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:{ssh_port}-:22',
         '-smbios', f'type=1,serial=ds=nocloud;s=http://10.0.2.2:{seed_server.server_address[1]}/'],
        stdin=subprocess.DEVNULL, stdout=console, stderr=subprocess.STDOUT, env={**os.environ, 'TMPDIR': str(work)})
    ssh_options = ['-i', str(key), '-o', 'StrictHostKeyChecking=no', '-o', f'UserKnownHostsFile={work / "known_hosts"}',
                   '-o', 'LogLevel=ERROR', '-o', 'ConnectTimeout=5', '-o', 'ServerAliveInterval=15']
    ssh = ['ssh', *ssh_options, '-p', str(ssh_port), 'root@127.0.0.1']
    try:
        deadline = time.monotonic() + args.boot_timeout
        while True:
            if qemu.poll() is not None:
                raise SystemExit('the machine exited while booting:\n' + console_tail(work))
            if subprocess.run([*ssh, 'cloud-init status --wait >/dev/null 2>&1; true'], capture_output=True).returncode == 0:
                break
            if time.monotonic() > deadline:
                raise SystemExit(f'the machine did not accept SSH in {args.boot_timeout}s:\n' + console_tail(work))
            time.sleep(3)
        log('machine is up')
        run('scp', *ssh_options, '-P', ssh_port, '-q', '-r', payload, 'root@127.0.0.1:/root/payload')
        return subprocess.run([*ssh, 'bash /root/payload/guest.sh /root/payload'], timeout=args.timeout).returncode
    finally:
        qemu.kill()
        qemu.wait()
        seed_server.shutdown()
        console.close()


def in_docker(args, work, payload):
    run('docker', 'build', '-q', '--platform', 'linux/amd64', '-t', DOCKER_IMAGE, '-', input=DOCKERFILE.encode(),
        stdout=subprocess.DEVNULL)
    name = f'jerboa-upgrade-e2e-{os.getpid()}'
    run('docker', 'run', '-d', '--name', name, '--platform', 'linux/amd64', '--privileged', '--cgroupns=host',
        '-v', '/sys/fs/cgroup:/sys/fs/cgroup:rw', '--tmpfs', '/run', '--tmpfs', '/run/lock', DOCKER_IMAGE,
        stdout=subprocess.DEVNULL)
    try:
        for _ in range(60):
            state = subprocess.run(['docker', 'exec', name, 'systemctl', 'is-system-running'], capture_output=True, text=True).stdout.strip()
            if state in ('running', 'degraded'):
                break
            time.sleep(1)
        else:
            raise SystemExit('systemd did not come up in the container')
        run('docker', 'cp', '-q', payload, f'{name}:/root/payload')
        return subprocess.run(['docker', 'exec', name, 'bash', '/root/payload/guest.sh', '/root/payload'],
                              timeout=args.timeout).returncode
    finally:
        subprocess.run(['docker', 'rm', '-f', '-v', name], capture_output=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--jerboa', required=True, type=Path, help='linux/amd64 jerboa under test')
    parser.add_argument('--jerboad', required=True, type=Path, help='linux/amd64 jerboad under test')
    parser.add_argument('--version', required=True, help='version the binaries report, e.g. v0.60.0')
    parser.add_argument('--installer', type=Path, default=REPO / 'scripts' / 'install.sh', help='installer under test')
    parser.add_argument('--work', required=True, type=Path, help='new directory for everything this run writes')
    parser.add_argument('--machine', choices=('vm', 'docker'), default='vm')
    parser.add_argument('--cpus', type=int, default=4)
    parser.add_argument('--memory', type=int, default=6144, help='machine memory in MiB')
    parser.add_argument('--boot-timeout', type=int, default=600)
    parser.add_argument('--timeout', type=int, default=2400, help='seconds guest.sh may take')
    parser.add_argument('--keep', action='store_true', help='keep --work afterwards (console log, payload)')
    args = parser.parse_args()
    work = args.work.resolve()
    work.mkdir(parents=True, exist_ok=False)
    try:
        payload = stage_payload(args, work)
        log(f'upgrade test of {args.version} in a {args.machine}')
        status = (in_vm if args.machine == 'vm' else in_docker)(args, work, payload)
    finally:
        if not args.keep:
            shutil.rmtree(work, ignore_errors=True)
    if status != 0:
        raise SystemExit(f'upgrade test failed (exit {status})')


if __name__ == '__main__':
    main()
