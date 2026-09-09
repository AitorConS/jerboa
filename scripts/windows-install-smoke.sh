#!/usr/bin/env bash
# Post-install smoke test for the Windows *desktop installer* path.
#
# Context: the desktop NSIS installer (JerboaDesktop) ships the jerboa CLI at
# <install>\resources\bin\jerboa.exe and puts it on PATH, but it does NOT bundle
# the engine — the WSL2 distro (jerboad + kernel toolchain) is provisioned at
# runtime. So this script, run *after* the .exe has been silently installed,
# provisions the distro from a freshly-built rootfs (bypassing the R2 download
# via `daemon install --rootfs`, so we exercise the release under test rather
# than current stable), starts jerboad, and boots three example unikernels
# end-to-end.
#
# Assertions avoid Windows->WSL2 host networking (published ports land on the
# daemon host inside the distro, not on localhost of the Windows runner): we
# read VM state and health from the daemon itself via `jerboa ps`.
#
# Required environment:
#   JERBOA_BIN       dir holding the installed jerboa.exe (unix path form)
#   JERBOA_ROOTFS    path to the freshly-built jerboa-rootfs-amd64.tar.gz
#   JERBOA_EXAMPLES  path to the examples/ directory (default: examples)
set -euo pipefail

: "${JERBOA_BIN:?set JERBOA_BIN to the dir holding the installed jerboa.exe}"
: "${JERBOA_ROOTFS:?set JERBOA_ROOTFS to the freshly-built rootfs tarball}"
EXAMPLES="${JERBOA_EXAMPLES:-examples}"

export PATH="${JERBOA_BIN}:${PATH}"

log()  { echo "==> $*"; }
fail() { echo "SMOKE FAILED: $*" >&2; exit 1; }

# wait_healthy <name...>: require every named VM to be running AND healthy
# in the same successful ps response. STATE alone says nothing about readiness.
wait_healthy() {
  local tmp; tmp="$(mktemp)"
  local ready name id
  for _ in $(seq 1 60); do
    ready=true
    if jerboa ps >"$tmp" 2>/dev/null; then
      for name in "$@"; do
        # ps columns: ID NAME STATE HEALTH IMAGE.
        if ! awk -v name="$name" '
          NR>1 && $2==name && $3=="running" && $4=="healthy" { found=1 }
          END { exit !found }
        ' "$tmp"; then
          ready=false
        fi
      done
    else
      ready=false
    fi
    if [ "$ready" = true ]; then rm -f "$tmp"; return 0; fi
    sleep 2
  done
  echo "--- last state/health table for $* ---" >&2; cat "$tmp" >&2 || true
  # Dump each VM's serial console so an unhealthy service explains itself (e.g. a
  # web app that can't reach its database) instead of failing blind. ID is the
  # first ps column; jerboa logs takes it.
  for name in "$@"; do
    id="$(awk -v n="$name" 'NR>1 && $2==n { print $1; exit }' "$tmp")"
    [ -n "$id" ] || continue
    echo "--- serial console: $name ($id) ---" >&2
    jerboa logs "$id" >&2 2>&1 || true
  done
  rm -f "$tmp"
  fail "$* never reached running AND healthy"
}

log "jerboa version"; jerboa version

# ── Provision the engine from the freshly-built bits ──────────────────────────
log "installing distro from ${JERBOA_ROOTFS}"
jerboa daemon install --rootfs "${JERBOA_ROOTFS}" --force
log "starting daemon"
jerboa daemon start
jerboa status || fail "daemon not reachable after start"

cleanup() {
  log "tearing down"
  jerboa compose down "${EXAMPLES}/flask-postgres/stack.yaml" >/dev/null 2>&1 || true
  jerboa stop mongo          >/dev/null 2>&1 || true
  jerboa rm   mongo          >/dev/null 2>&1 || true
  jerboa volume  rm mongodata >/dev/null 2>&1 || true
  jerboa network rm mynet     >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ── 1. hello — boot, print, exit ──────────────────────────────────────────────
# `build --smoke` builds then boots the image once and fails if it does not
# start (or produces no output), then tears the test VM down itself.
log "[1/3] hello"
jerboa build "${EXAMPLES}/hello" --name hello --lang go --smoke \
  || fail "hello: build/smoke boot failed"

# ── 2. flask-postgres — two-VM compose stack on a bridge network ──────────────
log "[2/3] flask-postgres (compose: web + db)"
jerboa build "${EXAMPLES}/postgresql"     --name postgresql                          || fail "postgresql build"
jerboa build "${EXAMPLES}/flask-postgres" --name flask-postgres --pkg-source ops --port 8080 || fail "flask-postgres build"
jerboa compose up "${EXAMPLES}/flask-postgres/stack.yaml"                             || fail "compose up"
# The web HTTP check queries PostgreSQL; require both VMs to be healthy.
# compose up only warns on failed health checks, so enforce them here.
wait_healthy web db
jerboa compose down "${EXAMPLES}/flask-postgres/stack.yaml" || true

# ── 3. mongodb — persistent volume + bridge network, detached ─────────────────
log "[3/3] mongodb (volume + network)"
jerboa network create mynet                 >/dev/null 2>&1 || true
jerboa volume  create mongodata --size 800M >/dev/null 2>&1 || true
jerboa build "${EXAMPLES}/mongodb" --name mongodb || fail "mongodb build"
# --port requires --network; publish 27017 so a real mongod bind is exercised.
jerboa run mongodb:latest --name mongo -d \
  -v mongodata:/data/db --network mynet -p 27017:27017 \
  --health-check tcp:27017 || fail "mongodb run"
wait_healthy mongo

log "OK — all three unikernels validated against the installed CLI"
