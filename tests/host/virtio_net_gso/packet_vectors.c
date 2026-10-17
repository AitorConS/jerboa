/* Host regression for the production TCPv4 TX GSO implementation. */
#include <assert.h>
#include <stdbool.h>
#include <stdint.h>
#include <string.h>
typedef uint8_t u8;
typedef uint16_t u16;
typedef uint32_t u32;
typedef bool boolean;
#define VNET_GSO_MAX_HEADER 94
#include "virtio_net_gso_packet.h"

static void ip_checksum(vnet_tcp_packet *p)
{
    vnet_put_be16(p->header + 24, 0);
    vnet_put_be16(p->header + 24, vnet_checksum(p->header + 14, 20));
}

static void packet(vnet_tcp_packet *p, u16 payload, u32 seq, u16 id)
{
    memset(p, 0, sizeof(*p));
    u8 *h = p->header;
    h[12] = 0x08; h[13] = 0x00;
    h[14] = 0x45; h[22] = 64; h[23] = 6;
    h[26] = 10; h[29] = 1; h[30] = 10; h[33] = 2;
    vnet_put_be16(h + 16, 40 + payload);
    vnet_put_be16(h + 18, id);
    vnet_put_be16(h + 34, 1000);
    vnet_put_be16(h + 36, 2000);
    h[38] = seq >> 24; h[39] = seq >> 16; h[40] = seq >> 8; h[41] = seq;
    h[46] = 0x50; h[47] = 0x10;
    vnet_put_be16(h + 48, 32768);
    ip_checksum(p);
    assert(vnet_tcp_header_parse(p, 54 + payload, 1460));
    assert(p->payload_len == payload && p->header_len == 54);
}

int main(void)
{
    vnet_tcp_packet first, next;
    packet(&first, 100, UINT32_MAX - 49, UINT16_MAX);
    packet(&next, 20, 50, 0); /* modular TCP seq and IPv4 ID */
    assert(first.seq + first.payload_len == next.seq);
    assert((u16)(first.ip_id + 1) == next.ip_id);
    assert(vnet_tcp_header_compatible(first.header, next.header, 54));
    assert(vnet_checksum(first.header + 14, 20) == 0);
    assert(vnet_tcp_pseudo_seed(first.header + 14, 120) == 0x1481);
    assert(vnet_tcp_pseudo_seed(first.header + 14, 40) == 0x1431);

    /* Every control, push and ECN flag is scalar. */
    const u8 bypass[] = {0x01, 0x02, 0x04, 0x08, 0x20, 0x40, 0x80};
    for (unsigned i = 0; i < sizeof(bypass); i++) {
        packet(&next, 100, 123, 4);
        next.header[47] |= bypass[i];
        assert(!vnet_tcp_header_parse(&next, 154, 1460));
    }
    packet(&next, 100, 123, 4);
    next.header[46] |= 1; /* TCP NS/reserved bits */
    assert(!vnet_tcp_header_parse(&next, 154, 1460));
    packet(&next, 100, 123, 4);
    next.header[20] = 0x20; /* IPv4 MF, with valid IP checksum */
    ip_checksum(&next);
    assert(!vnet_tcp_header_parse(&next, 154, 1460));
    packet(&next, 100, 123, 4);
    next.header[14] = 0x46; /* IPv4 options, with valid IP checksum */
    ip_checksum(&next);
    assert(!vnet_tcp_header_parse(&next, 154, 1460));
    packet(&next, 100, 123, 4);
    next.header[12] = 0x81; /* VLAN */
    assert(!vnet_tcp_header_parse(&next, 154, 1460));
    packet(&next, 100, 123, 4);
    next.header[48] ^= 1; /* advertised TCP window differs */
    assert(!vnet_tcp_header_compatible(first.header, next.header, 54));
    packet(&next, 100, 123, 4);
    next.header[34] ^= 1; /* TCP port differs */
    assert(!vnet_tcp_header_compatible(first.header, next.header, 54));
    packet(&next, 100, 123, 4);
    next.header[46] = 0x60; /* 4-byte TCP option */
    next.header[54] = 1;
    next.header[55] = 1;
    next.header[56] = 1;
    next.header[57] = 1;
    vnet_put_be16(next.header + 16, 144);
    ip_checksum(&next);
    assert(vnet_tcp_header_parse(&next, 158, 1460));
    assert(next.header_len == 58);
    packet(&first, 100, 123, 4);
    first.header[46] = 0x60;
    memset(first.header + 54, 1, 4);
    vnet_put_be16(first.header + 16, 144);
    ip_checksum(&first);
    assert(vnet_tcp_header_parse(&first, 158, 1460));
    assert(vnet_tcp_header_compatible(first.header, next.header, 58));
    next.header[57] = 2;
    assert(!vnet_tcp_header_compatible(first.header, next.header, 58));
    packet(&next, 100, 123, 4);
    assert(!vnet_tcp_header_parse(&next, 65535, 1460));
    return 0;
}
