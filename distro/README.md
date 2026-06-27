# jerboa WSL2 distro

This directory builds the dedicated WSL2 distro used by Windows installs.

It exists to keep the Windows story consistent:

- `jerboa.exe` runs on the host
- `jerboad` runs as Linux inside its own imported distro
- QEMU, Firecracker, and the kernel toolchain live there with it

## What The Rootfs Contains

- `/usr/local/bin/jerboad`
- `/usr/local/bin/firecracker`
- QEMU installed from the distro package manager
- kernel tools under `/root/.jerboa/tools/`

The daemon is launched as `root` inside this distro.

## Build

From the repo root:

```bash
make kernel
make -C kernel tools
bash distro/build.sh
```

That produces `jerboa-rootfs-amd64.tar.gz`.

## Use From Windows

```powershell
jerboa daemon install --rootfs .\jerboa-rootfs-amd64.tar.gz
jerboa daemon start
jerboa daemon status
```

Without `--rootfs`, `jerboa daemon install` downloads the release rootfs artifact instead.
