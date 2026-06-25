# Builds the dedicated jerboa WSL2 root filesystem: a self-contained Linux
# environment with jerboad, qemu, firecracker, and the kernel build toolchain.
# distro/build.sh exports this image's filesystem to a tarball that the client
# imports with `wsl --import`, the way Docker Desktop ships its own distro.
FROM ubuntu:24.04

ARG FIRECRACKER_VERSION=v1.10.1
ARG FC_ARCH=x86_64

# Runtime deps: qemu, networking for the firecracker tap device, e2fsprogs for
# the image build toolchain, and certs/curl for runtime downloads.
RUN apt-get update && apt-get install -y --no-install-recommends \
        qemu-system-x86 qemu-utils \
        iproute2 iptables \
        e2fsprogs \
        kmod \
        ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# Firecracker binary.
RUN curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}.tgz" \
        -o /tmp/fc.tgz \
    && tar -xzf /tmp/fc.tgz -C /tmp \
    && install -m 0755 "/tmp/release-${FIRECRACKER_VERSION}-${FC_ARCH}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}" \
        /usr/local/bin/firecracker \
    && rm -rf /tmp/fc.tgz "/tmp/release-${FIRECRACKER_VERSION}-${FC_ARCH}"

# jerboad daemon, staged into the build context by distro/build.sh.
COPY jerboad /usr/local/bin/jerboad
RUN chmod 0755 /usr/local/bin/jerboad

# Kernel build toolchain at the daemon's default cache dir (root's home), so
# image builds and the firecracker kernel resolve locally without downloads.
COPY tools/ /root/.jerboa/tools/

# A dedicated interactive user; the daemon itself runs as root for firecracker.
RUN useradd -m -s /bin/bash jerboa
COPY wsl.conf /etc/wsl.conf
# Loads the KVM module at distro boot (referenced by wsl.conf [boot] command).
COPY load-kvm.sh /usr/local/sbin/load-kvm.sh
RUN chmod 0755 /usr/local/sbin/load-kvm.sh

LABEL org.jerboa.distro="jerboa"
CMD ["/bin/bash"]
