#!/usr/bin/env bash
# Build Node.js runtime package for unikernel images.
# Downloads the static Linux x64 binary from nodejs.org and packages it.
set -euo pipefail

NAME="${PACKAGE_NAME:-node}"
VERSION="${PACKAGE_VERSION:-20.11.0}"
SOURCE_URL="${SOURCE_URL:-https://nodejs.org/dist/v${VERSION}/node-v${VERSION}-linux-x64.tar.xz}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."
echo "Source: ${SOURCE_URL}"

# Download
curl -fSL -o "$TMPDIR/node.tar.xz" "$SOURCE_URL"

# Extract
tar -xJf "$TMPDIR/node.tar.xz" -C "$TMPDIR"

# Locate the node binary
BINARY="$TMPDIR/node-v${VERSION}-linux-x64/bin/node"
if [ ! -f "$BINARY" ]; then
  echo "Error: node binary not found at $BINARY"
  exit 1
fi

# Verify it's a static or dynamically linked binary
file "$BINARY"
ldd "$BINARY" 2>/dev/null || echo "Static binary (ldd skipped)"

# Create output directory
OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/node"
chmod +x "$OUTDIR/node"

echo "Built ${NAME}:${VERSION} at ${OUTDIR}"