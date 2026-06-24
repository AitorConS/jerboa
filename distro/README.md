# jerboa WSL2 distro

A dedicated, versioned WSL2 distribution that hosts the `jerboad` daemon on
Windows — the same model Docker Desktop uses for `docker-desktop`. It bundles
everything the daemon needs (jerboad, qemu, firecracker, the kernel build
toolchain), so nothing depends on the user's WSL setup, on `jerboad` being on
PATH, or on host `sudo`.

## Contents

| Path in distro | What |
|---|---|
| `/usr/local/bin/jerboad` | the daemon binary |
| `/usr/local/bin/firecracker` | firecracker microVM monitor |
| `qemu-system-x86_64` (apt) | QEMU hypervisor |
| `/root/.jerboa/tools/{mkfs,boot.img,kernel.img}` | kernel build toolchain (daemon default cache dir) |
| user `jerboa` | default interactive user; the daemon runs as `root` |

## Build

```bash
make kernel && make -C kernel tools     # produce the toolchain
distro/build.sh                          # -> jerboa-rootfs-amd64.tar.gz
```

`build.sh` builds a linux `jerboad`, stages it with the toolchain into a Docker
build context, builds `distro/Dockerfile`, and `docker export`s the container
filesystem to the tarball. CI builds and attaches this tarball to each release.

## Install (client side)

```bash
jerboa daemon install                      # downloads the release rootfs and wsl --imports it
jerboa daemon install --rootfs ./jerboa-rootfs-amd64.tar.gz   # or a local build
jerboa daemon start --hypervisor firecracker
```

The client imports the distro to `%LOCALAPPDATA%\jerboa\distro`, runs `jerboad`
inside it as `root` bound to `tcp://0.0.0.0:7890`, and dials it from Windows on
`tcp://127.0.0.1:7890`. Remove it with `jerboa daemon uninstall`.
