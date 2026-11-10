#!/usr/bin/env bash
# Build memcached package for unikernel images.
# Compiles memcached from source against the build image's libevent.
set -euo pipefail

NAME="${PACKAGE_NAME:-memcached}"
VERSION="${PACKAGE_VERSION:?}"
SOURCE_URL="${SOURCE_URL:-https://memcached.org/files/memcached-${VERSION}.tar.gz}"

PACKAGE_ROOT="$(pwd)"
# A fixed build directory: compilers and build systems embed it in their
# output, so a random one makes every build different.
TMPDIR="/tmp/jerboa-pkg-${NAME}-${VERSION}"
rm -rf "$TMPDIR" && mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/memcached.tar.gz" "$SOURCE_URL"
tar -xf "$TMPDIR/memcached.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/memcached-${VERSION}"
./configure --disable-coverage --disable-docs 2>&1
make -j"$(nproc)" memcached 2>&1

OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
install -m 0755 memcached "$OUTDIR/memcached"

echo "Built ${NAME}:${VERSION}"
