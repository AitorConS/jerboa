#!/usr/bin/env bash
# Build PHP package for unikernel images.
# Compiles PHP from source with minimal configuration.
set -euo pipefail

NAME="${PACKAGE_NAME:-php}"
VERSION="${PACKAGE_VERSION:-8.3.3}"
SOURCE_URL="${SOURCE_URL:-https://www.php.net/distributions/php-${VERSION}.tar.gz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/php.tar.gz" "$SOURCE_URL"
tar -xzf "$TMPDIR/php.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/php-${VERSION}"
./configure \
  --prefix="$TMPDIR/install" \
  --disable-all \
  --disable-cgi \
  --disable-phpdbg \
  --enable-cli \
  --enable-maintainer-zts \
  2>&1
make -j"$(nproc)" 2>&1 || make 2>&1
make install 2>&1

BINARY="$TMPDIR/install/bin/php"
if [ ! -f "$BINARY" ]; then
  echo "Error: php binary not found at $BINARY"
  exit 1
fi

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/php"
chmod +x "$OUTDIR/php"

ldd "$OUTDIR/php" 2>/dev/null | grep "=>" | awk '{print $3}' | while read lib; do
  if [ -f "$lib" ]; then
    cp "$lib" "$OUTDIR/"
  fi
done || true

echo "Built ${NAME}:${VERSION}"