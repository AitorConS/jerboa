#include <kernel.h>

#include "virtio_internal.h"
#include "virtio_mmio.h"

/* VirtIO input event types and codes from the VirtIO/Linux input ABI. */
#define VIRTIO_INPUT_EV_SYN     0
#define VIRTIO_INPUT_EV_KEY     1
#define VIRTIO_INPUT_KEY_POWER  116
#define VIRTIO_INPUT_EVENT_COUNT 4
#define VIRTIO_INPUT_MMIO_BASE  0x09030000ull
#define VIRTIO_INPUT_MMIO_SIZE  0x1000
#define VIRTIO_INPUT_IRQ        8

struct virtio_input_event {
    u16 type;
    u16 code;
    u32 value;
} __attribute__((packed));

typedef struct virtio_input_buffer {
    struct virtio_input *input;
    struct virtio_input_event *event;
    u64 physical;
    closure_struct(vqfinish, complete);
} *virtio_input_buffer;

struct virtio_input {
    vtdev dev;
    virtqueue eventq;
    struct virtio_input_buffer buffers[VIRTIO_INPUT_EVENT_COUNT];
    boolean powerdown_queued;
    closure_struct(thunk, powerdown);
};

closure_func_basic(thunk, void, virtio_input_powerdown)
{
    kernel_powerdown();
}

static void virtio_input_submit(struct virtio_input *input,
                                struct virtio_input_buffer *buffer)
{
    vqmsg m = allocate_vqmsg(input->eventq);
    assert(m != INVALID_ADDRESS);
    vqmsg_push(input->eventq, m, buffer->physical, sizeof(*buffer->event), true);
    vqmsg_commit(input->eventq, m, (vqfinish)&buffer->complete);
}

closure_func_basic(vqfinish, void, virtio_input_complete,
                   u64 len)
{
    virtio_input_buffer buffer =
        struct_from_closure(virtio_input_buffer, complete);
    struct virtio_input *input = buffer->input;
    boolean power_pressed = len == sizeof(*buffer->event) &&
                            buffer->event->type == VIRTIO_INPUT_EV_KEY &&
                            buffer->event->code == VIRTIO_INPUT_KEY_POWER &&
                            buffer->event->value == 1;

    /* Re-arm before scheduling shutdown so a complete input report is consumed
     * and duplicate reports remain harmless while shutdown is in progress. */
    virtio_input_submit(input, buffer);
    if (power_pressed && !input->powerdown_queued) {
        input->powerdown_queued = true;
        rputs("virtio input: power button shutdown requested\n");
        async_apply((thunk)&input->powerdown);
    }
}

closure_function(2, 1, void, vtmmio_input_probe,
                 heap, general, backed_heap, page_allocator,
                 vtmmio dev)
{
    if (vtmmio_get_u32(dev, VTMMIO_OFFSET_DEVID) != VIRTIO_ID_INPUT ||
            dev->memsize < VTMMIO_OFFSET_CONFIG + 136)
        return;
    if (!attach_vtmmio(bound(general), bound(page_allocator), dev, 0))
        return;

    rputs("virtio input: power button attached\n");

    struct virtio_input *input = allocate_zero(bound(general), sizeof(*input));
    assert(input != INVALID_ADDRESS);
    input->dev = &dev->virtio_dev;
    status s = virtio_alloc_virtqueue(input->dev, ss("virtio input eventq"), 0,
                                     &input->eventq);
    if (!is_ok(s)) {
        msg_err("virtio input failed to attach: %v", s);
        vtdev_set_status(input->dev, VIRTIO_CONFIG_STATUS_FAILED);
        return;
    }

    u64 events_physical;
    struct virtio_input_event *events = alloc_map(bound(page_allocator),
        VIRTIO_INPUT_EVENT_COUNT * sizeof(*events), &events_physical);
    assert(events != INVALID_ADDRESS);
    init_closure_func(&input->powerdown, thunk, virtio_input_powerdown);
    for (int i = 0; i < VIRTIO_INPUT_EVENT_COUNT; i++) {
        struct virtio_input_buffer *buffer = &input->buffers[i];
        buffer->input = input;
        buffer->event = &events[i];
        buffer->physical = events_physical + i * sizeof(*events);
        init_closure_func(&buffer->complete, vqfinish, virtio_input_complete);
    }
    vtdev_set_status(input->dev, VIRTIO_CONFIG_STATUS_DRIVER_OK);
    for (int i = 0; i < VIRTIO_INPUT_EVENT_COUNT; i++)
        virtio_input_submit(input, &input->buffers[i]);
}

void init_virtio_input(kernel_heaps kh)
{
    /* The HVF fork exposes its optional power button at this fixed virt-machine
     * slot. Probe the magic before registering it so QEMU's PL061 device at the
     * same address is left untouched. */
    if (mmio_read_32(mmio_base_addr(GPIO)) != 0x74726976)
        return;
    virtio_mmio_register_device(kh, VIRTIO_INPUT_MMIO_BASE,
                                VIRTIO_INPUT_MMIO_SIZE, VIRTIO_INPUT_IRQ);
    heap general = heap_locked(kh);
    vtmmio_probe_devs(stack_closure(vtmmio_input_probe, general,
                                    heap_linear_backed(kh)));
}
