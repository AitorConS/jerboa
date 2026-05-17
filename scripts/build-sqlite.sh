#!/usr/bin/env bash
# Build SQLite package for unikernel images.
# Downloads the pre-built static binary from sqlite.org.
set -euo pipefail

NAME="${PACKAGE_NAME:-sqlite}"
VERSION="${PACKAGE_VERSION:-3.45.1}"
SOURCE_URL="${SOURCE_URL:-https://sqlite.org/2024/sqlite-tools-linux-x64-${VERSION//./0}.zip}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Building ${NAME}:${VERSION}..."

curl -fSL -o "$TMPDIR/sqlite.zip" "$SOURCE_URL"
unzip -o "$TMPDIR/sqlite.zip" -d "$TMPDIR/sqlite-extract" 2>&1

BINARY="$TMPDIR/sqlite-extract/sqlite3"
if [ ! -f "$BINARY" ]; then
  echo "Error: sqlite3 binary not found at $BINARY"
  ls -la "$TMPDIR/sqlite-extract/"
  exit 1
fi

OUTDIR="dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
cp "$BINARY" "$OUTDIR/sqlite3"
chmod +x "$OUTDIR/sqlite3"

echo "Built ${NAME}:${VERSION}"