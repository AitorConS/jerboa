#!/usr/bin/env bash
# The nightly image benchmark: a QEMU and a Firecracker daemon from this
# commit, then jerboa-bench against both.
#
#   scripts/nightly-image-bench.sh start      start both daemons, create their networks
#   scripts/nightly-image-bench.sh run        build and run jerboa-bench
#   scripts/nightly-image-bench.sh stop       remove VMs and networks, stop the daemons,
#                                             hand their files back
#   scripts/nightly-image-bench.sh diagnose   print the daemon and VM logs
#   scripts/nightly-image-bench.sh clean      remove the benchmark directory
#
# Guests get a bridge and a tap each, so the daemons need CAP_NET_ADMIN (see
# scripts/run-kvm-tests.sh). Without it they still start and `jerboa run`
# still returns an id, but no guest answers on its published port. When the
# runner has neither the capability nor root, the daemons and jerboa-bench
# (which talks to their root-owned sockets) run under passwordless sudo.
# `stop` gives the results and metrics back to the runner user so the
# workspace holds nothing owned by root.
#
# start records BENCH_ROOT and BENCH_AS_ROOT in $GITHUB_ENV for the later
# steps.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${root}"

as_root=()
if [ "${BENCH_AS_ROOT:-0}" = 1 ]; then
	as_root=(sudo -n)
fi

need_root() {
	[ "$(uname -s)" = Linux ] || return 1
	[ "$(id -u)" -ne 0 ] || return 1
	local probe="jbbench-$$"
	if ip link add "${probe}" type dummy 2>/dev/null; then
		ip link del "${probe}"
		return 1
	fi
	return 0
}

start() {
	BENCH_ROOT="$(mktemp -d)"
	local tools="${BENCH_ROOT}/tools"
	mkdir -p "${tools}" "${BENCH_ROOT}/qemu" "${BENCH_ROOT}/fc"
	echo "BENCH_ROOT=${BENCH_ROOT}" >>"${GITHUB_ENV:-/dev/null}"
	if need_root; then
		sudo -n true || { echo "::error::guest networking needs CAP_NET_ADMIN or passwordless sudo" >&2; exit 1; }
		as_root=(sudo -n)
		echo "BENCH_AS_ROOT=1" >>"${GITHUB_ENV:-/dev/null}"
		echo "nightly-image-bench: no CAP_NET_ADMIN here; the daemons and jerboa-bench run under sudo"
	fi
	# The freshly built kernel toolchain, so the QEMU daemon resolves
	# mkfs/boot.img/kernel.img locally instead of downloading a release.
	cp kernel/output/tools/bin/mkfs "${tools}/mkfs"
	cp kernel/output/platform/pc/boot/boot.img "${tools}/boot.img"
	cp kernel/output/platform/pc/bin/kernel.img "${tools}/kernel.img"

	# Each daemon is a sudo process when as_root is set; stop signals it and
	# sudo passes the signal on.
	${as_root[@]+"${as_root[@]}"} env "HOME=${BENCH_ROOT}/qemu" ./dist/jerboad \
		--host "unix://${BENCH_ROOT}/qemu.sock" --tools-dir "${tools}" \
		--store "${BENCH_ROOT}/qemu/images" >"${BENCH_ROOT}/qemu.log" 2>&1 &
	echo "QEMU_PID=$!" >>"${GITHUB_ENV:-/dev/null}"
	# The Firecracker daemon downloads kernel-fc.img on first start; it uses
	# the staged mkfs so both images come from this commit.
	${as_root[@]+"${as_root[@]}"} env "HOME=${BENCH_ROOT}/fc" ./dist/jerboad \
		--host "unix://${BENCH_ROOT}/fc.sock" --hypervisor firecracker \
		--tools-dir "${tools}" --store "${BENCH_ROOT}/fc/images" \
		--fc-metrics-dir "${BENCH_ROOT}/fc-metrics" >"${BENCH_ROOT}/fc.log" 2>&1 &
	echo "FC_PID=$!" >>"${GITHUB_ENV:-/dev/null}"

	for _ in $(seq 1 120); do
		[ -S "${BENCH_ROOT}/qemu.sock" ] && [ -S "${BENCH_ROOT}/fc.sock" ] && break
		sleep 1
	done
	[ -S "${BENCH_ROOT}/qemu.sock" ] || { cat "${BENCH_ROOT}/qemu.log"; exit 1; }
	[ -S "${BENCH_ROOT}/fc.sock" ] || { cat "${BENCH_ROOT}/fc.log"; exit 1; }
	jerboa qemu network create benchq --subnet 10.231.0.0/24
	jerboa fc network create benchfc --subnet 10.232.0.0/24
}

# jerboa DAEMON ARGS... runs the CLI against the qemu or fc daemon.
jerboa() {
	local daemon="$1"
	shift
	${as_root[@]+"${as_root[@]}"} env "HOME=${BENCH_ROOT}/${daemon}" ./dist/jerboa \
		--host "unix://${BENCH_ROOT}/${daemon}.sock" "$@"
}

run() {
	: "${BENCH_ROOT:?run start first}"
	go build -o dist/jerboa-bench ./cmd/jerboa-bench
	local runtimes=(
		--runtime "qemu=jerboa,host=unix://${BENCH_ROOT}/qemu.sock,network=benchq,store=${BENCH_ROOT}/qemu/images"
		--runtime "fc=jerboa,host=unix://${BENCH_ROOT}/fc.sock,network=benchfc,store=${BENCH_ROOT}/fc/images"
		--runtime "fc-compact=jerboa,host=unix://${BENCH_ROOT}/fc.sock,network=benchfc,store=${BENCH_ROOT}/fc/images,layout=compact"
	)
	if docker info >/dev/null 2>&1; then
		runtimes+=(--runtime docker=docker)
	else
		echo "::notice::docker unavailable on this runner; benchmarking Jerboa backends only"
	fi
	local env_root=()
	if [ "${#as_root[@]}" -gt 0 ]; then
		# jerboa-bench compiles its test app: a Go cache of its own, the
		# runner's module cache read only, no downloads.
		env_root=("${as_root[@]}" env "PATH=${PATH}" "HOME=${BENCH_ROOT}" "GOCACHE=${BENCH_ROOT}/go-build"
			"GOMODCACHE=$(go env GOMODCACHE)" GOPROXY=off GOTOOLCHAIN=local)
	fi
	# pipefail: a failed benchmark fails the step, not only the summary.
	${env_root[@]+"${env_root[@]}"} ./dist/jerboa-bench --jerboa ./dist/jerboa "${runtimes[@]}" \
		--runs 30 --warmup 3 --seed "${GITHUB_RUN_ID:-1}" \
		--idle-sample 5s --load-duration 5s --load-concurrency 16 --load-direct \
		--out bench-image.json | tee bench-summary.txt
}

stop() {
	# The daemon is what removes bridges, taps and their firewall entries:
	# with root, a bridge left behind keeps 10.23x.0.0/24 on the host and the
	# next night's `network create` refuses the overlapping subnet.
	if [ -n "${BENCH_ROOT:-}" ]; then
		local daemon net id
		for daemon in qemu fc; do
			[ -S "${BENCH_ROOT}/${daemon}.sock" ] || continue
			net=benchq
			[ "${daemon}" = fc ] && net=benchfc
			for id in $(jerboa "${daemon}" --output json ps 2>/dev/null |
				python3 -c 'import json,sys; [print(v["id"]) for v in json.load(sys.stdin) or []]' 2>/dev/null); do
				jerboa "${daemon}" stop --force "${id}" >/dev/null 2>&1 || true
				jerboa "${daemon}" rm "${id}" >/dev/null 2>&1 || true
			done
			jerboa "${daemon}" network rm "${net}" || true
		done
	fi
	[ -n "${QEMU_PID:-}" ] && ${as_root[@]+"${as_root[@]}"} kill "${QEMU_PID}" 2>/dev/null || true
	[ -n "${FC_PID:-}" ] && ${as_root[@]+"${as_root[@]}"} kill "${FC_PID}" 2>/dev/null || true
	if [ "${#as_root[@]}" -gt 0 ]; then
		local me
		me="$(id -u):$(id -g)"
		for f in bench-image.json bench-image.csv; do
			[ -e "${f}" ] && sudo -n chown "${me}" "${f}"
		done
		[ -n "${BENCH_ROOT:-}" ] && [ -d "${BENCH_ROOT}/fc-metrics" ] && sudo -n chown -R "${me}" "${BENCH_ROOT}/fc-metrics"
	fi
	return 0
}

diagnose() {
	[ -n "${BENCH_ROOT:-}" ] && [ -d "${BENCH_ROOT}" ] || { echo "no benchmark directory"; return 0; }
	for daemon in qemu fc; do
		echo "::group::${daemon} daemon log (last 200 lines)"
		${as_root[@]+"${as_root[@]}"} tail -n 200 "${BENCH_ROOT}/${daemon}.log" || true
		echo "::endgroup::"
	done
	# The VM console logs, wherever each daemon keeps them.
	while IFS= read -r -d '' log; do
		echo "::group::${log#"${BENCH_ROOT}/"} (last 80 lines)"
		${as_root[@]+"${as_root[@]}"} tail -n 80 "${log}" || true
		echo "::endgroup::"
	done < <(${as_root[@]+"${as_root[@]}"} find "${BENCH_ROOT}/qemu" "${BENCH_ROOT}/fc" -type f -name '*.log' -size -4M -print0 2>/dev/null)
	echo "::group::host network"
	ip -brief link show type bridge || true
	ip -brief addr || true
	${as_root[@]+"${as_root[@]}"} nft list ruleset 2>/dev/null | head -n 200 || true
	echo "::endgroup::"
}

clean() {
	# Only the directory this job made: it holds both daemons' homes.
	if [ -n "${BENCH_ROOT:-}" ] && [ -d "${BENCH_ROOT}/qemu" ] && [ -d "${BENCH_ROOT}/fc" ]; then
		${as_root[@]+"${as_root[@]}"} rm -rf -- "${BENCH_ROOT}"
	fi
}

case "${1:-}" in
start | run | stop | diagnose | clean) "$1" ;;
*)
	echo "usage: $0 start|run|stop|diagnose|clean" >&2
	exit 2
	;;
esac
