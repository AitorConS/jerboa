#!/usr/bin/env bash
# Build Ruby package for unikernel images.
# Compiles Ruby from source with minimal features.
set -euo pipefail

NAME="${PACKAGE_NAME:-ruby}"
VERSION="${PACKAGE_VERSION:-3.2.2}"
SOURCE_URL="${SOURCE_URL:-https://cache.ruby-lang.org/pub/ruby/3.2/ruby-${VERSION}.tar.gz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/ruby.tar.gz" "$SOURCE_URL"
tar -xzf "$TMPDIR/ruby.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/ruby-${VERSION}"
./configure \
  --prefix="$TMPDIR/install" \
  --disable-shared \
  --disable-install-doc \
  --without-gmp \
  --without-jemalloc \
  --with-out-ext=tk,tkutil,cgi \
  2>&1
make -j"$(nproc)" 2>&1 || make 2>&1
make install 2>&1

BINARY="$TMPDIR/install/bin/ruby"
if [ ! -f "$BINARY" ]; then
  echo "Error: ruby binary not found at $BINARY"
  exit 1
fi

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/ruby"
chmod +x "$OUTDIR/ruby"

ldd "$OUTDIR/ruby" 2>/dev/null | grep "=>" | awk '{print $3}' | while read lib; do
  if [ -f "$lib" ]; then
    cp "$lib" "$OUTDIR/"
  fi
done || true

echo "Built ${NAME}:${VERSION}"