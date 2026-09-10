/*
 * Runtime environment-variable injection from QEMU fw_cfg.
 *
 * uni's daemon passes "-fw_cfg name=opt/uni/env,string=KEY=VAL\nKEY2=VAL2\n"
 * to QEMU when the user invokes "uni run -e KEY=VAL". On boot, before the
 * user program starts, we read that file (if present) and merge the entries
 * into the manifest's "environment" tuple, where exec_elf will pick them up
 * and push them onto the user stack as envp.
 *
 * Format on the wire: lines separated by '\n'; each line "KEY=VALUE".
 * Trailing newlines and empty lines are ignored; lines without '=' are
 * skipped. Existing entries with the same key are overwritten.
 */
#include <unix_internal.h>

#if defined(__x86_64__) || defined(__aarch64__)
#include <drivers/fw_cfg.h>
#endif

/* Walk b from offset *idx to the next '\n' (or end). Returns true if a
 * non-empty line was extracted; updates *line_start and *line_len.
 * On exit, *idx points to the first character after the consumed '\n'. */
static boolean next_line(buffer b, bytes *idx, bytes *line_start, bytes *line_len)
{
    bytes total = buffer_length(b);
    bytes start = *idx;
    while (start < total && byte(b, start) == '\n')
        start++;
    if (start >= total)
        return false;
    bytes end = start;
    while (end < total && byte(b, end) != '\n')
        end++;
    *line_start = start;
    *line_len = end - start;
    *idx = (end < total) ? end + 1 : end;
    return *line_len > 0;
}

/* Parse one "KEY=VALUE" segment from b[start..start+len) and add it to env.
 * Returns true if a valid pair was added. */
static boolean inject_pair(tuple env, buffer b, bytes start, bytes len)
{
    /* Locate '=' */
    bytes eq = 0;
    boolean found = false;
    for (bytes i = 0; i < len; i++) {
        if (byte(b, start + i) == '=') {
            eq = i;
            found = true;
            break;
        }
    }
    if (!found || eq == 0)
        return false;

    /* Key → symbol via intern (deduped against the symbol table).
     * Hoist buffer_ref out of alloca_wrap_buffer: the macro declares its own
     * local `buffer b`, which would shadow our parameter mid-expansion. */
    void *kptr = buffer_ref(b, start);
    symbol key = intern(alloca_wrap_buffer(kptr, eq));

    /* Value → heap-allocated string buffer (env reader expects a buffer). */
    heap h = heap_locked(get_kernel_heaps());
    bytes vlen = len - eq - 1;
    buffer val = allocate_buffer(h, vlen + 1);
    if (val == INVALID_ADDRESS)
        return false;
    if (vlen > 0)
        buffer_write(val, buffer_ref(b, start + eq + 1), vlen);

    set(env, key, val);
    return true;
}

/* env_inject_from_fw_cfg — read opt/uni/env from QEMU fw_cfg (if any) and
 * merge into root[environment]. Safe no-op if the device is absent or the
 * entry is empty. fw_cfg is x86/ARM64; on other architectures this is a stub. */
void env_inject_from_fw_cfg(tuple root)
{
#if !defined(__x86_64__) && !defined(__aarch64__)
    (void)root;
    return;
#else
    if (!root)
        return;
    heap h = heap_locked(get_kernel_heaps());
    buffer raw = fw_cfg_read_file(h, ss("opt/uni/env"));
    if (raw == INVALID_ADDRESS)
        return;
    if (buffer_length(raw) == 0) {
        deallocate_buffer(raw);
        return;
    }

    /* Get or create the environment tuple. */
    tuple env = get_tuple(root, sym(environment));
    if (!env) {
        env = allocate_tuple();
        if (env == INVALID_ADDRESS) {
            deallocate_buffer(raw);
            return;
        }
        set(root, sym(environment), env);
    }

    bytes idx = 0, lstart, llen;
    int injected = 0;
    while (next_line(raw, &idx, &lstart, &llen)) {
        if (inject_pair(env, raw, lstart, llen))
            injected++;
    }
    msg_info("env_inject: %d var(s) from fw_cfg opt/uni/env", injected);
    deallocate_buffer(raw);
#endif
}
