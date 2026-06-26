#!/usr/bin/env bash
# Installs jerboad natively on a Linux host: the daemon binary, firecracker,
# qemu, and the runtime dependencies, plus a systemd service to run it.
#
# This is the Linux counterpart to `jerboa daemon install` on Windows, which
# imports a preconfigured WSL2 distro (see distro/Dockerfile). On Linux there is
# no distro: jerboad runs directly on the host, so this script provisions the
# same pieces the distro image bakes in.
#
# It mirrors the distro's privilege model with a dedicated, unprivileged `jerboa`
# user instead of root: KVM access comes from the `kvm` group and the networking
# privileges firecracker needs (tap devices, ip/iptables, ip_forward) come from
# CAP_NET_ADMIN/CAP_NET_RAW granted to the service unit. The daemon listens on
# loopback TCP with a generated token (the Unix socket default is skipped because
# jerboad does not group-share it across users), and that endpoint + token are
# written to the invoking user's config so the `jerboa` CLI connects out of the box.
#
# Usage:  sudo scripts/install.sh
#
# Environment overrides:
#   FIRECRACKER_VERSION   firecracker release tag       (default v1.10.1)
#   FC_ARCH               firecracker arch              (default x86_64)
#   JERBOA_PORT           daemon loopback TCP port      (default 7890)
#   HYPERVISOR            qemu or firecracker           (default firecracker)
#   RELEASE_BASE          release download base URL
set -euo pipefail

FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.10.1}"
FC_ARCH="${FC_ARCH:-x86_64}"
JERBOA_PORT="${JERBOA_PORT:-7890}"
HYPERVISOR="${HYPERVISOR:-firecracker}"
RELEASE_BASE="${RELEASE_BASE:-https://github.com/AitorConS/jerboa/releases/latest/download}"

PREFIX=/usr/local/bin
JERBOA_USER=jerboa
JERBOA_HOME=/var/lib/jerboa
ENV_DIR=/etc/jerboa
ENV_FILE="${ENV_DIR}/daemon.env"
UNIT=/etc/systemd/system/jerboad.service

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[ "$(id -u)" -eq 0 ] || die "run as root: sudo scripts/install.sh"
command -v apt-get >/dev/null 2>&1 || die "this installer targets Debian/Ubuntu (apt-get not found)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required (systemctl not found)"
[ -e /dev/kvm ] || die "/dev/kvm not present: enable hardware virtualization (and nested virtualization if in a VM)"

log "installing runtime dependencies"
# Same set the distro image installs (distro/Dockerfile), minus the build-time
# tooling: qemu, networking for firecracker's tap device, e2fsprogs for the
# image toolchain, and certs/curl for jerboad's runtime downloads.
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
	qemu-system-x86 qemu-utils \
	iproute2 iptables \
	e2fsprogs \
	kmod \
	ca-certificates curl

log "installing firecracker ${FIRECRACKER_VERSION}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}.tgz" \
	-o "${tmp}/fc.tgz"
tar -xzf "${tmp}/fc.tgz" -C "${tmp}"
install -m 0755 "${tmp}/release-${FIRECRACKER_VERSION}-${FC_ARCH}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}" \
	"${PREFIX}/firecracker"

log "installing jerboa CLI and jerboad daemon"
curl -fsSL "${RELEASE_BASE}/jerboad-linux-amd64" -o "${tmp}/jerboad"
curl -fsSL "${RELEASE_BASE}/jerboa-linux-amd64" -o "${tmp}/jerboa"
install -m 0755 "${tmp}/jerboad" "${PREFIX}/jerboad"
install -m 0755 "${tmp}/jerboa" "${PREFIX}/jerboa"

log "ensuring KVM module is loaded at boot"
# Microsoft's WSL2 kernel ships KVM as a module; a stock host usually auto-loads
# it, but persist it so a fresh boot always has /dev/kvm for firecracker.
modprobe kvm 2>/dev/null || true
echo kvm > /etc/modules-load.d/jerboa-kvm.conf

log "creating ${JERBOA_USER} system user"
if ! id "${JERBOA_USER}" >/dev/null 2>&1; then
	useradd --system --create-home --home-dir "${JERBOA_HOME}" \
		--shell /usr/sbin/nologin "${JERBOA_USER}"
fi
# KVM access for firecracker/qemu without root.
getent group kvm >/dev/null 2>&1 && usermod -aG kvm "${JERBOA_USER}"

log "generating daemon auth token"
# The daemon binds loopback TCP, reachable by any local process, so it always
# requires a token (jerboad reads JERBOA_AUTH_TOKEN). The same token goes to the
# CLI user's config below so the handshake is transparent.
token="$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
install -d -m 0750 "${ENV_DIR}"
umask 077
cat > "${ENV_FILE}" <<EOF
JERBOA_AUTH_TOKEN=${token}
EOF
umask 022
chown root:"${JERBOA_USER}" "${ENV_FILE}"
chmod 0640 "${ENV_FILE}"

endpoint="tcp://127.0.0.1:${JERBOA_PORT}"

log "writing systemd unit ${UNIT}"
cat > "${UNIT}" <<EOF
[Unit]
Description=jerboa unikernel engine daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${JERBOA_USER}
SupplementaryGroups=kvm
# firecracker networking: tap device creation, ip/iptables, ip_forward sysctl.
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW
EnvironmentFile=${ENV_FILE}
ExecStart=${PREFIX}/jerboad --host ${endpoint} --hypervisor ${HYPERVISOR}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

log "enabling and starting jerboad"
systemctl daemon-reload
systemctl enable --now jerboad.service

# Point the invoking user's CLI at the daemon with the shared token. SUDO_USER
# is the human who ran sudo; fall back to root when invoked as root directly.
target_user="${SUDO_USER:-root}"
target_home="$(getent passwd "${target_user}" | cut -d: -f6)"
if [ -n "${target_home}" ]; then
	cfg_dir="${target_home}/.jerboa"
	cfg="${cfg_dir}/config.toml"
	if [ -e "${cfg}" ]; then
		log "leaving existing ${cfg} untouched (set [daemon] endpoint/token manually if needed)"
	else
		install -d -o "${target_user}" -g "${target_user}" "${cfg_dir}"
		cat > "${cfg}" <<EOF
hypervisor = "${HYPERVISOR}"

[daemon]
endpoint = "${endpoint}"
token = "${token}"
EOF
		chown "${target_user}:${target_user}" "${cfg}"
		chmod 0600 "${cfg}"
		log "wrote ${cfg}"
	fi
fi

echo
log "done. jerboad is running at ${endpoint} (hypervisor=${HYPERVISOR})"
echo "    status:  systemctl status jerboad"
echo "    logs:    journalctl -u jerboad -f"
echo "    verify:  jerboa status"
if [ "${target_user}" != "root" ] && getent group kvm >/dev/null 2>&1; then
	echo "    note:    if 'jerboa' commands hit KVM permission errors, ensure your"
	echo "             user is in the kvm group (or just rely on the jerboad service)."
fi
