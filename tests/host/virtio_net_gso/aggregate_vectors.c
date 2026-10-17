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

static u16 ref_sum(const u8 *data, size_t len, uint32_t initial)
{
    uint32_t sum = initial;
    for (size_t i = 0; i < len; i += 2)
        sum += (uint32_t)data[i] << 8 | (i + 1 < len ? data[i + 1] : 0);
    while (sum >> 16) sum = (sum & 65535u) + (sum >> 16);
    return (u16)~sum;
}

static u16 ref_tcp_checksum(const u8 *frame, u16 frame_len)
{
    const u8 *ip = frame + 14;
    uint32_t pseudo = 6 + frame_len - 34;
    for (int i = 12; i < 20; i += 2)
        pseudo += (uint32_t)ip[i] << 8 | ip[i + 1];
    return ref_sum(frame + 34, frame_len - 34, pseudo);
}

static void original(u8 *frame, u16 payload, u32 seq, u16 id, u8 salt)
{
    memset(frame, 0, 54 + payload);
    frame[0] = 2; frame[6] = 2; frame[12] = 8;
    u8 *ip = frame + 14;
    ip[0] = 0x45; ip[8] = 64; ip[9] = 6;
    ip[12] = 10; ip[15] = 1; ip[16] = 10; ip[19] = 2;
    vnet_put_be16(ip + 2, 40 + payload);
    vnet_put_be16(ip + 4, id);
    u8 *tcp = ip + 20;
    vnet_put_be16(tcp, 1000);
    vnet_put_be16(tcp + 2, 2000);
    tcp[4] = seq >> 24; tcp[5] = seq >> 16; tcp[6] = seq >> 8; tcp[7] = seq;
    tcp[12] = 0x50; tcp[13] = 0x10; tcp[14] = 0x40;
    for (u16 i = 0; i < payload; i++) frame[54 + i] = (u8)(i * 17 + salt);
    vnet_put_be16(ip + 10, ref_sum(ip, 20, 0));
    vnet_put_be16(tcp + 16, ref_tcp_checksum(frame, 54 + payload));
}

static void segment_from_aggregate(u8 *dest, const u8 *frame,
                                   u16 payload_offset, u16 payload_len,
                                   u16 segment_index)
{
    memcpy(dest, frame, 54);
    memcpy(dest + 54, frame + 54 + payload_offset, payload_len);
    u8 *ip = dest + 14;
    u8 *tcp = ip + 20;
    vnet_put_be16(ip + 2, 40 + payload_len);
    vnet_put_be16(ip + 4, (u16)(vnet_be16(frame + 18) + segment_index));
    vnet_put_be16(ip + 10, 0);
    vnet_put_be16(ip + 10, ref_sum(ip, 20, 0));
    u32 seq = vnet_be32(frame + 38) + payload_offset;
    tcp[4] = seq >> 24; tcp[5] = seq >> 16; tcp[6] = seq >> 8; tcp[7] = seq;
    vnet_put_be16(tcp + 16, 0);
    vnet_put_be16(tcp + 16, ref_tcp_checksum(dest, 54 + payload_len));
}

int main(void)
{
    enum { MSS = 101, TAIL = 37, VNET_LEN = 12 };
    u8 first[54 + MSS], tail[54 + TAIL], aggregate[VNET_LEN + 54 + MSS + TAIL];
    u8 reconstructed[54 + MSS];
    original(first, MSS, UINT32_MAX - 49, UINT16_MAX, 7);
    original(tail, TAIL, 51, 0, 9); /* 0xffffffce + 101 wraps to 51 */
    memcpy(aggregate + VNET_LEN, first, sizeof(first));
    memcpy(aggregate + VNET_LEN + sizeof(first), tail + 54, TAIL);
    vnet_gso_prepare(aggregate, VNET_LEN, aggregate + VNET_LEN,
                     54 + MSS + TAIL, 54, MSS, 2);
    const u8 *vnet = aggregate;
    const u8 *frame = aggregate + VNET_LEN;
    assert(vnet[0] == 1 && vnet[1] == 1);
    assert(vnet_be16((const u8[]){vnet[3], vnet[2]}) == 54);
    assert(vnet_be16((const u8[]){vnet[5], vnet[4]}) == MSS);
    assert(vnet[6] == 34 && vnet[7] == 0 && vnet[8] == 16 && vnet[9] == 0);
    assert(vnet_be16(frame + 16) == 40 + MSS + TAIL);
    assert(ref_sum(frame + 14, 20, 0) == 0);
    assert(vnet_be16(frame + 50) == vnet_tcp_pseudo_seed(frame + 14, 20 + MSS + TAIL));
    segment_from_aggregate(reconstructed, frame, 0, MSS, 0);
    assert(memcmp(reconstructed, first, sizeof(first)) == 0);
    segment_from_aggregate(reconstructed, frame, MSS, TAIL, 1);
    assert(memcmp(reconstructed, tail, sizeof(tail)) == 0);
    memset(aggregate, 0xa5, VNET_LEN);
    vnet_gso_prepare(aggregate, VNET_LEN, first, sizeof(first), 54, MSS, 1);
    for (int i = 0; i < VNET_LEN; i++) assert(aggregate[i] == 0);
    return 0;
}
