#!/usr/bin/env bash
# Build Python runtime package for unikernel images.
# Compiles CPython and collects its runtime libraries.
set -euo pipefail

NAME="${PACKAGE_NAME:-python}"
VERSION="${PACKAGE_VERSION:-3.12.0}"
SOURCE_URL="${SOURCE_URL:-https://www.python.org/ftp/python/${VERSION}/Python-${VERSION}.tar.xz}"

PACKAGE_ROOT="$(pwd)"
# A fixed build directory: compilers and build systems embed it in their
# output, so a random one makes every build different.
TMPDIR="/tmp/jerboa-pkg-${NAME}-${VERSION}"
rm -rf "$TMPDIR" && mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."
echo "Source: ${SOURCE_URL}"

# Download
curl -fSL -o "$TMPDIR/python.tar.xz" "$SOURCE_URL"

# Extract
tar -xf "$TMPDIR/python.tar.xz" -C "$TMPDIR"

# Build with the build container's compiler; static flags also reach extension modules
# and break their shared linking. Package dependent libraries explicitly below.
cd "$TMPDIR/Python-${VERSION}"
./configure --prefix=/usr/local --disable-shared \
  --enable-optimizations=no --with-ensurepip=no
make -j"$(nproc)" 2>&1
make install DESTDIR="$TMPDIR/install" 2>&1

# Locate the python3 binary
BINARY="$TMPDIR/install/usr/local/bin/python3"
if [ ! -f "$BINARY" ]; then
  echo "Error: python3 binary not found at $BINARY"
  find "$TMPDIR/install" -name "python3*" -type f
  exit 1
fi

# Create output directory
OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/python3"
chmod +x "$OUTDIR/python3"
mkdir -p "$OUTDIR/rootfs/usr/local/lib"
cp -R "$TMPDIR/install/usr/local/lib/python${VERSION%.*}" "$OUTDIR/rootfs/usr/local/lib/"
# Build-only relocatable objects are not runtime ELF files and must not ship.
find "$OUTDIR/rootfs" -type f \( -name '*.o' -o -name '*.a' \) -delete
# Drop what a unikernel never runs: the test suite, GUI and editor modules,
# bundled pip wheels, build configuration, and optimized bytecode (only
# loaded under python -O). jerboa pkg create applies the same rules.
PYLIB="$OUTDIR/rootfs/usr/local/lib/python${VERSION%.*}"
rm -rf "$PYLIB"/test "$PYLIB"/idlelib "$PYLIB"/tkinter "$PYLIB"/turtledemo \
  "$PYLIB"/ensurepip "$PYLIB"/lib2to3 "$PYLIB"/pydoc_data "$PYLIB"/config-*
find "$PYLIB" -path '*/__pycache__/*' \( -name '*.opt-1.pyc' -o -name '*.opt-2.pyc' \) -delete
PYTHONHOME="$OUTDIR/rootfs/usr/local" "$OUTDIR/python3" -c 'import json, ssl, zlib; print("Python runtime resources verified")' 

# Collect shared libraries if dynamically linked
ldd "$OUTDIR/python3" 2>/dev/null | grep "=>" | awk '{print $3}' | while read lib; do
  if [ -f "$lib" ]; then
    cp "$lib" "$OUTDIR/"
  fi
done || true

echo "Built ${NAME}:${VERSION} at ${OUTDIR}"