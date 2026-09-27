#!/bin/sh
set -eu
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$here/../../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/virtio-net-gso.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
for test in packet_vectors aggregate_vectors lifecycle_mock; do
    # Mock closures/stats intentionally omit production-only parameters.
    extra=
    if [ "$test" = lifecycle_mock ]; then extra=-Wno-unused-parameter; fi
    "${CC:-cc}" -std=c11 -Wall -Wextra -Werror $extra \
        -fsanitize=address,undefined -fno-omit-frame-pointer \
        -I"$repo/kernel/src/virtio" "$here/$test.c" -o "$work/$test"
    "$work/$test"
    printf '%s: PASS\n' "$test"
done
