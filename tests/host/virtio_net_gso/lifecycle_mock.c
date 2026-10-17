/* Host regression for the production TCPv4 TX GSO implementation. */
#include <assert.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
typedef uint8_t u8;
typedef uint16_t u16;
typedef uint32_t u32;
typedef uint64_t u64;
typedef size_t bytes;
typedef bool boolean;
typedef void *context;
typedef void *heap;
typedef void *backed_heap;
#define INVALID_ADDRESS ((void *)-1)
#define ERR_OK 0
#define VNET_GSO_MAX_FRAME 60000
#define VNET_GSO_MAX_SEGMENTS 32
#define VNET_GSO_MAX_HEADER 94
#define VNET_TX_PACKET_OVERHEAD 512
#define VNET_TX_QUEUE_BYTES (4 * 1024 * 1024)
#define VNET_GSO_RESERVE (VNET_GSO_MAX_FRAME + 12 + VNET_TX_PACKET_OVERHEAD)
#define VIRTIO_NET_F_CSUM 1
#define VIRTIO_NET_F_HOST_TSO4 0x800

typedef int err_t;
struct pbuf { struct pbuf *next; u8 *payload; u16 len, tot_len; bool fail_payload_copy; };
struct netif;
typedef err_t (*linkoutput_fn)(struct netif *, struct pbuf *);
struct netif { void *state; linkoutput_fn linkoutput; u16 mtu; };
struct mock_dev { u64 features; heap general; backed_heap contiguous; };
typedef struct mock_dev *vtdev;
struct mock_msg { int reserved, pushed, committed; u64 phys[2]; u32 len[2]; void *complete; };
typedef struct mock_msg *vqmsg;
typedef struct mock_queue { int id; } *virtqueue;
struct mock_closure;
typedef struct mock_closure *vqfinish;
typedef struct vnet_gso_batch {
    struct vnet_gso_batch *next;
    context owner;
    u64 depth;
    boolean disabled;
    u8 count;
    u8 header_len;
    u16 mss;
    u16 next_ip_id;
    u32 next_seq;
    u32 frame_len;
    void *buffer;
    u64 phys;
    vqmsg msg;
    vqfinish complete;
    virtqueue txq;
} vnet_gso_batch;
struct spinlock { int held; };
typedef struct vnet {
    vtdev dev;
    u64 tx_queued_bytes;
    struct spinlock gso_lock;
    vnet_gso_batch *gso_batches;
    bytes net_header_len;
    virtqueue txq_map[1];
} *vnet;
struct mock_closure { vnet vn; void *buffer; u64 phys; bytes alloclen; u64 charge; };
struct mock_cpu { int id; };
static struct mock_cpu mock_cpu;
static context mock_owner;
static bool mock_interrupt;
static int scalar_calls, commits, frees, mapped, unmapped, failed_stage;
static int alloc_count, copy_calls;
static vqmsg committed[64];
static struct mock_dev dev;
static struct mock_queue queue;
static struct vnet vn_storage;
static struct netif netif;
#define current_cpu() (&mock_cpu)
#define get_current_context(cpu) (mock_owner)
#define in_interrupt() (mock_interrupt)
#define fetch_and_add(ptr, val) __sync_fetch_and_add((ptr), (val))
static u64 spin_lock_irq(struct spinlock *l) { assert(!l->held); l->held = 1; return 0; }
static void spin_unlock_irq(struct spinlock *l, u64 flags) { (void)flags; assert(l->held); l->held = 0; }
static void *allocate(heap h, bytes n)
{ (void)h; return failed_stage == 5 ? INVALID_ADDRESS : calloc(1, n); }
static void deallocate(heap h, void *p, bytes n) { (void)h; (void)n; free(p); }
static void *alloc_map(backed_heap h, bytes n, u64 *phys)
{
    (void)h;
    if (failed_stage == 1) return INVALID_ADDRESS;
    void *p = malloc(n);
    if (!p) return INVALID_ADDRESS;
    *phys = (uintptr_t)p;
    mapped++;
    return p;
}
static void dealloc_unmap(backed_heap h, void *p, u64 phys, bytes n)
{ (void)h; (void)phys; (void)n; assert(p); free(p); unmapped++; }
static vqmsg allocate_vqmsg(virtqueue q)
{ (void)q; return failed_stage == 2 ? INVALID_ADDRESS : calloc(1, sizeof(struct mock_msg)); }
static void deallocate_vqmsg(virtqueue q, vqmsg m)
{ (void)q; free(m); frees++; }
static bool vqmsg_reserve(vqmsg m, u32 count)
{ if (failed_stage == 3) return false; m->reserved = count; return true; }
static void vqmsg_push(virtqueue q, vqmsg m, u64 phys, u32 len, bool write)
{ (void)q; assert(!write); assert(m->pushed < m->reserved); m->phys[m->pushed] = phys; m->len[m->pushed++] = len; }
static void vqmsg_commit(virtqueue q, vqmsg m, vqfinish c)
{ (void)q; assert(m->pushed == 2); assert(commits < 64); m->committed = 1; m->complete = c; committed[commits++] = m; }
static struct mock_closure *mock_closure_alloc(vnet vn, void *buffer, u64 phys, bytes alloclen, u64 charge)
{
    if (failed_stage == 4) return INVALID_ADDRESS;
    struct mock_closure *c = malloc(sizeof(*c));
    if (!c) return INVALID_ADDRESS;
    *c = (struct mock_closure){vn, buffer, phys, alloclen, charge};
    return c;
}
static void deallocate_closure(vqfinish c) { free(c); }
#define closure_function(bound_count, arg_count, ret, name, t1, n1, t2, n2, t3, n3, t4, n4, t5, n5, arg) static ret name(void *self, arg)
#define bound(name) (((struct mock_closure *)self)->name)
#define closure(h, name, vn, buffer, phys, alloclen, charge) mock_closure_alloc(vn, buffer, phys, alloclen, charge)
#define closure_finish() free(self)
static u16 pbuf_copy_partial(const struct pbuf *p, void *dest, u16 length, u16 offset)
{
    copy_calls++;
    if (p->fail_payload_copy && offset > 0) return 0;
    u16 copied = 0;
    for (const struct pbuf *q = p; q && copied < length; q = q->next) {
        if (offset >= q->len) { offset -= q->len; continue; }
        u16 n = q->len - offset;
        if (n > length - copied) n = length - copied;
        memcpy((u8 *)dest + copied, q->payload + offset, n);
        copied += n;
        offset = 0;
    }
    return copied;
}
#define MIB2_STATS_NETIF_ADD(netif, field, n) ((void)0)
#define MIB2_STATS_NETIF_INC(netif, field) ((void)0)
#define LINK_STATS_INC(field) ((void)0)
static err_t low_level_output_scalar(struct netif *n, struct pbuf *p)
{ (void)n; (void)p; scalar_calls++; return ERR_OK; }
#include "virtio_net_gso.inc"

static void drain(void)
{
    for (int i = 0; i < commits; i++) {
        vqmsg m = committed[i];
        if (!m) continue;
        vnet_gso_complete(m->complete, 0);
        free(m);
        committed[i] = NULL;
    }
    assert(vn_storage.tx_queued_bytes == 0);
    assert(mapped == unmapped);
}

static void reset(void)
{
    drain();
    assert(!vn_storage.gso_batches);
    memset(&vn_storage, 0, sizeof(vn_storage));
    dev.features = VIRTIO_NET_F_CSUM | VIRTIO_NET_F_HOST_TSO4;
    vn_storage.dev = &dev;
    vn_storage.net_header_len = 12;
    vn_storage.txq_map[0] = &queue;
    netif = (struct netif){&vn_storage, low_level_output, 1460};
    mock_owner = (void *)1;
    mock_interrupt = false;
    scalar_calls = commits = frees = mapped = unmapped = failed_stage = alloc_count = copy_calls = 0;
    memset(committed, 0, sizeof(committed));
}

static void make_packet(u8 *frame, u16 payload, u32 seq, u16 id)
{
    memset(frame, 0, 54 + payload);
    frame[12] = 8;
    frame[14] = 0x45; frame[22] = 64; frame[23] = 6;
    frame[26] = 10; frame[29] = 1; frame[30] = 10; frame[33] = 2;
    vnet_put_be16(frame + 16, 40 + payload);
    vnet_put_be16(frame + 18, id);
    vnet_put_be16(frame + 34, 1000);
    vnet_put_be16(frame + 36, 2000);
    frame[38] = seq >> 24; frame[39] = seq >> 16; frame[40] = seq >> 8; frame[41] = seq;
    frame[46] = 0x50; frame[47] = 0x10;
    for (u16 i = 0; i < payload; i++) frame[54 + i] = i * 7;
    vnet_put_be16(frame + 24, vnet_checksum(frame + 14, 20));
}

static void pair(bool split_chain)
{
    u8 a[154], b[94];
    make_packet(a, 100, 1000, 10);
    make_packet(b, 40, 1100, 11);
    struct pbuf pa = {0, a, sizeof(a), sizeof(a), false};
    struct pbuf pb = {0, b, sizeof(b), sizeof(b), false};
    struct pbuf tail = {0, b + 60, sizeof(b) - 60, sizeof(b) - 60, false};
    if (split_chain) { pb.len = 60; pb.next = &tail; pb.tot_len = sizeof(b); }
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pa) == ERR_OK);
    assert(low_level_output(&netif, &pb) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(commits == 1 && scalar_calls == 0);
    vqmsg m = committed[0];
    assert(m->pushed == 2 && m->len[0] == 12 && m->len[1] == 194);
    assert(m->phys[1] == m->phys[0] + 12);
    u8 *buffer = (u8 *)(uintptr_t)m->phys[0];
    assert(buffer[0] == 1 && buffer[1] == 1 && buffer[4] == 100);
    assert(!memcmp(buffer + 12 + 54, a + 54, 100));
    assert(!memcmp(buffer + 12 + 154, b + 54, 40));
    drain();
}

int main(void)
{
    reset(); pair(true); reset();
    /* A legal large MTU cannot make the first-frame copy exceed its fixed
     * 60,000-byte allocation; oversized frames fall back before allocation. */
    {
        u8 *large = calloc(1, 60001);
        assert(large);
        make_packet(large, 59947, 1000, 10);
        struct pbuf p = {0, large, 60001, 60001, false};
        netif.mtu = 65521;
        virtio_net_tx_batch(&netif, 1);
        assert(low_level_output(&netif, &p) == ERR_OK);
        virtio_net_tx_batch(&netif, 0);
        assert(scalar_calls == 1 && commits == 0 && mapped == 0);
        free(large);
        netif.mtu = 1460;
        reset();
    }
    /* All first-packet resource failures return immediately to scalar. */
    for (int stage = 1; stage <= 4; stage++) {
        u8 a[154]; make_packet(a, 100, 1000, 10);
        struct pbuf pa = {0, a, sizeof(a), sizeof(a), false};
        failed_stage = stage;
        virtio_net_tx_batch(&netif, 1);
        assert(low_level_output(&netif, &pa) == ERR_OK);
        virtio_net_tx_batch(&netif, 0);
        assert(scalar_calls == 1 && commits == 0 && !vn_storage.gso_batches);
        assert(vn_storage.tx_queued_bytes == 0 && mapped == unmapped);
        reset();
    }
    /* Copy failure while appending flushes the accepted first packet and
     * sends only the current packet through the scalar path. */
    u8 append_a[154], append_b[154];
    make_packet(append_a, 100, 1000, 10);
    make_packet(append_b, 100, 1100, 11);
    struct pbuf first_append = {0, append_a, sizeof(append_a), sizeof(append_a), false};
    struct pbuf fail_append = {0, append_b, sizeof(append_b), sizeof(append_b), true};
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &first_append) == ERR_OK);
    assert(low_level_output(&netif, &fail_append) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(commits == 1 && scalar_calls == 1 && !vn_storage.gso_batches);
    reset();
    /* The 32-segment ceiling forces a commit before the scope ends. */
    virtio_net_tx_batch(&netif, 1);
    for (int i = 0; i < VNET_GSO_MAX_SEGMENTS; i++) {
        u8 segment[154];
        make_packet(segment, 100, 1000 + 100 * i, 10 + i);
        struct pbuf p = {0, segment, sizeof(segment), sizeof(segment), false};
        assert(low_level_output(&netif, &p) == ERR_OK);
    }
    assert(commits == 1 && committed[0]->len[1] == 54 + 32 * 100);
    virtio_net_tx_batch(&netif, 0);
    reset();
    /* Queue-budget refusal takes the same scalar path without leaked charge. */
    u8 a[154], b[154]; make_packet(a, 100, 1000, 10); make_packet(b, 100, 1100, 11);
    struct pbuf pa = {0, a, sizeof(a), sizeof(a), false};
    struct pbuf pb = {0, b, sizeof(b), sizeof(b), false};
    vn_storage.tx_queued_bytes = VNET_TX_QUEUE_BYTES;
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pa) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(scalar_calls == 1 && commits == 0 && vn_storage.tx_queued_bytes == VNET_TX_QUEUE_BYTES);
    vn_storage.tx_queued_bytes = 0; reset();
    /* Each required negotiated feature is independently mandatory. */
    const u64 incomplete_features[] = {0, VIRTIO_NET_F_CSUM,
                                       VIRTIO_NET_F_HOST_TSO4};
    for (unsigned i = 0; i < sizeof(incomplete_features) / sizeof(incomplete_features[0]); i++) {
        reset();
        dev.features = incomplete_features[i];
        virtio_net_tx_batch(&netif, 1);
        assert(low_level_output(&netif, &pa) == ERR_OK);
        virtio_net_tx_batch(&netif, 0);
        assert(scalar_calls == 1 && commits == 0 && !vn_storage.gso_batches);
    }
    /* Failure to allocate initial per-context metadata remains scalar. */
    reset();
    failed_stage = 5;
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pa) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(scalar_calls == 1 && commits == 0 && !vn_storage.gso_batches);
    reset();
    /* IRQ must never access a suspended context batch. */
    dev.features = VIRTIO_NET_F_CSUM | VIRTIO_NET_F_HOST_TSO4;
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pa) == ERR_OK);
    mock_interrupt = true;
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pb) == ERR_OK && scalar_calls == 1);
    virtio_net_tx_batch(&netif, 0);
    mock_interrupt = false;
    assert(low_level_output(&netif, &pb) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(commits == 1 && !vn_storage.gso_batches); reset();
    /* Two owners can stage concurrently without cross-flushing. */
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pa) == ERR_OK);
    mock_owner = (void *)2;
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pa) == ERR_OK);
    assert(low_level_output(&netif, &pb) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    mock_owner = (void *)1;
    assert(low_level_output(&netif, &pb) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(commits == 2 && !vn_storage.gso_batches); reset();
    /* Nested scalar scope flushes and disables the outer scope. */
    virtio_net_tx_batch(&netif, 1);
    assert(low_level_output(&netif, &pa) == ERR_OK);
    virtio_net_tx_batch(&netif, 2);
    assert(low_level_output(&netif, &pb) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(low_level_output(&netif, &pb) == ERR_OK);
    virtio_net_tx_batch(&netif, 0);
    assert(commits == 1 && scalar_calls == 2 && !vn_storage.gso_batches); reset();
    puts("lifecycle mock checks passed");
    return 0;
}
