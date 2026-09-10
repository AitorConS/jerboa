/*
 * QEMU fw_cfg driver — reads named files from QEMU's firmware configuration
 * device. Used by uni to inject runtime data (env vars, etc.) into the guest
 * without rebuilding the disk image.
 *
 * Hardware interface (x86):
 *   0x510 (selector, u16, write): select an entry by ID
 *   0x511 (data, u8, read): read selected entry one byte at a time
 *
 * Standard entries we use:
 *   0x0000 SIGNATURE — must read "QEMU" to confirm device is present
 *   0x0019 FILE_DIR  — directory of named files (count + struct entries)
 *
 * uni-specific entries (set by `uni run` via -fw_cfg flag):
 *   "opt/uni/env" — environment variables, "KEY=VAL\nKEY2=VAL2\n" form
 */
#include <kernel.h>
#include "fw_cfg.h"
#if defined(__x86_64__)
#include <io.h>
#endif

#define FW_CFG_PORT_SEL     0x510
#define FW_CFG_PORT_DATA    0x511

#define FW_CFG_SIGNATURE    0x0000
#define FW_CFG_FILE_DIR     0x0019

#define FW_CFG_FILE_NAME_LEN    56

/* QEMU fw_cfg uses big-endian on the wire. */
struct fw_cfg_file {
    u32 size;
    u16 select;
    u16 reserved;
    char name[FW_CFG_FILE_NAME_LEN];
} __attribute__((packed));

static u32 be32_to_cpu(u32 v)
{
    return ((v & 0xff) << 24) | ((v & 0xff00) << 8) |
           ((v & 0xff0000) >> 8) | ((v >> 24) & 0xff);
}

static u16 be16_to_cpu(u16 v)
{
    return (v << 8) | (v >> 8);
}

static void fw_cfg_select(u16 entry)
{
#if defined(__aarch64__)
    /* virt exposes fw_cfg as MMIO; selector is big endian at offset 8. */
    mmio_write_16(mmio_base_addr(FW_CFG) + 8, be16_to_cpu(entry));
#else
    out16(FW_CFG_PORT_SEL, entry);
#endif
}

static void fw_cfg_read(void *buf, u32 len)
{
    u8 *p = buf;
    for (u32 i = 0; i < len; i++)
#if defined(__aarch64__)
        p[i] = mmio_read_8(mmio_base_addr(FW_CFG));
#else
        p[i] = in8(FW_CFG_PORT_DATA);
#endif
}

boolean fw_cfg_present(void)
{
    char sig[4];
    fw_cfg_select(FW_CFG_SIGNATURE);
    fw_cfg_read(sig, 4);
    return sig[0] == 'Q' && sig[1] == 'E' && sig[2] == 'M' && sig[3] == 'U';
}

/*
 * fw_cfg_read_file — locate a named entry, read its contents into a heap buffer.
 * Returns INVALID_ADDRESS if the device is missing or the entry is not found.
 *
 * Caller must deallocate the returned buffer.
 */
buffer fw_cfg_read_file(heap h, sstring name)
{
    if (!fw_cfg_present())
        return INVALID_ADDRESS;

    fw_cfg_select(FW_CFG_FILE_DIR);
    u32 count_be;
    fw_cfg_read(&count_be, sizeof(count_be));
    u32 count = be32_to_cpu(count_be);
    if (count == 0)
        return INVALID_ADDRESS;

    /* Iterate the directory looking for our name. We stream entries: each is
     * 64 bytes; we consume them in order from the data port. */
    u16 selector = 0;
    u32 size = 0;
    boolean found = false;
    for (u32 i = 0; i < count; i++) {
        struct fw_cfg_file e;
        fw_cfg_read(&e, sizeof(e));
        if (found)
            continue;   /* still need to drain the stream to be tidy */
        /* Compare against name (NUL-terminated within 56 bytes). */
        u32 nlen = 0;
        while (nlen < FW_CFG_FILE_NAME_LEN && e.name[nlen] != '\0')
            nlen++;
        if (nlen != name.len)
            continue;
        boolean match = true;
        for (u32 j = 0; j < nlen; j++) {
            if (e.name[j] != name.ptr[j]) {
                match = false;
                break;
            }
        }
        if (!match)
            continue;
        selector = be16_to_cpu(e.select);
        size = be32_to_cpu(e.size);
        found = true;
    }
    if (!found || size == 0)
        return INVALID_ADDRESS;

    buffer b = allocate_buffer(h, size);
    if (b == INVALID_ADDRESS)
        return INVALID_ADDRESS;

    fw_cfg_select(selector);
    fw_cfg_read(buffer_ref(b, 0), size);
    buffer_produce(b, size);
    return b;
}
