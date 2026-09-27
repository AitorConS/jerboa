/* TCPv4 TX batching for hosts that negotiate both checksum and TSO4.
 * Included after the scalar transmit path so every other frame keeps it. */

#include "virtio_net_gso_packet.h"

/* Copy the complete Ethernet/IP/TCP header even when pbufs are chained. */
static boolean vnet_tcp_parse(struct netif *netif, struct pbuf *p, vnet_tcp_packet *pkt)
{
    u8 *h = pkt->header;
    if (p->tot_len < 54 || p->tot_len > VNET_GSO_MAX_FRAME ||
        p->tot_len > 14 + netif->mtu ||
        pbuf_copy_partial(p, h, 54, 0) != 54)
        return false;
    u8 tcp_len = (h[46] >> 4) * 4;
    if (tcp_len < 20 || tcp_len > 60 ||
        pbuf_copy_partial(p, h, 34 + tcp_len, 0) != 34 + tcp_len)
        return false;
    return vnet_tcp_header_parse(pkt, p->tot_len, netif->mtu);
}

static boolean vnet_tcp_compatible(vnet vn, vnet_gso_batch *b,
                                   const vnet_tcp_packet *pkt)
{
    if (pkt->header_len != b->header_len || pkt->payload_len > b->mss ||
        pkt->seq != b->next_seq || pkt->ip_id != b->next_ip_id ||
        b->count >= VNET_GSO_MAX_SEGMENTS ||
        b->frame_len + pkt->payload_len > VNET_GSO_MAX_FRAME)
        return false;
    const u8 *first = (u8 *)b->buffer + vn->net_header_len;
    return vnet_tcp_header_compatible(first, pkt->header, b->header_len);
}

closure_function(5, 1, void, vnet_gso_complete,
                 vnet, vn, void *, buffer, u64, phys, bytes, alloclen,
                 u64, charge, u64 len)
{
    dealloc_unmap(bound(vn)->dev->contiguous, bound(buffer), bound(phys), bound(alloclen));
    fetch_and_add(&bound(vn)->tx_queued_bytes, -bound(charge));
    closure_finish();
}

static void vnet_gso_abort(vnet vn, vnet_gso_batch *b)
{
    if (b->complete)
        deallocate_closure(b->complete);
    if (b->msg)
        deallocate_vqmsg(b->txq, b->msg);
    if (b->buffer)
        dealloc_unmap(vn->dev->contiguous, b->buffer, b->phys,
                      VNET_GSO_MAX_FRAME + vn->net_header_len);
    fetch_and_add(&vn->tx_queued_bytes, -VNET_GSO_RESERVE);
    b->buffer = 0;
    b->msg = 0;
    b->complete = 0;
    b->count = 0;
}

/* Reserve every fallible resource before the first packet returns ERR_OK.
 * Once staged, flush only edits owned bytes and commits a reserved vqmsg. */
static boolean vnet_gso_start(vnet vn, vnet_gso_batch *b)
{
    b->txq = vn->txq_map[current_cpu()->id];
    if (fetch_and_add(&vn->tx_queued_bytes, VNET_GSO_RESERVE) +
        VNET_GSO_RESERVE > VNET_TX_QUEUE_BYTES) {
        fetch_and_add(&vn->tx_queued_bytes, -VNET_GSO_RESERVE);
        return false;
    }
    b->buffer = alloc_map(vn->dev->contiguous,
                          VNET_GSO_MAX_FRAME + vn->net_header_len, &b->phys);
    b->msg = 0;
    b->complete = 0;
    if (b->buffer == INVALID_ADDRESS) {
        b->buffer = 0;
        vnet_gso_abort(vn, b);
        return false;
    }
    b->msg = allocate_vqmsg(b->txq);
    if (b->msg == INVALID_ADDRESS) {
        b->msg = 0;
        vnet_gso_abort(vn, b);
        return false;
    }
    if (!vqmsg_reserve(b->msg, 2)) {
        vnet_gso_abort(vn, b);
        return false;
    }
    b->complete = closure(vn->dev->general, vnet_gso_complete, vn, b->buffer,
                          b->phys, VNET_GSO_MAX_FRAME + vn->net_header_len,
                          VNET_GSO_RESERVE);
    if (b->complete == INVALID_ADDRESS) {
        b->complete = 0;
        vnet_gso_abort(vn, b);
        return false;
    }
    b->count = 0;
    return true;
}

static void vnet_gso_flush(vnet vn, vnet_gso_batch *b)
{
    if (!b->count)
        return;
    u8 *buffer = b->buffer;
    u8 *frame = buffer + vn->net_header_len;
    vnet_gso_prepare(buffer, vn->net_header_len, frame, b->frame_len,
                     b->header_len, b->mss, b->count);
    /* The buffer is physically contiguous via heap_linear_backed(), which
     * virtio_attach stores in dev->contiguous. The completion owns it now. */
    vqmsg_push(b->txq, b->msg, b->phys, vn->net_header_len, false);
    vqmsg_push(b->txq, b->msg, b->phys + vn->net_header_len,
               b->frame_len, false);
    virtqueue txq = b->txq;
    vqmsg msg = b->msg;
    vqfinish complete = b->complete;
    b->buffer = 0;
    b->msg = 0;
    b->complete = 0;
    b->count = 0;
    vqmsg_commit(txq, msg, complete);
}

static boolean vnet_gso_append(vnet vn, vnet_gso_batch *b, struct netif *netif,
                                struct pbuf *p, const vnet_tcp_packet *pkt)
{
    u8 *frame = (u8 *)b->buffer + vn->net_header_len;
    if (b->count == 0) {
        if (pbuf_copy_partial(p, frame, p->tot_len, 0) != p->tot_len)
            return false;
        b->header_len = pkt->header_len;
        b->mss = pkt->payload_len;
        b->frame_len = p->tot_len;
    } else {
        if (pbuf_copy_partial(p, frame + b->frame_len,
                              pkt->payload_len, pkt->header_len) != pkt->payload_len)
            return false;
        b->frame_len += pkt->payload_len;
    }
    b->count++;
    b->next_seq = pkt->seq + pkt->payload_len; /* modulo 2^32 */
    b->next_ip_id = pkt->ip_id + 1; /* modulo 2^16 */
    MIB2_STATS_NETIF_ADD(netif, ifoutoctets, p->tot_len);
    if (pkt->header[0] & 1)
        MIB2_STATS_NETIF_INC(netif, ifoutnucastpkts);
    else
        MIB2_STATS_NETIF_INC(netif, ifoutucastpkts);
    LINK_STATS_INC(link.xmit);
    return true;
}

/* A context owns its batch until its matching end hook. The short list is
 * protected only for insertion/lookup/removal; no allocation or virtqueue
 * operation occurs under this lock. Distinct PCBs/CPUs cannot share state. */
static vnet_gso_batch *vnet_gso_find(vnet vn, context owner)
{
    vnet_gso_batch *b;
    u64 flags = spin_lock_irq(&vn->gso_lock);
    for (b = vn->gso_batches; b && b->owner != owner; b = b->next);
    spin_unlock_irq(&vn->gso_lock, flags);
    return b;
}

static err_t low_level_output(struct netif *netif, struct pbuf *p);

void virtio_net_tx_batch(struct netif *netif, int begin)
{
    /* IRQ handlers retain the interrupted thread's context pointer. They
     * must neither find nor end its suspended batch. */
    if (in_interrupt() || !netif || netif->linkoutput != low_level_output ||
        !netif->state)
        return;
    vnet vn = netif->state;
    if ((vn->dev->features & (VIRTIO_NET_F_CSUM | VIRTIO_NET_F_HOST_TSO4)) !=
        (VIRTIO_NET_F_CSUM | VIRTIO_NET_F_HOST_TSO4))
        return;
    context owner = get_current_context(current_cpu());
    if (!owner)
        return;
    vnet_gso_batch *b = vnet_gso_find(vn, owner);
    if (begin) {
        if (b) {
            /* Reentrant output from the same context goes scalar until the
             * outer call exits; different contexts retain independent state. */
            vnet_gso_flush(vn, b);
            b->disabled = true;
            assert(b->depth != ~(u64)0);
            b->depth++;
            return;
        }
        if (begin == 2) /* single segment: no deferred batch */
            return;
        b = allocate(vn->dev->general, sizeof(*b));
        if (b == INVALID_ADDRESS)
            return;
        __builtin_memset(b, 0, sizeof(*b));
        b->owner = owner;
        b->depth = 1;
        u64 flags = spin_lock_irq(&vn->gso_lock);
        b->next = vn->gso_batches;
        vn->gso_batches = b;
        spin_unlock_irq(&vn->gso_lock, flags);
        return;
    }
    if (!b)
        return;
    if (--b->depth)
        return;
    u64 flags = spin_lock_irq(&vn->gso_lock);
    vnet_gso_batch **slot = &vn->gso_batches;
    while (*slot && *slot != b)
        slot = &(*slot)->next;
    assert(*slot == b);
    *slot = b->next;
    spin_unlock_irq(&vn->gso_lock, flags);
    vnet_gso_flush(vn, b);
    deallocate(vn->dev->general, b, sizeof(*b));
}

static err_t low_level_output(struct netif *netif, struct pbuf *p)
{
    vnet vn = netif->state;
    if (in_interrupt())
        return low_level_output_scalar(netif, p);
    if ((vn->dev->features & (VIRTIO_NET_F_CSUM | VIRTIO_NET_F_HOST_TSO4)) !=
        (VIRTIO_NET_F_CSUM | VIRTIO_NET_F_HOST_TSO4))
        return low_level_output_scalar(netif, p);
    context owner = get_current_context(current_cpu());
    vnet_gso_batch *b = owner ? vnet_gso_find(vn, owner) : 0;
    if (!b || b->disabled)
        return low_level_output_scalar(netif, p);
    vnet_tcp_packet pkt;
    if (!vnet_tcp_parse(netif, p, &pkt)) {
        vnet_gso_flush(vn, b);
        return low_level_output_scalar(netif, p);
    }
    if (b->count && !vnet_tcp_compatible(vn, b, &pkt))
        vnet_gso_flush(vn, b);
    if (!b->count && !vnet_gso_start(vn, b)) {
        b->disabled = true;
        return low_level_output_scalar(netif, p);
    }
    if (!vnet_gso_append(vn, b, netif, p, &pkt)) {
        if (b->count)
            vnet_gso_flush(vn, b);
        else
            vnet_gso_abort(vn, b);
        b->disabled = true;
        return low_level_output_scalar(netif, p);
    }
    if (b->count >= VNET_GSO_MAX_SEGMENTS || pkt.payload_len < b->mss)
        vnet_gso_flush(vn, b);
    return ERR_OK;
}
