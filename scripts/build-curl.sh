#!/usr/bin/env bash
# Build curl package for unikernel images.
# Compiles curl from source with minimal features.
set -euo pipefail

NAME="${PACKAGE_NAME:-curl}"
VERSION="${PACKAGE_VERSION:-8.6.0}"
SOURCE_URL="${SOURCE_URL:-https://curl.se/download/curl-${VERSION}.tar.gz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/curl.tar.gz" "$SOURCE_URL"
tar -xzf "$TMPDIR/curl.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/curl-${VERSION}"
./configure \
  --prefix="$TMPDIR/install" \
  --disable-shared \
  --enable-static \
  --without-ssl \
  --without-libpsl \
  --without-brotli \
  --without-zstd \
  --without-nghttp2 \
  --without-libidn2 \
  --without-librtmp \
  2>&1
make -j"$(nproc)" 2>&1 || make 2>&1
make install 2>&1

BINARY="$TMPDIR/install/bin/curl"
if [ ! -f "$BINARY" ]; then
  echo "Error: curl binary not found at $BINARY"
  exit 1
fi

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/curl"
chmod +x "$OUTDIR/curl"

ldd "$OUTDIR/curl" 2>/dev/null | grep "=>" | awk '{print $3}' | while read lib; do
  if [ -f "$lib" ]; then
    cp "$lib" "$OUTDIR/"
  fi
done || true

echo "Built ${NAME}:${VERSION}"