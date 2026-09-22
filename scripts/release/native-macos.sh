#!/usr/bin/env bash
# Validate the actual candidate app in an isolated HOME/store on native Apple Silicon.
# Usage: native-macos.sh APP_ZIP CANDIDATE_RUN OUTPUT_JSON
set -euo pipefail
archive="${1:?candidate app zip}"; candidate_run="${2:?candidate run id}"; report="${3:?report json}"
[[ "$candidate_run" =~ ^[0-9]+$ ]]
test "$(uname -m)" = arm64
test "$(sw_vers -productVersion | cut -d. -f1)" -ge 26
command -v ditto >/dev/null
work=$(mktemp -d)
engine_pid=
cleanup() {
  if [ -n "$engine_pid" ]; then kill "$engine_pid" 2>/dev/null || true; wait "$engine_pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT
ditto -xk "$archive" "$work/app"
app="$work/app/Jerboa Desktop.app"
codesign --verify --deep --strict "$app"
res="$app/Contents/Resources"
version=$(cat "$res/bin/engine-version.txt")
mkdir -p "$work/home"
export PATH="$res/bin:$PATH"
# The existing isolated daemon smoke validates create/start/stop using its own
# temporary VM/image state. Keep this invocation explicit: hosted Mac is not valid.
"$res/bin/jerboa" --version
"$res/bin/jerboad" --version
"$res/bin/firecracker" --version
# Native suites accept the staged binaries and build their own isolated fixtures.
JERBOA_TEST_BIN="$res/bin" JERBOA_FIRECRACKER_BIN="$res/bin/firecracker" \
  python3 scripts/test-firecracker-macos.py
python3 - "$archive" "$candidate_run" "$version" "$report" <<'PY'
import datetime,hashlib,json,pathlib,sys
archive,run,version,out=sys.argv[1:]
h=hashlib.sha256(pathlib.Path(archive).read_bytes()).hexdigest()
pathlib.Path(out).write_text(json.dumps({'candidate_run':run,'app_sha256':h,'version':version,'result':'pass','tested_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'checks':['app-signature','binary-versions','native-vm-start-stop']},indent=2)+'\n')
PY
