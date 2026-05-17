#!/usr/bin/env bash
# Build jq package for unikernel images.
# Compiles jq from source with autoreconf.
set -euo pipefail

NAME="${PACKAGE_NAME:-jq}"
VERSION="${PACKAGE_VERSION:-1.7.1}"
SOURCE_URL="${SOURCE_URL:-https://github.com/jqlang/jq/archive/refs/tags/jq-${VERSION}.tar.gz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/jq.tar.gz" "$SOURCE_URL"
tar -xzf "$TMPDIR/jq.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/jq-jq-${VERSION}"
autoreconf -fi 2>&1
./configure --prefix="$TMPDIR/install" --disable-shared --enable-static --disable-maintainer-mode 2>&1
make -j"$(nproc)" 2>&1 || make 2>&1
make install 2>&1

BINARY="$TMPDIR/install/bin/jq"
if [ ! -f "$BINARY" ]; then
  echo "Error: jq binary not found at $BINARY"
  exit 1
fi

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/jq"
chmod +x "$OUTDIR/jq"

ldd "$OUTDIR/jq" 2>/dev/null | grep "=>" | awk '{print $3}' | while read lib; do
  if [ -f "$lib" ]; then
    cp "$lib" "$OUTDIR/"
  fi
done || true

echo "Built ${NAME}:${VERSION}"