#!/usr/bin/env bash
# Build Lua package for unikernel images.
# Compiles Lua from source (simple make build).
set -euo pipefail

NAME="${PACKAGE_NAME:-lua}"
VERSION="${PACKAGE_VERSION:-5.4.6}"
SOURCE_URL="${SOURCE_URL:-https://www.lua.org/ftp/lua-${VERSION}.tar.gz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/lua.tar.gz" "$SOURCE_URL"
tar -xzf "$TMPDIR/lua.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/lua-${VERSION}"
make -j"$(nproc)" linux 2>&1 || make linux 2>&1

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp src/lua "$OUTDIR/"
cp src/luac "$OUTDIR/"
chmod +x "$OUTDIR/lua" "$OUTDIR/luac"

ldd "$OUTDIR/lua" 2>/dev/null | grep "=>" | awk '{print $3}' | while read lib; do
  if [ -f "$lib" ]; then
    cp "$lib" "$OUTDIR/"
  fi
done || true

echo "Built ${NAME}:${VERSION}"