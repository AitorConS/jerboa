#!/usr/bin/env bash
# Build Valkey package for unikernel images.
# Compiles valkey-server from source, without TLS or systemd support.
set -euo pipefail

NAME="${PACKAGE_NAME:-valkey}"
VERSION="${PACKAGE_VERSION:?}"
SOURCE_URL="${SOURCE_URL:-https://github.com/valkey-io/valkey/archive/refs/tags/${VERSION}.tar.gz}"

PACKAGE_ROOT="$(pwd)"
# A fixed build directory: compilers and build systems embed it in their
# output, so a random one makes every build different.
TMPDIR="/tmp/jerboa-pkg-${NAME}-${VERSION}"
rm -rf "$TMPDIR" && mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/valkey.tar.gz" "$SOURCE_URL"
tar -xf "$TMPDIR/valkey.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/valkey-${VERSION}"
make -j"$(nproc)" BUILD_TLS=no USE_SYSTEMD=no valkey-server 2>&1

OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
install -m 0755 src/valkey-server "$OUTDIR/valkey-server"

echo "Built ${NAME}:${VERSION}"
