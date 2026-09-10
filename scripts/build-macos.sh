#!/bin/sh
set -eu
with_x86=false
case "${1:-}" in
  "") ;;
  --with-x86) with_x86=true ;;
  *) echo 'Usage: build-macos.sh [--with-x86]' >&2; exit 1 ;;
esac

# Build host tools as Mach-O ARM64 and guests as Linux/AArch64 ELF.
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) ;;
  *) echo 'This build requires macOS on Apple Silicon.' >&2; exit 1 ;;
esac
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
export PATH="/opt/homebrew/bin:$PATH"
for tool in go cc aarch64-elf-as aarch64-elf-ld aarch64-elf-objcopy; do
  command -v "$tool" >/dev/null || { echo "Missing build tool: $tool" >&2; exit 1; }
done
cd "$root"
make -C kernel PLATFORM=virt kernel tools
make -C kernel/platform/virt PLATFORM=virt "$root/kernel/output/platform/virt/boot-stub.img"
dest="$root/dist/macos-arm64"
mkdir -p "$dest/tools"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -o "$dest/jerboa" ./cmd/jerboa
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -o "$dest/jerboad" ./cmd/jerboad
cp kernel/output/tools/bin/mkfs kernel/output/tools/bin/dump "$dest/tools/"
cp kernel/output/platform/virt/bin/kernel.img "$dest/tools/kernel.img"
cp kernel/output/platform/virt/boot-stub.img "$dest/tools/boot.img"
printf 'darwin-arm64\n' > "$dest/tools/platform.txt"
cp kernel/VERSION "$dest/tools/kernel-version.txt"
if "$with_x86" && [ ! -d "$dest/tools/x86" ]; then
  go run scripts/fetch-x86-tools.go "$dest/tools"
fi
file "$dest/jerboa" "$dest/jerboad" "$dest/tools/mkfs" "$dest/tools/kernel.img"
echo "Native build staged in $dest"
