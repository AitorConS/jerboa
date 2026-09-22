#!/usr/bin/env bash
# Build Redis package for unikernel images.
# Compiles Redis from source.
set -euo pipefail

NAME="${PACKAGE_NAME:-redis}"
VERSION="${PACKAGE_VERSION:-7.2.4}"
SOURCE_URL="${SOURCE_URL:-https://github.com/redis/redis/archive/refs/tags/v${VERSION}.tar.gz}"

PACKAGE_ROOT="$(pwd)"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/redis.tar.gz" "$SOURCE_URL"
tar -xf "$TMPDIR/redis.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/redis-${VERSION}"
make -j"$(nproc)" 2>&1 || make 2>&1

OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp src/redis-server "$OUTDIR/"
chmod +x "$OUTDIR/redis-server"

echo "Built ${NAME}:${VERSION}"