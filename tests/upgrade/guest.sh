#!/usr/bin/env bash
# The upgrade a user makes, inside a disposable machine (tests/upgrade/run.py
# provides it and copies this script and its payload in; never run it on a
# machine you keep, it installs Jerboa system-wide):
#
#   1. install the current stable release with the public installer;
#   2. build an image, run a VM with a volume and a published port, and bring
#      up a compose project, and write data through both;
#   3. upgrade to the build under test with that build's installer;
#   4. everything is still there and works: same VMs, images, volumes and
#      networks, the stopped VMs start again with their data, the compose
#      project is still managed, new work is possible, `jerboa doctor` finds
#      no problem;
#   5. go back to the stable release and forward again: the old version
#      starts on the new state, fails clearly where it cannot follow, and
#      nothing is lost on the way.
#
# The build under test is not a published release, so step 3 serves it from a
# release origin made here: the installer of the build under test is run
# unchanged except for the key it trusts, against a stable manifest signed
# with a key generated for this run.
#
# Usage: guest.sh PAYLOAD_DIR   (jerboa, jerboad, install.sh, VERSION,
#                                vmstart, voltest/, compose.yaml)
set -euo pipefail
payload="$(cd "${1:?payload directory}" && pwd)"
new_version="$(cat "${payload}/VERSION")"
public_installer="${PUBLIC_INSTALLER:-https://releases.jerboa.dev/install.sh}"
work=/var/tmp/upgrade-e2e
origin_host=releases.upgrade.test
origin_port=8443
export DEBIAN_FRONTEND=noninteractive

step() { printf '\n==> %s\n' "$*"; }
fail() {
	printf '\nFAIL: %s\n' "$*" >&2
	journalctl -u jerboad --no-pager -n 80 >&2 || true
	exit 1
}
[ "$(id -u)" -eq 0 ] || fail "run as root inside the disposable machine"
[ ! -e /usr/local/bin/jerboad ] || fail "Jerboa is already installed here: this is not a disposable machine"
[[ "${new_version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail "invalid version under test: ${new_version}"
mkdir -p "${work}"

# wait_http URL: until the guest answers.
wait_http() {
	local url="$1" i
	for i in $(seq 120); do
		if curl -fsS --max-time 3 "${url}" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	jerboa ps >&2 || true
	fail "no answer from ${url}"
}
# expect_contains URL TEXT: the guest serves TEXT.
expect_contains() {
	local body
	body="$(curl -fsS --max-time 10 "$1")" || fail "no answer from $1"
	grep -qF -- "$2" <<<"${body}" || fail "$1 does not hold '$2': ${body}"
}
write_marker() { curl -fsS --max-time 10 -X POST "$1/write?msg=$2" >/dev/null || fail "could not write $2 through $1"; }
wait_stopped() {
	local i
	for i in $(seq 60); do
		[ "$(jerboa inspect "$1" | jq -r .state)" = stopped ] && return 0
		sleep 1
	done
	fail "VM $1 did not stop"
}
# snapshot DIR: what the daemon reports, in a form that must not change.
snapshot() {
	mkdir -p "$1"
	jerboa ps --output json | jq -S 'map({id, name, image}) | sort_by(.id)' > "$1/vms.json"
	jerboa images --output json | jq -S 'map({name, tag, disk_digest, disk_size}) | sort_by(.name + ":" + .tag)' > "$1/images.json"
	jerboa volume ls --output json | jq -S 'map({id, disk_path, size_bytes, label}) | sort_by(.id)' > "$1/volumes.json"
	jerboa network ls --output json | jq -S 'map({name, subnet, gateway, bridge, driver}) | sort_by(.name)' > "$1/networks.json"
}
same_state() {
	local name
	for name in vms images volumes networks; do
		diff -u "$1/${name}.json" "$2/${name}.json" || fail "${name} changed: $3"
	done
}
installed_version() { jerboa --version | awk '{print $NF}'; }

step "1. install the current stable release with the public installer"
curl --proto '=https' -fsSL "${public_installer}" -o "${work}/stable-install.sh"
bash "${work}/stable-install.sh"
old_version="$(installed_version)"
[ "${old_version}" != "${new_version}" ] || fail "the stable release is already ${new_version}: there is nothing to upgrade from"
systemctl is-active --quiet jerboad || fail "the stable daemon is not running"
hypervisor="$(sed -n 's/.*--hypervisor \([a-z]*\).*/\1/p' /etc/systemd/system/jerboad.service)"
echo "stable ${old_version}, hypervisor ${hypervisor}, kvm $([ -e /dev/kvm ] && echo yes || echo no)"
token_before="$(sha256sum /etc/jerboa/daemon.env | cut -d' ' -f1)"
config_before="$(sha256sum /root/.jerboa/config.toml | cut -d' ' -f1)"

step "2. create an image, a VM with a volume, a network and a compose project"
apt-get install -y --no-install-recommends golang-go >/dev/null
layout=()
[ -e /dev/kvm ] || layout=(--layout standard)
cp -r "${payload}/voltest" "${work}/voltest"
jerboa build "${work}/voltest" --name voltest ${layout[@]+"${layout[@]}"}
jerboa volume create data --size 64M
jerboa network create appnet --subnet 10.90.0.0/24
jerboa run voltest:latest --name web -v data:/data --network appnet -p 18080:8080
wait_http http://127.0.0.1:18080/
write_marker http://127.0.0.1:18080 web-before-upgrade
mkdir -p /srv/app
cp "${payload}/compose.yaml" /srv/app/compose.yaml
(cd /srv/app && jerboa compose up compose.yaml)
wait_http http://127.0.0.1:18081/
write_marker http://127.0.0.1:18081 api-before-upgrade
api_id="$(jerboa inspect api | jq -r .id)"
ls /srv/app/.jerboa-compose-*.json >/dev/null 2>&1 || ls /root/.jerboa/compose/*.json >/dev/null 2>&1 \
	|| fail "compose up left no state"
# Releases before 0.60.0 keep the compose state next to the compose file.
old_compose_beside=no
if ls /srv/app/.jerboa-compose-*.json >/dev/null 2>&1; then old_compose_beside=yes; fi
snapshot "${work}/before"
[ "$(jq length "${work}/before/vms.json")" -eq 2 ] || fail "expected two VMs before the upgrade"

step "3. upgrade to ${new_version} with its own installer"
# A release origin for the build under test: its binaries, and a stable
# manifest signed with a key made for this run.
origin="${work}/origin"
mkdir -p "${origin}/channels" "${origin}/cli/${new_version}" "${origin}/daemon/${new_version}" "${work}/keys"
cp "${payload}/jerboa" "${origin}/cli/${new_version}/jerboa-linux-amd64"
cp "${payload}/jerboad" "${origin}/daemon/${new_version}/jerboad-linux-amd64"
asset() {
	jq -n --arg url "https://${origin_host}:${origin_port}/$1" --arg sha "$(sha256sum "${origin}/$1" | cut -d' ' -f1)" \
		--argjson size "$(stat -c %s "${origin}/$1")" '{url: $url, sha256: $sha, size: $size}'
}
jq -n --arg v "${new_version}" --arg generated "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	--argjson cli "$(asset "cli/${new_version}/jerboa-linux-amd64")" \
	--argjson daemon "$(asset "daemon/${new_version}/jerboad-linux-amd64")" \
	'{channel: "stable", generated: $generated, components: {
		cli: {version: $v, platforms: {"linux-amd64": $cli}},
		daemon: {version: $v, platforms: {"linux-amd64": $daemon}}}}' > "${origin}/channels/stable.json"
minisign -G -W -f -p "${work}/keys/test.pub" -s "${work}/keys/test.key" >/dev/null
minisign -S -s "${work}/keys/test.key" -m "${origin}/channels/stable.json" -x "${origin}/channels/stable.json.minisig" \
	-t "release:${new_version}" >/dev/null
test_key="$(tail -1 "${work}/keys/test.pub")"
# The installer only talks HTTPS: a certificate for the origin, trusted by
# this machine alone.
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=${origin_host}" \
	-addext "subjectAltName=DNS:${origin_host}" -keyout "${work}/keys/tls.key" -out "${work}/keys/tls.crt" 2>/dev/null
cp "${work}/keys/tls.crt" /usr/local/share/ca-certificates/upgrade-e2e.crt
update-ca-certificates >/dev/null 2>&1
grep -q "${origin_host}" /etc/hosts || echo "127.0.0.1 ${origin_host}" >> /etc/hosts
python3 - "${origin}" "${origin_port}" "${work}/keys/tls.crt" "${work}/keys/tls.key" > "${work}/origin.log" 2>&1 <<'PY' &
import functools, http.server, ssl, sys
root, port, cert, key = sys.argv[1:]
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=root)
server = http.server.ThreadingHTTPServer(('127.0.0.1', int(port)), handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(cert, key)
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
PY
origin_pid=$!
trap 'kill "${origin_pid}" 2>/dev/null || true' EXIT
for _ in $(seq 30); do
	curl -fsS "https://${origin_host}:${origin_port}/channels/stable.json" >/dev/null 2>&1 && break
	sleep 0.5
done
# The installer under test, with nothing changed but the key it trusts.
[ "$(grep -c '^PUBLIC_KEY=' "${payload}/install.sh")" -eq 1 ] || fail "install.sh has no single PUBLIC_KEY line"
sed "s|^PUBLIC_KEY=.*|PUBLIC_KEY=\"${test_key}\"|" "${payload}/install.sh" > "${work}/new-install.sh"
upgrade() { RELEASE_BASE="https://${origin_host}:${origin_port}" bash "${work}/new-install.sh"; }
upgrade
[ "$(installed_version)" = "${new_version}" ] || fail "the CLI is $(installed_version), not ${new_version}"
[ "$(jerboad --version | awk '{print $NF}')" = "${new_version}" ] || fail "the daemon binary is not ${new_version}"
systemctl is-active --quiet jerboad || fail "the upgraded daemon is not running"

step "4. everything is still there"
[ "$(sha256sum /etc/jerboa/daemon.env | cut -d' ' -f1)" = "${token_before}" ] || fail "the upgrade changed the daemon token"
[ "$(sha256sum /root/.jerboa/config.toml | cut -d' ' -f1)" = "${config_before}" ] || fail "the upgrade changed the CLI settings"
snapshot "${work}/after"
same_state "${work}/before" "${work}/after" "across the upgrade"
[ "$(jerboa inspect web | jq -r .state)" = stopped ] || fail "web should be stopped after the daemon restart"

step "4a. a stopped VM starts again, with its network and its data"
# (Starting a stopped VM replaces it with a new one of the same name, so it
# is addressed by name from here on.)
"${payload}/vmstart" web
wait_http http://127.0.0.1:18080/
expect_contains http://127.0.0.1:18080/ web-before-upgrade
write_marker http://127.0.0.1:18080 web-after-upgrade

step "4b. the compose project is still managed"
(cd /srv/app && jerboa compose ps compose.yaml --output json) > "${work}/compose-ps.json"
[ "$(jq -r '.[] | select(.service == "api") | .id' "${work}/compose-ps.json")" = "${api_id}" ] || fail "compose ps lost the api service"
[ "$(jq -r '.[] | select(.service == "api") | .state' "${work}/compose-ps.json")" = stopped ] || fail "compose ps does not see api stopped"
if ls /srv/app/.jerboa-compose-*.json >/dev/null 2>&1; then fail "the compose state is still next to the compose file"; fi
ls /root/.jerboa/compose/*.json >/dev/null || fail "the compose state was not moved to ~/.jerboa/compose"
(cd /srv/app && jerboa compose down compose.yaml)
if jerboa inspect api >/dev/null 2>&1; then fail "compose down left the api VM"; fi
# The volume outlives a down without --volumes, and the next up reuses it.
(cd /srv/app && jerboa compose up compose.yaml)
wait_http http://127.0.0.1:18081/
expect_contains http://127.0.0.1:18081/ api-before-upgrade
write_marker http://127.0.0.1:18081 api-after-upgrade

step "4c. new work is possible on the old state"
jerboa build "${work}/voltest" --name voltest --tag rebuilt ${layout[@]+"${layout[@]}"}
jerboa volume create fresh --size 64M
jerboa run voltest:rebuilt --name second -v fresh:/data --network appnet -p 18082:8080
wait_http http://127.0.0.1:18082/
jerboa stop second
wait_stopped second
jerboa rm second
jerboa volume rm fresh

step "4d. jerboa doctor finds no problem"
jerboa doctor || fail "jerboa doctor reports a problem after the upgrade"
snapshot "${work}/upgraded"

step "5. back to ${old_version}: the old version on the new state"
bash "${work}/stable-install.sh" --version "${old_version}"
[ "$(installed_version)" = "${old_version}" ] || fail "the downgrade did not install ${old_version}"
systemctl is-active --quiet jerboad || fail "${old_version} does not start on the state ${new_version} left"
snapshot "${work}/downgraded"
same_state "${work}/upgraded" "${work}/downgraded" "across the downgrade"
if [ "${old_compose_beside}" = yes ]; then
	# The old CLI looks for the compose state next to the compose file, where it
	# no longer is: it must say so, and its down must not act on anything.
	if (cd /srv/app && jerboa compose ps compose.yaml) > "${work}/old-compose.log" 2>&1; then
		fail "${old_version} lists a project whose state it cannot see: $(cat "${work}/old-compose.log")"
	fi
	grep -q "read state" "${work}/old-compose.log" || fail "${old_version} failed on the compose project without saying why: $(cat "${work}/old-compose.log")"
	(cd /srv/app && jerboa compose down compose.yaml --volumes) >/dev/null 2>&1 || true
	snapshot "${work}/downgraded-after"
	same_state "${work}/upgraded" "${work}/downgraded-after" "by a refused compose down"
else
	# The old CLI keeps the compose state where the new one does: it still
	# manages the project.
	(cd /srv/app && jerboa compose ps compose.yaml --output json) | jq -e '.[] | select(.service == "api")' >/dev/null \
		|| fail "${old_version} no longer manages the compose project after the downgrade"
fi

step "5a. forward again: nothing was lost on the way"
upgrade
[ "$(installed_version)" = "${new_version}" ] || fail "the second upgrade did not install ${new_version}"
snapshot "${work}/final"
same_state "${work}/upgraded" "${work}/final" "across the downgrade and the second upgrade"
"${payload}/vmstart" web
wait_http http://127.0.0.1:18080/
expect_contains http://127.0.0.1:18080/ web-before-upgrade
expect_contains http://127.0.0.1:18080/ web-after-upgrade
(cd /srv/app && jerboa compose ps compose.yaml --output json) | jq -e '.[] | select(.service == "api")' >/dev/null \
	|| fail "the compose project is no longer managed after the round trip"
(cd /srv/app && jerboa compose down compose.yaml --volumes)
jerboa doctor || fail "jerboa doctor reports a problem after the round trip"

printf '\nPASS: %s -> %s -> %s -> %s (hypervisor %s)\n' "${old_version}" "${new_version}" "${old_version}" "${new_version}" "${hypervisor}"
