/*
 * Runtime network configuration injection from QEMU fw_cfg.
 *
 * Reads the "opt/uni/network" fw_cfg file and injects static IP configuration
 * into the root tuple for init_network_iface() to consume.
 *
 * Format: "IP/CIDR,GATEWAY" (e.g. "10.0.0.2/24,10.0.0.1")
 *
 * The configuration is stored under the first network interface tuple (by
 * convention looked up by name or root-level). We create a "network" sub-tuple
 * under root containing ipaddr, netmask, and gateway as string buffers — this
 * mirrors the Nanos manifest static network config format that init_network_iface
 * already understands.
 */
#include <unix_internal.h>

#if defined(__x86_64__)
#include <drivers/fw_cfg.h>
#endif

/* Parse an IPv4 address string like "10.0.0.2" into a buffer.
 * Returns a heap-allocated string buffer, or INVALID_ADDRESS on failure. */
static buffer parse_ip4_string(heap h, const u8 *s, bytes len)
{
    if (len == 0 || len > 15)   /* max: "255.255.255.255" */
        return INVALID_ADDRESS;

    /* Validate: must contain exactly 3 dots and only digits. */
    int dots = 0;
    for (bytes i = 0; i < len; i++) {
        if (s[i] == '.') {
            dots++;
        } else if (s[i] < '0' || s[i] > '9') {
            return INVALID_ADDRESS;
        }
    }
    if (dots != 3)
        return INVALID_ADDRESS;

    /* No NUL terminator: Nanos ip4addr_aton() takes a length-based sstring
     * (via buffer_to_sstring), so a trailing NUL would be read as a stray
     * non-space character and make aton reject the whole address. */
    buffer buf = allocate_buffer(h, len);
    if (buf == INVALID_ADDRESS)
        return INVALID_ADDRESS;
    buffer_write(buf, s, len);
    return buf;
}

/* Parse a CIDR suffix like "24" and produce a netmask buffer string.
 * Returns a heap-allocated string buffer like "255.255.255.0",
 * or INVALID_ADDRESS on failure. */
static buffer cidr_to_netmask(heap h, int cidr)
{
    if (cidr < 0 || cidr > 32)
        return INVALID_ADDRESS;

    u32 mask = (cidr == 0) ? 0 : ~((1U << (32 - cidr)) - 1);
    /* Format as "A.B.C.D" */
    u8 a = (mask >> 24) & 0xFF;
    u8 b = (mask >> 16) & 0xFF;
    u8 c = (mask >> 8) & 0xFF;
    u8 d = mask & 0xFF;

    buffer buf = allocate_buffer(h, 16);
    if (buf == INVALID_ADDRESS)
        return INVALID_ADDRESS;
    /* No NUL: ip4addr_aton reads this as a length-based sstring (see
     * parse_ip4_string). */
    bprintf(buf, "%d.%d.%d.%d", a, b, c, d);
    return buf;
}

/* net_inject_from_fw_cfg — read opt/uni/network from QEMU fw_cfg (if present)
 * and inject static network configuration into the root tuple.
 *
 * x86-only: on other architectures this is a no-op stub. */
void net_inject_from_fw_cfg(tuple root)
{
#if !defined(__x86_64__)
    (void)root;
    return;
#else
    if (!root)
        return;

    heap h = heap_locked(get_kernel_heaps());
    buffer raw = fw_cfg_read_file(h, ss("opt/uni/network"));
    if (raw == INVALID_ADDRESS)
        return;
    if (buffer_length(raw) == 0) {
        deallocate_buffer(raw);
        return;
    }

    /* Format: "IP/CIDR,GATEWAY"
     * e.g. "10.0.0.2/24,10.0.0.1"
     * We need to extract:
     *   - IP address (before '/')
     *   - CIDR prefix (between '/' and ',')
     *   - Gateway (after ',')
     */
    bytes total = buffer_length(raw);
    const u8 *data = buffer_ref(raw, 0);

    /* Find '/' separator */
    bytes slash_pos = 0;
    boolean found_slash = false;
    for (bytes i = 0; i < total; i++) {
        if (data[i] == '/') {
            slash_pos = i;
            found_slash = true;
            break;
        }
    }
    if (!found_slash) {
        msg_err("net_inject: invalid format in opt/uni/network: no '/' found");
        deallocate_buffer(raw);
        return;
    }

    /* Find ',' separator */
    bytes comma_pos = 0;
    boolean found_comma = false;
    for (bytes i = slash_pos + 1; i < total; i++) {
        if (data[i] == ',') {
            comma_pos = i;
            found_comma = true;
            break;
        }
    }
    if (!found_comma) {
        msg_err("net_inject: invalid format in opt/uni/network: no ',' found");
        deallocate_buffer(raw);
        return;
    }

    /* Parse IP address: data[0..slash_pos) */
    buffer ip_buf = parse_ip4_string(h, data, slash_pos);
    if (ip_buf == INVALID_ADDRESS) {
        msg_err("net_inject: failed to parse IP address");
        deallocate_buffer(raw);
        return;
    }

    /* Parse CIDR: data[slash_pos+1..comma_pos) */
    int cidr = 0;
    for (bytes i = slash_pos + 1; i < comma_pos; i++) {
        if (data[i] < '0' || data[i] > '9') {
            msg_err("net_inject: invalid CIDR prefix");
            deallocate_buffer(raw);
            return;
        }
        cidr = cidr * 10 + (data[i] - '0');
    }

    /* Parse Gateway: data[comma_pos+1..total) */
    bytes gw_start = comma_pos + 1;
    bytes gw_len = 0;
    /* Trim trailing newline/whitespace */
    bytes gw_end = total;
    while (gw_end > gw_start && (data[gw_end - 1] == '\n' || data[gw_end - 1] == '\r' || data[gw_end - 1] == ' '))
        gw_end--;
    gw_len = gw_end - gw_start;

    buffer gw_buf = parse_ip4_string(h, data + gw_start, gw_len);
    if (gw_buf == INVALID_ADDRESS) {
        msg_err("net_inject: failed to parse gateway address");
        deallocate_buffer(ip_buf);
        deallocate_buffer(raw);
        return;
    }

    /* Generate netmask from CIDR */
    buffer mask_buf = cidr_to_netmask(h, cidr);
    if (mask_buf == INVALID_ADDRESS) {
        msg_err("net_inject: invalid CIDR %d", cidr);
        deallocate_buffer(ip_buf);
        deallocate_buffer(gw_buf);
        deallocate_buffer(raw);
        return;
    }

    /* The network tuple is stored at root level so that init_network_iface()
     * finds it when looking for static config for the first interface.
     * Examples of the Nanos manifest format:
     *   (en0:(ipaddr:"10.0.0.2" netmask:"255.255.255.0" gateway:"10.0.0.1"))
     * However, looking at init_network_iface(), if there's no interface tuple
     * it falls back to root. We inject at root level as "ipaddr", "netmask",
     * "gateway" symbols — this matches the fallback path for the en1 default.
     */
    set(root, sym(ipaddr), ip_buf);
    set(root, sym(netmask), mask_buf);
    set(root, sym(gateway), gw_buf);

    msg_info("net_inject: static IP config from fw_cfg: ipaddr=%b netmask=%b gateway=%b",
             ip_buf, mask_buf, gw_buf);

    deallocate_buffer(raw);
#endif
}