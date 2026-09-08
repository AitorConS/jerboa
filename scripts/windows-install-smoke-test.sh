#!/usr/bin/env bash
# Exercise the complete smoke script without Windows, WSL, or real VMs.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/jerboa" <<'MOCK'
#!/usr/bin/env bash
set -eu
case "$1 ${2:-}" in
  "compose up")
    echo 'warning: compose health checks are not enforced by compose up' >&2 ;;
  "run mongodb:latest")
    case " $* " in
      *" --health-check tcp:27017 "*) ;;
      *) echo 'MongoDB needs a readiness check' >&2; exit 1 ;;
    esac ;;
  "ps ")
    echo 'ID NAME STATE HEALTH IMAGE'
    web_health=healthy; db_state=running; mongo_health=healthy
    case "$SCENARIO" in
      unhealthy-web) web_health=unhealthy ;;
      stopped-db) db_state=stopped ;;
      unhealthy-mongo) mongo_health=unhealthy ;;
      starting)
        if [ ! -f "$MOCK_READY" ]; then
          touch "$MOCK_READY"
          web_health=starting
        fi ;;
    esac
    printf 'web-id web running %s flask-postgres:latest\n' "$web_health"
    if [ "$SCENARIO" != missing-db ]; then
      printf 'db-id db %s healthy postgresql:latest\n' "$db_state"
    fi
    printf 'mongo-id mongo running %s mongodb:latest\n' "$mongo_health"
    # Even plausible output must be ignored when the command failed.
    if [ "$SCENARIO" = failed-ps ]; then exit 1; fi ;;
esac
MOCK
printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/sleep"
chmod +x "$tmp/jerboa" "$tmp/sleep"

for scenario in healthy starting unhealthy-web stopped-db missing-db unhealthy-mongo failed-ps; do
  result=0
  SCENARIO="$scenario" MOCK_READY="$tmp/ready" \
    JERBOA_BIN="$tmp" JERBOA_ROOTFS=mock-rootfs \
    bash "$here/windows-install-smoke.sh" >"$tmp/output" 2>&1 || result=$?
  case "$scenario" in
    healthy|starting) expected=0 ;;
    *) expected=1 ;;
  esac
  if [ "$result" -ne "$expected" ]; then
    cat "$tmp/output" >&2
    echo "FAIL: $scenario exited $result (expected $expected)" >&2
    exit 1
  fi
  echo "PASS: $scenario"
done
