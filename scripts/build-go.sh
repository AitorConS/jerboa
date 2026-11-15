#!/usr/bin/env bash
# Build a Go program from its upstream source for unikernel images, with the
# toolchain this workflow pins and the dependency fixes in
# scripts/release/go-floor.txt. The recipe's "go" object in
# scripts/release/packages.json says how (scripts/release/build-go.py).
set -euo pipefail
exec python3 "$(dirname "$0")/release/build-go.py"
