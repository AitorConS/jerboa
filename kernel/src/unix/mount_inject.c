/*
 * Runtime volume mount injection from QEMU fw_cfg.
 *
 * Reads the "opt/uni/mounts" fw_cfg file and injects a "mounts" tuple into the
 * root tuple so that storage_set_mountpoints() mounts each attached volume at
 * the requested path. This mirrors env_inject/net_inject: the QEMU daemon sets
 * the volume's mount points at run time instead of baking them into the image
 * at build time, so a generic database image can attach any named volume.
 *
 * Format: one "LABEL:/path" entry per line, e.g.
 *   "mysqldata:/var/lib/mysql\npgdata:/var/lib/postgresql/data\n"
 *
 * LABEL is the volume's TFS filesystem label (set by mkfs -l at volume create
 * time); storage's volume_match() resolves it against attached volumes. The
 * path must be absolute.
 *
 * Timing: the kernel reads the manifest "mounts" tuple early (init.c, before
 * the unix process starts), so this runs from stage3 startup and the caller
 * re-invokes storage_set_mountpoints() with the merged tuple. Volumes that
 * attach later are handled by volume_add(), which re-checks storage.mounts.
 */
#include <unix_internal.h>

#if defined(__x86_64__)
#include <drivers/fw_cfg.h>
#endif

/* mounts_inject_from_fw_cfg — read opt/uni/mounts from QEMU fw_cfg (if present)
 * and merge a "mounts" tuple into the root tuple. Returns true if at least one
 * mount entry was added (so the caller knows to (re-)apply the mount points).
 *
 * x86-only: on other architectures this is a no-op stub returning false. */
boolean mounts_inject_from_fw_cfg(tuple root)
{
#if !defined(__x86_64__)
    (void)root;
    return false;
#else
    if (!root)
        return false;

    heap h = heap_locked(get_kernel_heaps());
    buffer raw = fw_cfg_read_file(h, ss("opt/uni/mounts"));
    if (raw == INVALID_ADDRESS)
        return false;
    bytes total = buffer_length(raw);
    if (total == 0) {
        deallocate_buffer(raw);
        return false;
    }

    /* Reuse an existing mounts tuple (e.g. from the manifest) or create one. */
    tuple mounts = get_tuple(root, sym(mounts));
    boolean created_new = false;
    if (!mounts) {
        mounts = allocate_tuple();
        if (mounts == INVALID_ADDRESS) {
            deallocate_buffer(raw);
            return false;
        }
        created_new = true;
    }

    const u8 *data = buffer_ref(raw, 0);
    int added = 0;
    bytes line_start = 0;
    for (bytes i = 0; i <= total; i++) {
        /* Treat end-of-buffer like a newline so the final unterminated entry is
         * parsed too. */
        if (i < total && data[i] != '\n')
            continue;

        bytes line_end = i;
        /* Trim a trailing '\r' (in case of CRLF). */
        if (line_end > line_start && data[line_end - 1] == '\r')
            line_end--;
        if (line_end <= line_start) {
            line_start = i + 1;
            continue;
        }

        /* Split on the first ':' — LABEL before, /path after. */
        bytes colon = line_start;
        boolean found = false;
        for (bytes j = line_start; j < line_end; j++) {
            if (data[j] == ':') {
                colon = j;
                found = true;
                break;
            }
        }
        if (!found || colon == line_start || colon + 1 >= line_end) {
            msg_err("mount_inject: malformed entry (expected LABEL:/path)");
            line_start = i + 1;
            continue;
        }

        bytes label_len = colon - line_start;
        bytes path_start = colon + 1;
        bytes path_len = line_end - path_start;

        symbol s = intern(alloca_wrap_buffer((void *)(data + line_start), label_len));
        string path = allocate_string(path_len);
        if (path == INVALID_ADDRESS) {
            line_start = i + 1;
            continue;
        }
        buffer_append(path, data + path_start, path_len);
        set(mounts, s, path);
        added++;
        msg_info("mount_inject: mount %b at %b", symbol_string(s), path);

        line_start = i + 1;
    }

    if (added > 0)
        set(root, sym(mounts), mounts);
    else if (created_new)
        deallocate_value(mounts);

    deallocate_buffer(raw);
    return added > 0;
#endif
}
