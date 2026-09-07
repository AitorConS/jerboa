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
# Usage: sudo bash install.sh [--version latest|0.51.2|v0.51.2]
# Piped: curl -fsSL https://jerboa.dev/install.sh | sudo bash -s -- --version 0.51.2
#
# Environment overrides:
#   FIRECRACKER_VERSION   firecracker release tag       (default v1.10.1)
#   FC_ARCH               firecracker arch              (default x86_64)
#   JERBOA_PORT           daemon loopback TCP port      (default 7890)
#   HYPERVISOR            qemu or firecracker           (default firecracker)
#   RELEASE_BASE          release origin (default https://releases.jerboa.dev)
set -euo pipefail

FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.10.1}"
FC_ARCH="${FC_ARCH:-x86_64}"
JERBOA_PORT="${JERBOA_PORT:-7890}"
HYPERVISOR="${HYPERVISOR:-firecracker}"
RELEASE_BASE="${RELEASE_BASE:-https://releases.jerboa.dev}"
RELEASE_BASE="${RELEASE_BASE%/}"
VERSION=latest
PUBLIC_KEY="RWQUeMEQrLXFcshAMUevjf6nlhsSB1PuZYt5dFhb0za9aypwSAUMorsH"

PREFIX=/usr/local/bin
JERBOA_USER=jerboa
JERBOA_HOME=/var/lib/jerboa
ENV_DIR=/etc/jerboa
ENV_FILE="${ENV_DIR}/daemon.env"
UNIT=/etc/systemd/system/jerboad.service

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }

usage() {
	cat <<'HELP'
Usage: sudo bash install.sh [--version VERSION]

  --version VERSION  Install latest (default), 0.51.2 or v0.51.2.
  -h, --help         Show this help without changing the system.

Requires Debian/Ubuntu with systemd on x86_64 Linux.
HYPERVISOR=qemu allows running without KVM (software emulation).
HELP
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			[ "$#" -ge 2 ] || die "--version requires a value"
			VERSION="$2"; shift 2 ;;
		--version=*) VERSION="${1#*=}"; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown argument: $1 (see --help)" ;;
	esac
done
if [ "${VERSION}" != latest ]; then
	VERSION="v${VERSION#v}"
	[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "invalid version: ${VERSION}"
fi
[[ "${RELEASE_BASE}" =~ ^https://[^[:space:]]+$ ]] || die "RELEASE_BASE must be an HTTPS origin"
case "${HYPERVISOR}" in qemu|firecracker) ;; *) die "HYPERVISOR must be qemu or firecracker" ;; esac
[[ "${JERBOA_PORT}" =~ ^[0-9]{1,5}$ ]] || die "JERBOA_PORT must be between 1 and 65535"
JERBOA_PORT=$((10#${JERBOA_PORT}))
[ "${JERBOA_PORT}" -ge 1 ] && [ "${JERBOA_PORT}" -le 65535 ] || die "JERBOA_PORT must be between 1 and 65535"
[ "${FC_ARCH}" = x86_64 ] || die "only FC_ARCH=x86_64 is supported by the Linux daemon"
[[ "${FIRECRACKER_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid FIRECRACKER_VERSION"

[ "$(uname -s)" = Linux ] || die "this installer requires Linux"
[ "$(uname -m)" = x86_64 ] || die "the published Linux daemon currently supports x86_64 only"
[ "$(id -u)" -eq 0 ] || die "run as root: sudo bash install.sh"
command -v apt-get >/dev/null 2>&1 || die "this installer targets Debian/Ubuntu (apt-get not found)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required (systemctl not found)"
[ -d /run/systemd/system ] || die "systemd must be running as the system manager"
if [ "${HYPERVISOR}" = firecracker ] && [ ! -e /dev/kvm ]; then
	die "/dev/kvm not present: enable virtualization or use HYPERVISOR=qemu"
fi

if [ -f "${ENV_FILE}" ]; then
	# Preserve credentials on upgrades; never execute an environment file as shell.
	token="$(sed -n 's/^JERBOA_AUTH_TOKEN=//p' "${ENV_FILE}")"
	[[ "${token}" =~ ^[0-9a-fA-F]{64}$ ]] || die "existing ${ENV_FILE} has an invalid token; refusing to overwrite it"
else
	token="$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
fi

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
	ca-certificates curl jq minisign

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
fetch() {
	curl --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 \
		--retry 3 -fsSL -A jerboa-installer/1.0 "$1" -o "$2"
}

log "resolving Jerboa ${VERSION}"
fetch "${RELEASE_BASE}/channels/stable.json" "${tmp}/stable.json"
fetch "${RELEASE_BASE}/channels/stable.json.minisig" "${tmp}/stable.json.minisig"
minisign -Vm "${tmp}/stable.json" -x "${tmp}/stable.json.minisig" -P "${PUBLIC_KEY}" >/dev/null \
	|| die "release manifest signature verification failed"
if [ "${VERSION}" = latest ]; then
	VERSION="$(jq -er '.components.cli.version' "${tmp}/stable.json")"
	[ "${VERSION}" = "$(jq -er '.components.daemon.version' "${tmp}/stable.json")" ] \
		|| die "stable CLI and daemon versions differ; refusing a mixed installation"
fi
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "invalid release manifest version"

for component in cli daemon; do
	binary=jerboa
	[ "${component}" != daemon ] || binary=jerboad
	url="${RELEASE_BASE}/${component}/${VERSION}/${binary}-linux-amd64"
	log "downloading ${binary} ${VERSION}"
	fetch "${url}" "${tmp}/${binary}"
	if [ "${VERSION}" = "$(jq -er --arg c "${component}" '.components[$c].version' "${tmp}/stable.json")" ]; then
		digest="$(jq -er --arg c "${component}" '.components[$c].platforms["linux-amd64"].sha256' "${tmp}/stable.json")"
		[[ "${digest}" =~ ^[0-9a-fA-F]{64}$ ]] || die "invalid ${component} checksum in manifest"
		printf '%s  %s\n' "${digest}" "${tmp}/${binary}" | sha256sum -c - \
			|| die "${binary} checksum verification failed"
	else
		log "${VERSION} is not in the signed stable manifest; historical download uses HTTPS without a signed checksum"
	fi
done

if [ "${HYPERVISOR}" = firecracker ]; then
	log "downloading firecracker ${FIRECRACKER_VERSION}"
	fetch "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}.tgz" "${tmp}/fc.tgz"
	tar -xzf "${tmp}/fc.tgz" -C "${tmp}"
	[ -f "${tmp}/release-${FIRECRACKER_VERSION}-${FC_ARCH}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}" ] || die "firecracker archive does not contain the expected binary"
fi

# Stage replacements beside their destination, then rename. Overwriting a
# running executable directly can fail with ETXTBSY during upgrades.
install -d "${PREFIX}"
for binary in jerboa jerboad; do
	stage="$(mktemp "${PREFIX}/.${binary}.XXXXXX")"
	install -m 0755 "${tmp}/${binary}" "${stage}"
	mv -f "${stage}" "${PREFIX}/${binary}"
done
if [ "${HYPERVISOR}" = firecracker ]; then
	stage="$(mktemp "${PREFIX}/.firecracker.XXXXXX")"
	install -m 0755 "${tmp}/release-${FIRECRACKER_VERSION}-${FC_ARCH}/firecracker-${FIRECRACKER_VERSION}-${FC_ARCH}" "${stage}"
	mv -f "${stage}" "${PREFIX}/firecracker"
fi

log "ensuring KVM module is loaded at boot"
# Microsoft's WSL2 kernel ships KVM as a module; a stock host usually auto-loads
# it, but persist it so a fresh boot always has /dev/kvm for firecracker.
modprobe kvm 2>/dev/null || true
install -d /etc/modules-load.d
if [ -e /dev/kvm ]; then
	echo kvm > /etc/modules-load.d/jerboa-kvm.conf
fi

log "creating ${JERBOA_USER} system user"
getent group "${JERBOA_USER}" >/dev/null 2>&1 || groupadd --system "${JERBOA_USER}"
if ! id "${JERBOA_USER}" >/dev/null 2>&1; then
	useradd --system --gid "${JERBOA_USER}" --create-home --home-dir "${JERBOA_HOME}" \
		--shell /usr/sbin/nologin "${JERBOA_USER}"
fi
# KVM access for firecracker/qemu without root.
kvm_unit=""
if [ -e /dev/kvm ]; then
	getent group kvm >/dev/null 2>&1 || groupadd --system kvm
	usermod -aG kvm "${JERBOA_USER}"
	kvm_unit="SupplementaryGroups=kvm"
fi
install -d -o "${JERBOA_USER}" -g "${JERBOA_USER}" -m 0750 "${JERBOA_HOME}"

log "preparing daemon auth token"
# The daemon binds loopback TCP, reachable by any local process, so it always
# requires a token (jerboad reads JERBOA_AUTH_TOKEN). The same token goes to the
# CLI user's config below so the handshake is transparent.
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
${kvm_unit}
Environment=HOME=${JERBOA_HOME}
WorkingDirectory=${JERBOA_HOME}
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
systemctl enable jerboad.service
systemctl restart jerboad.service
systemctl is-active --quiet jerboad.service || die "jerboad failed to start; inspect journalctl -u jerboad"

# Point the invoking user's CLI at the daemon with the shared token. SUDO_USER
# is the human who ran sudo; fall back to root when invoked as root directly.
target_user="${SUDO_USER:-root}"
target_home="$(getent passwd "${target_user}" | cut -d: -f6)"
target_group="$(id -gn "${target_user}")"
if [ -n "${target_home}" ]; then
	cfg_dir="${target_home}/.jerboa"
	cfg="${cfg_dir}/config.toml"
	if [ -e "${cfg}" ]; then
		log "leaving existing ${cfg} untouched (set [daemon] endpoint/token manually if needed)"
	else
		install -d -o "${target_user}" -g "${target_group}" "${cfg_dir}"
		umask 077
		cat > "${cfg}" <<EOF
hypervisor = "${HYPERVISOR}"

[daemon]
endpoint = "${endpoint}"
token = "${token}"
EOF
		chown "${target_user}:${target_group}" "${cfg}"
		chmod 0600 "${cfg}"
		umask 022
		log "wrote ${cfg}"
	fi
fi

echo
log "done. Jerboa ${VERSION}: jerboad is running at ${endpoint} (hypervisor=${HYPERVISOR})"
echo "    status:  systemctl status jerboad"
echo "    logs:    journalctl -u jerboad -f"
echo "    verify:  jerboa status"
if [ "${target_user}" != "root" ] && getent group kvm >/dev/null 2>&1; then
	echo "    note:    if 'jerboa' commands hit KVM permission errors, ensure your"
	echo "             user is in the kvm group (or just rely on the jerboad service)."
fi
