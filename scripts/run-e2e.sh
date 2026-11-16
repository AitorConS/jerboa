#!/usr/bin/env bash
# Runs the end-to-end suite (tests/e2e) against the binaries in dist/ and the
# kernel in kernel/output/. Arguments are passed to the test binary, e.g.
#
#   scripts/run-e2e.sh -test.run TestCompose -test.v
#
# On Linux every guest network is a bridge with a tap per VM, and the
# firewall tests read and change nftables: the suite needs CAP_NET_ADMIN and
# CAP_NET_RAW. It has them when it runs as root or when the caller holds
# them as ambient capabilities (a runner service with AmbientCapabilities=).
# When it has neither and sudo works without a password, the test binary
# (not the build) runs under sudo, with a HOME and Go cache of its own so
# that nothing root-owned is left in the caller's. Otherwise the suite runs
# as it is, and with CI set the tests that need the privilege fail saying so.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${root}"

mkdir -p dist
go test -c -tags e2e -o dist/e2e.test ./tests/e2e

run=()
scratch=""
if [ "$(uname -s)" = Linux ] && [ "$(id -u)" -ne 0 ]; then
	probe="jbe2e-s$$"
	if ip link add "${probe}" type dummy 2>/dev/null; then
		ip link del "${probe}"
	elif sudo -n true 2>/dev/null; then
		scratch="$(mktemp -d /tmp/jb-e2e-root-XXXXXX)"
		cleanup() { sudo -n rm -rf "${scratch}"; }
		trap cleanup EXIT
		run=(sudo -n env "PATH=${PATH}" "HOME=${scratch}" "GOCACHE=${scratch}/go-build"
			"GOMODCACHE=$(go env GOMODCACHE)" "CI=${CI:-}"
			"JERBOA_E2E_SLOW=${JERBOA_E2E_SLOW:-}" "JERBOA_E2E_HYPERVISOR=${JERBOA_E2E_HYPERVISOR:-}"
			"JERBOA_E2E_BIN=${JERBOA_E2E_BIN:-}" "JERBOA_E2E_TOOLS=${JERBOA_E2E_TOOLS:-}")
		echo "run-e2e: no CAP_NET_ADMIN here; running the test binary under sudo"
	else
		echo "run-e2e: no CAP_NET_ADMIN and no passwordless sudo: the tests that need guest networking will not be able to run" >&2
	fi
fi

cd tests/e2e
${run[@]+"${run[@]}"} "${root}/dist/e2e.test" -test.count=1 "$@"
