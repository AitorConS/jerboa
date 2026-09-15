void init_virtio_9p(kernel_heaps kh);
void init_virtio_balloon(kernel_heaps kh);
void init_virtio_blk(kernel_heaps kh, storage_attach a);
void init_virtio_input(kernel_heaps kh);
void init_virtio_network(kernel_heaps kh);
void init_virtio_rng(kernel_heaps kh);
void init_virtio_scsi(kernel_heaps kh, storage_attach a);
void init_virtio_socket(kernel_heaps kh);

void virtio_mmio_enum_devs(kernel_heaps kh);
void virtio_mmio_register_device(kernel_heaps kh, u64 membase, u64 memsize, int irq);
