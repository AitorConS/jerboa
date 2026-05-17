#!/usr/bin/env bash
# Build Caddy package for unikernel images.
# Compiles Caddy from source using Go.
set -euo pipefail

NAME="${PACKAGE_NAME:-caddy}"
VERSION="${PACKAGE_VERSION:-2.7.6}"
SOURCE_URL="${SOURCE_URL:-https://github.com/caddyserver/caddy/archive/refs/tags/v${VERSION}.tar.gz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/caddy.tar.gz" "$SOURCE_URL"
tar -xzf "$TMPDIR/caddy.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/caddy-${VERSION}"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/caddyserver/caddy/v2.CustomVersion=${VERSION}" -o "$TMPDIR/caddy" ./cmd/caddy 2>&1

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$TMPDIR/caddy" "$OUTDIR/caddy"
chmod +x "$OUTDIR/caddy"

echo "Built ${NAME}:${VERSION}"