#!/usr/bin/env bash
# Build Python runtime package for unikernel images.
# Compiles CPython from source as a static binary using musl.
set -euo pipefail

NAME="${PACKAGE_NAME:-python}"
VERSION="${PACKAGE_VERSION:-3.12.0}"
SOURCE_URL="${SOURCE_URL:-https://www.python.org/ftp/python/${VERSION}/Python-${VERSION}.tar.xz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."
echo "Source: ${SOURCE_URL}"

# Download
curl -fSL -o "$TMPDIR/python.tar.xz" "$SOURCE_URL"

# Extract
tar -xJf "$TMPDIR/python.tar.xz" -C "$TMPDIR"

# Build with musl for static linking
cd "$TMPDIR/Python-${VERSION}"
CPPFLAGS="-static" LDFLAGS="-static" ./configure \
  --prefix="$TMPDIR/install" \
  --disable-shared \
  --enable-optimizations=no \
  --with-ensurepip=no \
  2>&1 || { echo "Configure failed, trying without static flags..."; ./configure --prefix="$TMPDIR/install" --disable-shared --enable-optimizations=no --with-ensurepip=no; }
make -j"$(nproc)" 2>&1
make install 2>&1

# Locate the python3 binary
BINARY="$TMPDIR/install/bin/python3"
if [ ! -f "$BINARY" ]; then
  echo "Error: python3 binary not found at $BINARY"
  find "$TMPDIR/install" -name "python3*" -type f
  exit 1
fi

# Create output directory
OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/python3"
chmod +x "$OUTDIR/python3"

# Collect shared libraries if dynamically linked
ldd "$OUTDIR/python3" 2>/dev/null | grep "=>" | awk '{print $3}' | while read lib; do
  if [ -f "$lib" ]; then
    cp "$lib" "$OUTDIR/"
  fi
done || true

echo "Built ${NAME}:${VERSION} at ${OUTDIR}"