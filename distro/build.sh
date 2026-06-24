#!/usr/bin/env bash
# Builds the jerboa WSL2 rootfs tarball (jerboa-rootfs-amd64.tar.gz).
#
# It stages the linux jerboad binary and the kernel build toolchain into a Docker
# build context, builds the image, and exports its filesystem as the tarball the
# client imports with `wsl --import`.
#
# Usage: distro/build.sh [output.tar.gz]
#
# Requires:
#   - docker
#   - the kernel toolchain built first: `make kernel && make -C kernel tools`
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/.." && pwd)"
out="${1:-${root}/jerboa-rootfs-amd64.tar.gz}"

ctx="$(mktemp -d)"
cleanup() { rm -rf "${ctx}"; [ -n "${cid:-}" ] && docker rm -f "${cid}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

cp "${here}/Dockerfile" "${here}/wsl.conf" "${ctx}/"

echo "==> building linux jerboad"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -C "${root}" -o "${ctx}/jerboad" ./cmd/jerboad

echo "==> staging kernel toolchain"
mkdir -p "${ctx}/tools"
cp "${root}/kernel/output/tools/bin/mkfs"             "${ctx}/tools/mkfs"
cp "${root}/kernel/output/platform/pc/boot/boot.img"  "${ctx}/tools/boot.img"
cp "${root}/kernel/output/platform/pc/bin/kernel.img" "${ctx}/tools/kernel.img"

echo "==> building distro image"
docker build -t jerboa-distro:latest "${ctx}"

echo "==> exporting rootfs to ${out}"
cid="$(docker create jerboa-distro:latest)"
docker export "${cid}" | gzip > "${out}"

echo "wrote ${out}"
