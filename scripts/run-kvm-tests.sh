#!/usr/bin/env bash
# Builds the tests of one Go package and runs them with the privileges guest
# networking needs:
#
#   scripts/run-kvm-tests.sh TAG PACKAGE [test binary flags...]
#   scripts/run-kvm-tests.sh integration ./tests/integration -test.v
#
# On Linux every guest network is a bridge with a tap per VM, and the
# firewall tests read and change nftables: the tests need CAP_NET_ADMIN and
# CAP_NET_RAW. They have them when this runs as root or when the caller
# holds them as ambient capabilities (a runner service with
# AmbientCapabilities=). When it has neither and sudo works without a
# password, the test binary runs under sudo. The build never does, and the
# tests get a HOME and a Go build cache of their own under /tmp, removed
# afterwards: nothing owned by root is left in the workspace, in the
# caller's caches or in the runner's temporary directory. Without either,
# the tests run as they are, and with CI set those that need the privilege
# fail saying so.
set -euo pipefail
tag="${1:?usage: run-kvm-tests.sh TAG PACKAGE [test flags...]}"
pkg="${2:?usage: run-kvm-tests.sh TAG PACKAGE [test flags...]}"
shift 2
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${root}"

mkdir -p dist
bin="${root}/dist/$(basename "${pkg}").${tag}.test"
go test -c -tags "${tag}" -o "${bin}" "${pkg}"

run=()
if [ "$(uname -s)" = Linux ] && [ "$(id -u)" -ne 0 ]; then
	probe="jbkvm-s$$"
	if ip link add "${probe}" type dummy 2>/dev/null; then
		ip link del "${probe}"
	elif sudo -n true 2>/dev/null; then
		scratch="$(mktemp -d /tmp/jb-kvm-tests-XXXXXX)"
		cleanup() { sudo -n rm -rf "${scratch}"; }
		trap cleanup EXIT
		# The module cache is the caller's, read only: with GOPROXY=off a
		# build the tests start can use what is there and download nothing.
		run=(sudo -n env "PATH=${PATH}" "HOME=${scratch}" "GOCACHE=${scratch}/go-build"
			"GOMODCACHE=$(go env GOMODCACHE)" GOPROXY=off GOTOOLCHAIN=local
			"CI=${CI:-}" "GITHUB_ACTIONS=${GITHUB_ACTIONS:-}")
		while IFS= read -r kv; do
			run+=("${kv}")
		done < <(env | grep '^JERBOA_' || true)
		echo "run-kvm-tests: no CAP_NET_ADMIN here; running the test binary under sudo"
	else
		echo "run-kvm-tests: no CAP_NET_ADMIN and no passwordless sudo: the tests that need guest networking cannot run" >&2
	fi
fi

cd "${pkg}"
${run[@]+"${run[@]}"} "${bin}" -test.count=1 "$@"
