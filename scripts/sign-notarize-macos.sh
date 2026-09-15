#!/bin/sh
# Developer ID signing and optional notarization for a staged distribution.
# Nothing is submitted to Apple unless --notarize is explicitly supplied.
set -eu

usage() {
  echo "Usage: $0 --input DIR --output DIR --application-identity NAME [--pkg FILE --installer-identity NAME] [--notarize --notary-profile PROFILE]" >&2
  exit 2
}
input= output= application_identity= pkg= installer_identity= notary_profile=
notarize=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --input) input=${2-}; shift 2 ;;
    --output) output=${2-}; shift 2 ;;
    --application-identity) application_identity=${2-}; shift 2 ;;
    --pkg) pkg=${2-}; shift 2 ;;
    --installer-identity) installer_identity=${2-}; shift 2 ;;
    --notary-profile) notary_profile=${2-}; shift 2 ;;
    --notarize) notarize=true; shift ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
[ -d "$input" ] && [ -n "$output" ] && [ -n "$application_identity" ] || usage
[ ! -e "$output" ] || { echo "output exists; refusing to overwrite: $output" >&2; exit 1; }
if [ -n "$pkg" ]; then
  [ -n "$installer_identity" ] || { echo "--pkg requires --installer-identity" >&2; exit 1; }
  [ ! -e "$pkg" ] || { echo "pkg exists; refusing to overwrite: $pkg" >&2; exit 1; }
fi
if "$notarize"; then
  [ -n "$pkg" ] && [ -n "$notary_profile" ] || { echo "--notarize requires --pkg and --notary-profile" >&2; exit 1; }
fi
for command_name in codesign ditto; do
  command -v "$command_name" >/dev/null || { echo "missing command: $command_name" >&2; exit 1; }
done
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# Validate the complete input tree before codesign is allowed to open any path.
# This rejects file/directory symlinks, special nodes and escaping checksums.
/usr/bin/python3 "$script_dir/verify-macos-distribution.py" "$input" --mode adhoc
ditto "$input" "$output"

# Sign inside-out; all runtime libraries and executables use the same team.
for relative in lib/libintl.8.dylib lib/libglib-2.0.0.dylib bin/tools/mkfs bin/tools/dump bin/jerboa bin/jerboad; do
  codesign --force --options runtime --timestamp --sign "$application_identity" "$output/$relative"
done
codesign --force --options runtime --timestamp --entitlements "$script_dir/macos-firecracker.entitlements" \
  --sign "$application_identity" "$output/bin/firecracker"
for relative in lib/libintl.8.dylib lib/libglib-2.0.0.dylib bin/tools/mkfs bin/tools/dump bin/jerboa bin/jerboad bin/firecracker; do
  codesign --verify --strict --verbose=2 "$output/$relative"
done
/usr/bin/python3 "$script_dir/update-macos-signing-metadata.py" "$output"

if [ -n "$pkg" ]; then
  command -v pkgbuild >/dev/null || { echo "missing command: pkgbuild" >&2; exit 1; }
  package_root=$(mktemp -d "${TMPDIR:-/tmp}/jerboa-pkg.XXXXXX")
  trap 'rm -rf "$package_root"' EXIT HUP INT TERM
  mkdir -p "$package_root/usr/local/libexec"
  ditto "$output" "$package_root/usr/local/libexec/jerboa"
  version=$(/usr/bin/python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$output/distribution-manifest.json")
  pkgbuild --root "$package_root" --identifier dev.jerboa.firecracker --version "$version" \
    --install-location / --sign "$installer_identity" "$pkg"
  pkgutil --check-signature "$pkg"
fi
if "$notarize"; then
  # This is the only external submission in the script and requires explicit opt-in.
  xcrun notarytool submit "$pkg" --keychain-profile "$notary_profile" --wait
  xcrun stapler staple "$pkg"
  xcrun stapler validate "$pkg"
  spctl --assess --type install --verbose=2 "$pkg"
fi
