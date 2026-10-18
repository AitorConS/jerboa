#!/usr/bin/env bash
# Build Node.js runtime package for unikernel images.
# Downloads the official Linux binary for the build machine's architecture
# from nodejs.org and packages it.
set -euo pipefail

NAME="${PACKAGE_NAME:-node}"
VERSION="${PACKAGE_VERSION:-20.11.0}"
case "$(uname -m)" in
  x86_64) NODE_ARCH=x64 ;;
  aarch64|arm64) NODE_ARCH=arm64 ;;
  *) echo "Error: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
SOURCE_URL="${SOURCE_URL:-https://nodejs.org/dist/v${VERSION}/node-v${VERSION}-linux-${NODE_ARCH}.tar.xz}"

PACKAGE_ROOT="$(pwd)"
# A fixed build directory: compilers and build systems embed it in their
# output, so a random one makes every build different.
TMPDIR="/tmp/jerboa-pkg-${NAME}-${VERSION}"
rm -rf "$TMPDIR" && mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."
echo "Source: ${SOURCE_URL}"

# Download
curl -fSL -o "$TMPDIR/node.tar.xz" "$SOURCE_URL"

# Extract
tar -xf "$TMPDIR/node.tar.xz" -C "$TMPDIR"

# Locate the node binary
BINARY="$TMPDIR/node-v${VERSION}-linux-${NODE_ARCH}/bin/node"
if [ ! -f "$BINARY" ]; then
  echo "Error: node binary not found at $BINARY"
  exit 1
fi

# Verify it's a static or dynamically linked binary
file "$BINARY"
ldd "$BINARY" 2>/dev/null || echo "Static binary (ldd skipped)"

# Create output directory
OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/node"
chmod +x "$OUTDIR/node"

echo "Built ${NAME}:${VERSION} at ${OUTDIR}"