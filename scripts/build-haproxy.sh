#!/usr/bin/env bash
# Build HAProxy package for unikernel images.
# Compiles haproxy from source with OpenSSL and zlib. A unikernel runs one
# process: images start it in the foreground (-db) without master-worker mode.
set -euo pipefail

NAME="${PACKAGE_NAME:-haproxy}"
VERSION="${PACKAGE_VERSION:?}"
SOURCE_URL="${SOURCE_URL:-https://www.haproxy.org/download/${VERSION%.*}/src/haproxy-${VERSION}.tar.gz}"

PACKAGE_ROOT="$(pwd)"
# A fixed build directory: compilers and build systems embed it in their
# output, so a random one makes every build different.
TMPDIR="/tmp/jerboa-pkg-${NAME}-${VERSION}"
rm -rf "$TMPDIR" && mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/haproxy.tar.gz" "$SOURCE_URL"
tar -xf "$TMPDIR/haproxy.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/haproxy-${VERSION}"
make -j"$(nproc)" TARGET=linux-glibc USE_OPENSSL=1 USE_ZLIB=1 USE_PROMEX=1 2>&1

OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
install -m 0755 haproxy "$OUTDIR/haproxy"

echo "Built ${NAME}:${VERSION}"
