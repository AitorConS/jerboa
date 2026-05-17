#!/usr/bin/env bash
# Build Nginx package for unikernel images.
# Compiles nginx from source with minimal modules.
set -euo pipefail

NAME="${PACKAGE_NAME:-nginx}"
VERSION="${PACKAGE_VERSION:-1.24.0}"
SOURCE_URL="${SOURCE_URL:-https://nginx.org/download/nginx-${VERSION}.tar.gz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/nginx.tar.gz" "$SOURCE_URL"
tar -xzf "$TMPDIR/nginx.tar.gz" -C "$TMPDIR"

cd "$TMPDIR/nginx-${VERSION}"
./configure --prefix=/etc/nginx --sbin-path=/usr/sbin/nginx --without-http_rewrite_module --without-http_gzip_module 2>&1
make -j"$(nproc)" 2>&1

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp objs/nginx "$OUTDIR/"
chmod +x "$OUTDIR/nginx"

echo "Built ${NAME}:${VERSION}"