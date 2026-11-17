#!/usr/bin/env bash
# Record the Linux upgrade fixture of a released version in a container, for
# hosts that are not Linux x86_64 or where make-fixture.py should not run as
# root. The released binaries are x86_64; on another architecture Docker runs
# them through its emulation and QEMU boots the guests without KVM.
#
# Usage: scripts/upgrade/make-fixture-docker.sh v0.59.0
set -euo pipefail
version="${1:?released version, e.g. v0.59.0}"
repo="$(cd "$(dirname "$0")/../.." && pwd)"
image=jerboa-upgrade-fixture
docker build -q -t "${image}" - <<'DOCKERFILE' >/dev/null
FROM ubuntu:24.04
RUN apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    qemu-system-x86 iproute2 nftables iptables curl ca-certificates python3 procps zstd golang-go minisign \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE
kvm=()
[ ! -e /dev/kvm ] || kvm=(--device /dev/kvm)
# Privileged for the daemon's bridges, TAP devices and firewall rules, all of
# which live and die in the container's own network namespace.
# (bash 3.2, as on macOS, rejects an empty array under set -u.)
docker run --rm --privileged ${kvm[@]+"${kvm[@]}"} \
	-v "${repo}:/repo:ro" -v "${repo}/internal/upgradetest/testdata:/out" \
	"${image}" python3 /repo/scripts/upgrade/make-fixture.py --version "${version}" --out /out
