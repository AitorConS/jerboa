#ifdef KERNEL
#include <kernel.h>
#include <pagecache.h>
#else
#include <runtime.h>
#endif
#include <storage.h>
#include <tfs.h>

/* On-disk log format. Version 6 adds per-extent initialization maps to uninitialized
 * extents; it is written only by mkfs for new volumes. Existing version 5 volumes stay
 * version 5: no version 6 attribute is ever written to them. Kernels and tools that
 * only know version 5 refuse to mount version 6 when they parse the log header. */
#define TFS_VERSION_V5 0x00000005
#define TFS_VERSION 0x00000006

typedef struct log *log;

typedef struct tfs {
    struct filesystem fs;   /* must be first */
    rangemap storage;
#ifdef KERNEL
    struct spinlock storage_lock;
#endif
    int alignment_order;        /* in blocks */
    int page_order;
    u8 uuid[UUID_LEN];
    char label[VOLUME_LABEL_MAX_LEN];
    table files; // maps tuple to fsfile
    heap dma;
    void *zero_page;
    storage_req_handler req_handler;
    log tl;
    log temp_log;
    u64 next_extend_log_offset;
    u64 next_new_log_offset;
    u64 version;            /* log format of this volume (TFS_VERSION_V5 or TFS_VERSION) */
    u64 map_bytes;          /* memory used by extent page maps */
#ifdef KERNEL
    /* Released storage that the durable log may still reference (see
     * filesystem_release_storage()); protected by the filesystem lock. */
    buffer deferred_frees;      /* awaiting the next release cycle */
    buffer releasing_frees;     /* captured by the release cycle in flight */
    vector release_waiters;     /* allocations retrying after a cycle */
    u64 release_generation;     /* completed cycles that freed storage */
    boolean release_active, release_scheduled;
    /* Data publication (see tfs_pub_register()): the log entries that expose
     * written blocks are staged only after a device flush that follows the
     * completion of those writes. Data writes are counted per generation. */
    struct spinlock pub_lock;   /* inflight, wgen updates, draining */
    u64 inflight[2];            /* data writes in flight per generation */
    int wgen;                   /* generation of new data writes */
    int cycle_gen;              /* generation the cycle in flight publishes */
    u64 pub_cycle;              /* id of the cycle in flight (pub_lock) */
    boolean pub_drained;        /* inflight[cycle_gen] reached 0 (pub_lock) */
    boolean pub_eager;          /* cycle must issue its own flush (pub_lock) */
    boolean pub_flush_issued;   /* that flush was issued (pub_lock) */
    status_handler pub_flush_sh;  /* consumes the status of that flush */
    vector pubs;                /* struct tfs_pub *, pending publications */
    vector pub_waiters;         /* completed by the cycle in flight */
    vector pub_next_waiters;    /* need the next cycle */
    boolean pub_active, pub_error, pub_timer_armed;
    thunk pub_flush;            /* starts the device flush of the cycle */
    struct timer pub_timer;
#endif
} *tfs;

typedef struct tfsfile {
    struct fsfile f;    /* must be first */
    rangemap extentmap;
} *tfsfile;

declare_closure_struct(2, 0, void, free_uninited,
                       heap, h, struct uninited *, u);

declare_closure_struct(2, 1, void, uninited_complete,
                       struct uninited *, u, status_handler, complete,
                       status s);

typedef struct uninited {
    tfs fs;
    struct refcount refcount;
    boolean initialized;
    closure_struct(uninited_complete, complete);
    closure_struct(free_uninited, free);
} *uninited;

typedef struct extent {
    /* these are in block units */
    struct rmnode node;         /* must be first */
    u64 start_block;
    u64 allocated;
    tuple md;                   /* shortcut to extent meta; only published state */
    uninited uninited;
    u32 pubs;                   /* pending publications referring to this extent */
    /* v6 page maps of an uninited extent, one bit per page of the allocation
     * (see tfs_map_*): pages whose data writes were issued, and pages whose
     * data is published in the log (attribute idesc). 0 when unused. */
    u8 *map;
    u8 *map_pub;
    u32 map_pages;
    boolean corrupt;            /* metadata failed validation: I/O fails */
} *extent;

void ingest_extent(tfsfile f, symbol foff, tuple value);

log log_create(heap h, tfs fs, boolean initialize, status_handler sh);
boolean log_write(log tl, tuple t);
boolean log_write_eav(log tl, tuple e, symbol a, value v);
void log_flush(log tl, status_handler completion);
void log_destroy(log tl);
u64 filesystem_allocate_storage(tfs fs, u64 nblocks);
boolean filesystem_reserve_storage(tfs fs, range storage_blocks);
boolean filesystem_free_storage(tfs fs, range storage_blocks);
void filesystem_release_storage(tfs fs, range storage_blocks);
void filesystem_storage_op(tfs fs, sg_list sg, range blocks, boolean write,
                           status_handler completion);

void filesystem_log_rebuild(tfs fs, log new_tl, status_handler sh);
void filesystem_log_rebuild_done(tfs fs, log new_tl);

boolean filesystem_reserve_log_space(tfs fs, u64 *next_offset, u64 *offset, u64 size);

tfsfile allocate_fsfile(tfs fs, tuple md);

#define filesystem_log_blocks(fs) (TFS_LOG_DEFAULT_EXTENSION_SIZE >> (fs)->fs.blocksize_order)
