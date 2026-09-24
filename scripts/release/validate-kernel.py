#!/usr/bin/env python3
"""Boot the staged Linux toolset on QEMU/KVM and Firecracker without rebuilding it."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess

import candidate


def boot(command, log, expected, success_codes=(0,)):
    with log.open('w') as stream:
        result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=stream,
                                stderr=subprocess.STDOUT, timeout=120)
    if result.returncode not in success_codes or expected not in log.read_text(errors='replace'):
        raise RuntimeError(f'Guest boot failed; see {log}')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--toolset', type=Path, default=Path('kernel-candidate'))
    parser.add_argument('--lock', type=Path, default=Path('release-lock.json'))
    parser.add_argument('--work', type=Path, required=True)
    parser.add_argument('--firecracker', default='firecracker')
    parser.add_argument('--qemu', default='qemu-system-x86_64')
    args = parser.parse_args()
    root = args.toolset.resolve()
    spec = json.loads(args.lock.read_text())
    record, _ = candidate.verify_kernel_toolset(root, spec)
    # A failed retry must never leave an earlier success marker in its output.
    (root / 'boot-validation.json').unlink(missing_ok=True)
    args.work.mkdir(parents=True, exist_ok=False)
    work = args.work.resolve()
    for name in ('mkfs-linux-amd64', 'dump-linux-amd64'):
        (root / name).chmod(0o755)  # artifact transport does not retain Unix modes
    fc = shutil.which(args.firecracker)
    qemu = shutil.which(args.qemu)
    if not fc or not qemu:
        raise RuntimeError('QEMU and Firecracker must be installed on the KVM runner')
    program = work / 'hello'
    subprocess.run(['cc', '-O2', '-static', 'tests/e2e/testdata/hello.c', '-o', program], check=True)
    manifest = f'(children:(program:(contents:(host:{json.dumps(str(program))}))) program:/program arguments:(0:/program) environment:())'
    for backend in ('qemu', 'firecracker'):
        disk = work / f'{backend}.img'
        command = [str(root / 'mkfs-linux-amd64'), '-s', '128M']
        command += ['-b', str(root / 'boot.img'), '-k', str(root / 'kernel.img')] if backend == 'qemu' else ['-c']
        subprocess.run([*command, str(disk)], input=manifest, text=True, check=True, capture_output=True)
        if backend == 'qemu':
            # ACPI shutdown exits 0; isa-debug-exit maps guest exit(0) to 1.
            command = [qemu, '-enable-kvm', '-m', '256', '-smp', '2', '-display', 'none',
                       '-serial', 'stdio', '-monitor', 'none', '-no-reboot',
                       '-drive', f'file={disk},format=raw,if=virtio',
                       '-device', 'isa-debug-exit,iobase=0x501,iosize=1']
            boot(command, work / 'qemu.log', 'hello from unikernel', (0, 1))
        else:
            config = {'boot-source': {'kernel_image_path': str(root / 'kernel-fc.img'),
                                     'boot_args': 'console=ttyS0 reboot=k panic=1 pci=off'},
                      'machine-config': {'vcpu_count': 2, 'mem_size_mib': 256},
                      'drives': [{'drive_id': 'rootfs', 'path_on_host': str(disk),
                                  'is_root_device': True, 'is_read_only': False}]}
            path = work / 'firecracker.json'
            candidate.write_json(path, config)
            boot([fc, '--no-api', '--config-file', str(path)], work / 'firecracker.log', 'hello from unikernel')
    after, _ = candidate.verify_kernel_toolset(root, spec)
    if after != record:
        raise ValueError('Kernel toolset changed during boot validation')
    report = {'toolset': record, 'checks': {'qemu': 'pass', 'firecracker': 'pass'},
              'logs': {name: candidate.digest(work / f'{name}.log') for name in ('qemu', 'firecracker')},
              'hypervisors': {name: subprocess.check_output([binary, '--version'], text=True).strip()
                              for name, binary in [('qemu', qemu), ('firecracker', fc)]}}
    candidate.write_json(root / 'boot-validation.json', report)
    print('PASS: exact kernel toolset booted on QEMU/KVM and Firecracker')


if __name__ == '__main__':
    main()
