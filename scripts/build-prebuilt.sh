#!/usr/bin/env bash
# Package an upstream project's own release binary, for projects that publish
# a build for each Linux architecture with checksums (most Go and Rust
# servers). The recipe pins each architecture's archive and SHA-256, which the
# workflow checks before this runs; ARCHIVE_MEMBER, from the recipe's
# "member", is the program's path inside the archive, or empty when the
# download is the program itself. The program is installed under the
# package's name.
set -euo pipefail

NAME="${PACKAGE_NAME:?}"
VERSION="${PACKAGE_VERSION:?}"
SOURCE_URL="${SOURCE_URL:?}"
MEMBER="${ARCHIVE_MEMBER:-}"

PACKAGE_ROOT="$(pwd)"
TMPDIR="/tmp/jerboa-pkg-${NAME}-${VERSION}"
rm -rf "$TMPDIR" && mkdir -p "$TMPDIR/src"
trap 'rm -rf "$TMPDIR"' EXIT

# The workflow hands over the verified download under a name of its own, so
# its format is read from its content.
src="$TMPDIR/download"
curl -fsSL -o "$src" "$SOURCE_URL"
case "$(file -b --mime-type "$src")" in
  application/gzip | application/x-gzip) tar -xzf "$src" -C "$TMPDIR/src" ;;
  application/x-xz) tar -xJf "$src" -C "$TMPDIR/src" ;;
  application/zip) unzip -q "$src" -d "$TMPDIR/src" ;;
  application/x-executable | application/x-pie-executable | application/x-sharedlib)
    [ -z "$MEMBER" ] || { echo "Error: the download is a program, but member ${MEMBER} is set" >&2; exit 1; } ;;
  *) echo "Error: unsupported download type $(file -b "$src")" >&2; exit 1 ;;
esac
if [ -n "$MEMBER" ]; then
  case "$MEMBER" in /* | *..*) echo "Error: member must be a relative path: ${MEMBER}" >&2; exit 1 ;; esac
  binary="$TMPDIR/src/${MEMBER}"
else
  binary="$src"
fi
# A link in the archive could point anywhere on the build machine.
[ ! -L "$binary" ] || { echo "Error: ${MEMBER} is a symbolic link" >&2; exit 1; }
[ -f "$binary" ] || { echo "Error: ${MEMBER} not found in the download" >&2; find "$TMPDIR/src" -maxdepth 3 | head -50 >&2; exit 1; }
file "$binary"
case "$(file -b "$binary")" in
  ELF*) ;;
  *) echo "Error: ${MEMBER:-the download} is not an ELF program" >&2; exit 1 ;;
esac

OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
mkdir -p "$OUTDIR"
install -m 0755 "$binary" "$OUTDIR/${NAME}"
echo "Packaged ${NAME}:${VERSION}"
