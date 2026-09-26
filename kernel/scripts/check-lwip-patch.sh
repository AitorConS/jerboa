#!/bin/sh
# Apply the tracked lwIP patch to an existing clean checkout, or verify that it
# is already applied. Never reset or remove a checkout with local changes.
set -eu

vendor_dir=$1
patch_path=$2
expected_rev=$3
actual_rev=$(git -C "$vendor_dir" rev-parse HEAD)
if [ "$actual_rev" != "$expected_rev" ]; then
    echo "lwIP revision $actual_rev differs from pinned $expected_rev" >&2
    exit 1
fi

# patch(1) can overwrite an untracked file added at a path in the patch.
# Reject that case before either the dry run or the real application.
for path in $(sed -n 's@^diff --git a/[^ ]* b/\([^ ]*\)$@\1@p' "$patch_path"); do
    if [ -e "$vendor_dir/$path" ] &&
       ! git -C "$vendor_dir" ls-files --error-unmatch -- "$path" >/dev/null 2>&1; then
        echo "lwIP patch path $path collides with an untracked file" >&2
        exit 1
    fi
done

if git -C "$vendor_dir" diff HEAD --quiet; then
    if ! patch --dry-run -p1 -d "$vendor_dir" < "$patch_path" >/dev/null; then
        echo "lwIP patch does not apply cleanly to the pinned checkout" >&2
        exit 1
    fi
    patch -p1 -d "$vendor_dir" < "$patch_path"
fi

if ! git -C "$vendor_dir" diff HEAD --binary | cmp - "$patch_path"; then
    echo "lwIP checkout has local changes that differ from the tracked patch" >&2
    exit 1
fi
