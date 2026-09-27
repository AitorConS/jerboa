#ifndef VIRTIO_NET_GSO_PACKET_H
#define VIRTIO_NET_GSO_PACKET_H

/* Pure packet operations shared by the driver and its unit vectors. Callers
 * supply u8/u16/u32 types. No allocation, DMA or lwIP state is referenced. */
typedef struct vnet_tcp_packet {
    u8 header[VNET_GSO_MAX_HEADER];
    u8 header_len;
    u16 payload_len;
    u16 ip_id;
    u32 seq;
} vnet_tcp_packet;

static inline u16 vnet_be16(const u8 *p)
{
    return ((u16)p[0] << 8) | p[1];
}

static inline u32 vnet_be32(const u8 *p)
{
    return ((u32)vnet_be16(p) << 16) | vnet_be16(p + 2);
}

static inline void vnet_put_be16(u8 *p, u16 v)
{
    p[0] = v >> 8;
    p[1] = v;
}

static inline u16 vnet_checksum(const u8 *p, u16 len)
{
    u32 sum = 0;
    for (u16 i = 0; i + 1 < len; i += 2)
        sum += vnet_be16(p + i);
    if (len & 1)
        sum += (u16)p[len - 1] << 8;
    while (sum >> 16)
        sum = (sum & 0xffff) + (sum >> 16);
    return ~sum;
}

/* CHECKSUM_PARTIAL's uncomplemented IPv4 pseudoheader sum. */
static inline u16 vnet_tcp_pseudo_seed(const u8 *ip, u16 tcp_len)
{
    u32 sum = 6 + tcp_len;
    for (u8 i = 12; i < 20; i += 2)
        sum += vnet_be16(ip + i);
    while (sum >> 16)
        sum = (sum & 0xffff) + (sum >> 16);
    return sum;
}

static inline boolean vnet_tcp_header_parse(vnet_tcp_packet *pkt, u16 frame_len, u16 mtu)
{
    const u8 *h = pkt->header;
    if (frame_len < 55 || frame_len > 14 + mtu ||
        vnet_be16(h + 12) != 0x0800 || h[14] != 0x45 || h[23] != 6 ||
        (vnet_be16(h + 20) & 0xbfff) != 0 ||
        vnet_be16(h + 16) != frame_len - 14 ||
        vnet_checksum(h + 14, 20) != 0 ||
        (h[46] & 0x0f) != 0 || h[47] != 0x10)
        return false;
    u8 tcp_len = (h[46] >> 4) * 4;
    if (tcp_len < 20 || tcp_len > 60 || frame_len <= 34 + tcp_len)
        return false;
    pkt->header_len = 34 + tcp_len;
    pkt->payload_len = frame_len - pkt->header_len;
    pkt->ip_id = vnet_be16(h + 18);
    pkt->seq = vnet_be32(h + 38);
    return true;
}

static inline void vnet_put_le16(u8 *p, u16 v)
{
    p[0] = v;
    p[1] = v >> 8;
}

/* Build the exact virtio/TAP header and aggregate IPv4/TCP header. This
 * function is kept allocation-free so host vectors can verify its output. */
static inline void vnet_gso_prepare(u8 *virtio, u8 virtio_len, u8 *frame,
                             u16 frame_len, u8 header_len, u16 mss, u8 count)
{
    __builtin_memset(virtio, 0, virtio_len);
    if (count < 2)
        return;
    u8 *ip = frame + 14;
    u8 *tcp = ip + 20;
    vnet_put_be16(ip + 2, frame_len - 14);
    vnet_put_be16(ip + 10, 0);
    vnet_put_be16(ip + 10, vnet_checksum(ip, 20));
    vnet_put_be16(tcp + 16, vnet_tcp_pseudo_seed(ip, frame_len - 34));
    virtio[0] = 1; /* VIRTIO_NET_HDR_F_NEEDS_CSUM */
    virtio[1] = 1; /* VIRTIO_NET_HDR_GSO_TCPV4 */
    vnet_put_le16(virtio + 2, header_len);
    vnet_put_le16(virtio + 4, mss);
    vnet_put_le16(virtio + 6, 34); /* TCP starts after Ethernet and IPv4 */
    vnet_put_le16(virtio + 8, 16); /* TCP checksum field offset */
}

static inline boolean vnet_tcp_header_compatible(const u8 *first, const u8 *next, u8 len)
{
    for (u8 i = 0; i < len; i++) {
        /* IPv4 length, ID, checksum; TCP sequence and checksum vary. */
        if ((i >= 16 && i <= 19) || (i >= 24 && i <= 25) ||
            (i >= 38 && i <= 41) || (i >= 50 && i <= 51))
            continue;
        if (first[i] != next[i])
            return false;
    }
    return true;
}

#endif
