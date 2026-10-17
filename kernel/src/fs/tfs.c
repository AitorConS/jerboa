#include <errno.h>
#include <tfs_internal.h>
#ifdef KERNEL
#include <dma.h>
#endif

//#define TFS_DEBUG
#if defined(TFS_DEBUG)
#ifdef KERNEL
#define tfs_debug(x, ...) do {tprintf(sym(tfs), 0, ss(x), ##__VA_ARGS__);} while(0)
#else
#define tfs_debug(x, ...) do {rprintf("TFS: " x, ##__VA_ARGS__);} while(0)
#endif
#else
#define tfs_debug(x, ...)
#endif

#ifdef BOOT
#define TFS_READ_ONLY
#endif

#ifdef KERNEL

#define tfs_storage_lock(fs)    spin_lock(&(fs)->storage_lock)
#define tfs_storage_unlock(fs)  spin_unlock(&(fs)->storage_lock)

#else

#define tfs_storage_lock(fs)    ((void)fs)
#define tfs_storage_unlock(fs)  ((void)fs)

#endif

#define fs_is_tfs(fs)   ((fs)->get_meta == tmpfs_get_meta)

#define tfs_from_file(f)    ((tfs)((f)->f.fs))

/* Called with fs locked */
static tuple tmpfs_get_meta(filesystem fs, inode n)
{
    return fs_tuple_from_inode(((tfs)fs)->files, n);
}

static s64 tfsfile_get_blocks(fsfile f)
{
    s64 blocks = 0;
    rangemap_foreach(((tfsfile)f)->extentmap, n) {
        blocks += range_span(n->r);
    }
    return blocks;
}

void fixup_directory(tuple parent, tuple dir);

closure_function(1, 2, boolean, fixup_directory_each,
                 tuple, dir,
                 value s, value v)
{
    if (is_tuple(v))
        fixup_directory(bound(dir), v);
    return true;
}

void fixup_directory(tuple parent, tuple n)
{
    tuple c = children(n);
    if (c) {
        iterate(c, stack_closure(fixup_directory_each, n));
        set(n, sym(..), parent);
    }
}

static inline boolean ingest_parse_int(tuple e, symbol a, u64 * i)
{
    return get_u64(e, a, i);
}

static inline extent allocate_extent(heap h, range file_blocks, range storage_blocks)
{
    extent e = allocate(h, sizeof(struct extent));
    if (e == INVALID_ADDRESS)
        return e;
    rmnode_init(&e->node, file_blocks);
    e->start_block = storage_blocks.start;
    e->allocated = range_span(storage_blocks);
    e->uninited = 0;
    e->pubs = 0;
    e->map = e->map_pub = 0;
    e->map_pages = 0;
    e->corrupt = false;
    return e;
}

/* v6 page maps.
 *
 * On a v6 volume a write into an uninited extent does not zero-fill the
 * extent: it writes its pages (zeroing only the uncovered blocks of partial
 * pages) and marks them in ex->map. Once their data is durable, the pages are
 * published in ex->map_pub and in the log as attribute idesc:
 *   "ID6\0" | u32 pages | u64 start_block | bitmap | u64 fnv1a-64 of all before
 * Pages without a bit read as zeros. A map is sized by the allocation, and an
 * extent with a map never grows, so the last page's bit never covers blocks
 * that were outside the extent when they were written. An idesc that fails
 * validation marks the extent corrupt: its reads and writes fail, they never
 * return zeros or stale blocks. */
#define TFS_IDESC_MAGIC 0x00364449  /* "ID6\0" */

static inline u64 tfs_blocks_per_page(tfs fs)
{
    return U64_FROM_BIT(fs->page_order - fs->fs.blocksize_order);
}

static inline u32 tfs_map_pages_for(tfs fs, u64 blocks)
{
    return (blocks + tfs_blocks_per_page(fs) - 1) / tfs_blocks_per_page(fs);
}

static inline boolean map_bit(u8 *map, u64 p)
{
    return (map[p >> 3] >> (p & 7)) & 1;
}

static inline void map_set(u8 *map, u64 p)
{
    map[p >> 3] |= 1 << (p & 7);
}

static u64 tfs_fnv1a(u8 *p, u64 n)
{
    u64 h = 0xcbf29ce484222325ull;
    for (u64 i = 0; i < n; i++) {
        h ^= p[i];
        h *= 0x100000001b3ull;
    }
    return h;
}

/* Pages of the extent covered by its length. */
static inline u32 tfs_map_used_pages(tfs fs, extent ex)
{
    return tfs_map_pages_for(fs, range_span(ex->node.r));
}

static boolean tfs_map_alloc(tfs fs, extent ex, boolean capped)
{
    u32 pages = tfs_map_pages_for(fs, ex->allocated);
    u64 bytes = (pages + 7) / 8;
#ifdef KERNEL
    /* Over the cap, writes fall back to converting the whole extent. */
    if (capped && fs->map_bytes + 2 * bytes > TFS_MAP_MEMORY_LIMIT)
        return false;
#endif
    u8 *map = allocate_zero(fs->fs.h, bytes);
    if (map == INVALID_ADDRESS)
        return false;
    u8 *pub = allocate_zero(fs->fs.h, bytes);
    if (pub == INVALID_ADDRESS) {
        deallocate(fs->fs.h, map, bytes);
        return false;
    }
    ex->map = map;
    ex->map_pub = pub;
    ex->map_pages = pages;
    fs->map_bytes += 2 * bytes;
    return true;
}

#ifndef TFS_READ_ONLY
static void tfs_map_free(tfs fs, extent ex)
{
    if (!ex->map)
        return;
    u64 bytes = (ex->map_pages + 7) / 8;
    deallocate(fs->fs.h, ex->map, bytes);
    deallocate(fs->fs.h, ex->map_pub, bytes);
    ex->map = ex->map_pub = 0;
    ex->map_pages = 0;
    fs->map_bytes -= 2 * bytes;
}
#endif

/* Load an idesc read from the log; false if it fails validation. */
static boolean tfs_map_load(tfs fs, extent ex, string s)
{
    u8 *p = buffer_ref(s, 0);
    u64 len = buffer_length(s);
    if (len < 4 + 4 + 8 + 8)
        return false;
    u32 magic, pages;
    u64 start, sum;
    runtime_memcpy(&magic, p, 4);
    runtime_memcpy(&pages, p + 4, 4);
    runtime_memcpy(&start, p + 8, 8);
    u64 bytes = (pages + 7) / 8;
    /* A shrink keeps the map of the larger allocation. */
    if (magic != TFS_IDESC_MAGIC || len != 16 + bytes + 8 || start != ex->start_block ||
        pages < tfs_map_used_pages(fs, ex))
        return false;
    runtime_memcpy(&sum, p + 16 + bytes, 8);
    if (sum != tfs_fnv1a(p, 16 + bytes))
        return false;
    if (!tfs_map_alloc(fs, ex, false))
        return false;
    u32 used = MIN(tfs_map_used_pages(fs, ex), ex->map_pages);
    for (u32 i = 0; i < used; i++)
        if (map_bit(p + 16, i)) {
            map_set(ex->map, i);
            map_set(ex->map_pub, i);
        }
    return true;
}

#ifdef KERNEL
static string tfs_map_encode(tfs fs, extent ex)
{
    u32 pages = ex->map_pages;
    u64 bytes = (pages + 7) / 8;
    string s = allocate_string(16 + bytes + 8);
    if (s == INVALID_ADDRESS)
        return s;
    u32 magic = TFS_IDESC_MAGIC;
    u64 start = ex->start_block;
    buffer_write(s, &magic, 4);
    buffer_write(s, &pages, 4);
    buffer_write(s, &start, 8);
    buffer_write(s, ex->map_pub, bytes);
    u64 sum = tfs_fnv1a(buffer_ref(s, 0), 16 + bytes);
    buffer_write(s, &sum, 8);
    return s;
}
#endif


closure_function(2, 1, boolean, tfs_storage_alloc,
                 u64, nblocks, u64 *, start_block,
                 range r)
{
    if (range_span(r) >= bound(nblocks)) {
        *bound(start_block) = r.start;
        return false;
    }
    return true;
}

u64 filesystem_allocate_storage(tfs fs, u64 nblocks)
{
    if (fs->storage) {
        tfs_storage_lock(fs);
        u64 start_block;
        int result = rangemap_range_find_gaps(fs->storage,
                                              irange(0, fs->fs.size >> fs->fs.blocksize_order),
                                              stack_closure(tfs_storage_alloc,
                                                            nblocks, &start_block));
        boolean success = (result == RM_ABORT) &&
                          rangemap_insert_range(fs->storage, irangel(start_block, nblocks));
        tfs_storage_unlock(fs);
        if (success)
            return start_block;
    }
    return INVALID_PHYSICAL;
}

boolean filesystem_reserve_storage(tfs fs, range blocks)
{
    if (fs->storage) {
        tfs_storage_lock(fs);
        boolean success = !rangemap_range_intersects(fs->storage, blocks) &&
                          rangemap_insert_range(fs->storage, blocks);
        tfs_storage_unlock(fs);
        return success;
    }
    return true;
}

boolean filesystem_free_storage(tfs fs, range blocks)
{
    if (fs->storage) {
        tfs_storage_lock(fs);
        boolean success = rangemap_insert_hole(fs->storage, blocks);
        tfs_storage_unlock(fs);
        return success;
    }
    return true;
}

#ifdef KERNEL
/* Storage released by truncate, unlink or log compaction may still be referenced
 * by the durable log until the log entries releasing it are written and the
 * device is flushed. Reusing it earlier would let a crash leave the old file
 * pointing at another file's new data. Released ranges therefore stay allocated
 * until a release cycle has flushed the log and then the device. The caller
 * must hold the filesystem lock and have logged the release already. */
static void tfs_release_schedule(tfs fs);
static void tfs_device_flush(tfs fs, status_handler sh);

static void tfs_release_wake(tfs fs)
{
    status_handler sh;
    vector_foreach(fs->release_waiters, sh)
        async_apply_status_handler(sh, STATUS_OK);
    vector_clear(fs->release_waiters);
}

closure_function(1, 1, void, tfs_release_flushed,
                 tfs, fs,
                 status s)
{
    tfs fs = bound(fs);
    filesystem_lock(&fs->fs);
    range r;
    while (buffer_read(fs->releasing_frees, &r, sizeof(r))) {
        if (!is_ok(s)) {
            /* Not durable: keep the range allocated; a later cycle retries. */
            if (!buffer_write(fs->deferred_frees, &r, sizeof(r)))
                msg_err("TFS: cannot retain released range %R; leaving it allocated", r);
        } else if (!filesystem_free_storage(fs, r)) {
            msg_err("TFS: failed to free released range %R", r);
        }
    }
    fs->release_active = false;
    if (is_ok(s)) {
        fs->release_generation++;
        if (buffer_length(fs->deferred_frees))
            tfs_release_schedule(fs);
    } else {
        msg_err("TFS: storage release not durable, retained: %v", s);
        timm_dealloc(s);
    }
    tfs_release_wake(fs);
    filesystem_unlock(&fs->fs);
    filesystem_release(&fs->fs);
    closure_finish();
}

closure_function(1, 1, void, tfs_release_logged,
                 tfs, fs,
                 status s)
{
    tfs fs = bound(fs);
    status_handler flushed = closure(fs->fs.h, tfs_release_flushed, fs);
    assert(flushed != INVALID_ADDRESS);
    if (is_ok(s)) {
        tfs_device_flush(fs, flushed);
    } else {
        apply(flushed, timm_clone(s));
    }
    closure_finish();
}

closure_function(1, 0, void, tfs_release_start,
                 tfs, fs)
{
    tfs fs = bound(fs);
    filesystem_lock(&fs->fs);
    fs->release_scheduled = false;
    if (!fs->release_active) {
        if (buffer_length(fs->deferred_frees)) {
            status_handler logged = closure(fs->fs.h, tfs_release_logged, fs);
            if (logged != INVALID_ADDRESS) {
                /* Every range here was released after its log entries were
                 * staged, so this flush writes them. */
                buffer b = fs->releasing_frees;
                fs->releasing_frees = fs->deferred_frees;
                fs->deferred_frees = b;
                fs->release_active = true;
                filesystem_reserve(&fs->fs);
                log_flush(fs->tl, logged);
            }
        } else {
            tfs_release_wake(fs);
        }
    }
    filesystem_unlock(&fs->fs);
    filesystem_release(&fs->fs);
    closure_finish();
}

static void tfs_release_schedule(tfs fs)
{
    if (fs->release_scheduled || fs->release_active)
        return;
    thunk t = closure(fs->fs.h, tfs_release_start, fs);
    if (t == INVALID_ADDRESS)
        return;     /* retried with the next release or allocation failure */
    fs->release_scheduled = true;
    filesystem_reserve(&fs->fs);
    async_apply(t);
}

void filesystem_release_storage(tfs fs, range blocks)
{
    if (!fs->storage || range_empty(blocks))
        return;
    if (!buffer_write(fs->deferred_frees, &blocks, sizeof(blocks))) {
        msg_err("TFS: cannot defer release of %R; leaving it allocated", blocks);
        return;
    }
    tfs_release_schedule(fs);
}

closure_function(1, 1, void, tfs_release_wait,
                 tfs, fs,
                 status_handler complete)
{
    tfs fs = bound(fs);
    filesystem_lock(&fs->fs);
    vector_push(fs->release_waiters, complete);
    tfs_release_schedule(fs);
    filesystem_unlock(&fs->fs);
}

static boolean tfs_status_enospc(status s)
{
    s64 fss;
    return !is_ok(s) && get_s64(s, sym(fsstatus), &fss) && fss == -ENOSPC;
}
#else
void filesystem_release_storage(tfs fs, range blocks)
{
    if (!filesystem_free_storage(fs, blocks))
        msg_err("TFS: failed to free released range %R", blocks);
}
#endif

void ingest_extent(tfsfile f, symbol off, tuple value)
{
    tfs_debug("ingest_extent: f %p, off %b, value %v\n", f, symbol_string(off), value);
    u64 length, file_offset, start_block, allocated;
    assert(off);
    assert(parse_int(alloca_wrap(symbol_string(off)), 10, &file_offset));
    assert(ingest_parse_int(value, sym(length), &length));
    assert(ingest_parse_int(value, sym(offset), &start_block));
    assert(ingest_parse_int(value, sym(allocated), &allocated));
    tfs_debug("   file offset %ld, length %ld, start_block 0x%lx, allocated %ld\n",
              file_offset, length, start_block, allocated);

    range storage_blocks = irangel(start_block, allocated);
    tfs fs = tfs_from_file(f);
    if (!filesystem_reserve_storage(fs, storage_blocks)) {
        /* soft error... */
        msg_err("TFS %s: unable to reserve storage blocks %R", func_ss, storage_blocks);
    }
    range r = irangel(file_offset, length);
    extent ex = allocate_extent(fs->fs.h, r, storage_blocks);
    if (ex == INVALID_ADDRESS)
        halt("out of memory\n");
    ex->md = value;
    if (get(value, sym(uninited))) {
        ex->uninited = INVALID_ADDRESS;
        string idesc = get_string(value, sym(idesc));
        if (idesc && !tfs_map_load(fs, ex, idesc)) {
            msg_err("TFS: extent at file block %ld has an invalid page map; its I/O will fail",
                    file_offset);
            ex->corrupt = true;
        }
    }
    assert(rangemap_insert(f->extentmap, &ex->node));
}

closure_function(1, 2, boolean, tfs_ingest_extent,
                 tfsfile, f,
                 value s, value v)
{
    assert(is_symbol(s));
    ingest_extent(bound(f), s, v);
    return true;
}

static boolean enumerate_dir_entries(tfs fs, tuple t);

closure_function(1, 2, boolean, enumerate_dir_entries_each,
                 tfs, fs,
                 value s, value v)
{
    tfs fs = bound(fs);
    if (is_tuple(v))
        return enumerate_dir_entries(fs, v);
    return true;
}

static boolean enumerate_dir_entries(tfs fs, tuple t)
{
    tuple extents = get_tuple(t, sym(extents));
    if (extents) {
        if (table_find(fs->files, t))
            return true;
        tfsfile f = allocate_fsfile(fs, t);
        if (f == INVALID_ADDRESS)
            return false;
        table_set(fs->files, t, f);
        value filelength = get(t, sym(filelength));
        u64 len;
        if (filelength && u64_from_value(filelength, &len))
            fsfile_set_length(&f->f, len);
        return iterate(extents, stack_closure(tfs_ingest_extent, f));
    }
    table_set(fs->files, t, INVALID_ADDRESS);
    tuple c = children(t);
    if (c)
        return iterate(c, stack_closure(enumerate_dir_entries_each, fs));
    return true;
}

void filesystem_storage_op(tfs fs, sg_list sg, range blocks, boolean write,
                           status_handler completion)
{
    tfs_debug("%s: fs %p, sg %p, sg size %ld, blocks %R, %c\n", func_ss,
              fs, sg, sg->count, blocks, write ? 'w' : 'r');
    struct storage_req req = {
        .op = write ? STORAGE_OP_WRITESG : STORAGE_OP_READSG,
        .blocks = blocks,
        .data = sg,
        .completion = completion,
    };
    apply(fs->req_handler, &req);
}

#ifndef TFS_READ_ONLY
closure_function(2, 1, void, zero_blocks_complete,
                 sg_list, sg, status_handler, completion,
                 status s)
{
    sg_list sg = bound(sg);
    sg_list_release(sg);
    deallocate_sg_list(sg);
    apply(bound(completion), s);
    closure_finish();
}

/* Zeros are written in requests of at most ZERO_CHUNK_PAGES pages, one at a
 * time: the sg list of a single request for a whole extent (up to
 * MAX_EXTENT_SIZE) needs a large contiguous allocation, and issuing all the
 * chunks at once moves the same demand into the storage driver; either fails
 * under memory pressure. */
#define ZERO_CHUNK_PAGES 256

static void zero_blocks_chunk(tfs fs, range blocks, status_handler completion)
{
    int blocks_per_page = U64_FROM_BIT(fs->page_order - fs->fs.blocksize_order);
    sg_list sg = allocate_sg_list();
    if (sg == INVALID_ADDRESS) {
        apply(completion, timm("result", "failed to allocate sg list"));
        return;
    }
    status_handler zero_blocks_completion = closure(fs->fs.h, zero_blocks_complete, sg, completion);
    if (zero_blocks_completion == INVALID_ADDRESS) {
        apply(completion, timm("result", "failed to allocate completion"));
        deallocate_sg_list(sg);
        return;
    }
    range r = blocks;
    while (range_span(r) > 0) {
        u64 length = MIN(range_span(r), blocks_per_page);
        u64 bytes = length << fs->fs.blocksize_order;
        sg_buf sgb = sg_list_tail_add(sg, bytes);
        if (sgb == INVALID_ADDRESS) {
            apply(zero_blocks_completion, timm("result", "failed to allocate sg buf"));
            return;
        }
        sgb->buf = fs->zero_page;
        sgb->offset = 0;
        sgb->size = bytes;
        sgb->refcount = 0;
        r.start += length;
    }
    struct storage_req req = {
        .op = STORAGE_OP_WRITESG,
        .blocks = blocks,
        .data = sg,
        .completion = zero_blocks_completion,
    };
    apply(fs->req_handler, &req);
}

static u64 zero_chunk_blocks(tfs fs)
{
    return (u64)ZERO_CHUNK_PAGES << (fs->page_order - fs->fs.blocksize_order);
}

/* Writes the chunk after the one just completed; the first failure ends it. */
closure_function(3, 1, void, zero_blocks_next,
                 tfs, fs, range, rest, status_handler, completion,
                 status s)
{
    tfs fs = bound(fs);
    range rest = bound(rest);
    if (!is_ok(s) || range_span(rest) == 0) {
        apply(bound(completion), s);
        closure_finish();
        return;
    }
    u64 n = MIN(range_span(rest), zero_chunk_blocks(fs));
    bound(rest).start += n;
    zero_blocks_chunk(fs, irangel(rest.start, n), (status_handler)closure_self());
}

static void zero_blocks(tfs fs, range blocks, status_handler completion)
{
    tfs_debug("%s: fs %p, blocks %R\n", func_ss, fs, blocks);
    if (range_span(blocks) <= zero_chunk_blocks(fs)) {
        zero_blocks_chunk(fs, blocks, completion);
        return;
    }
    status_handler next = closure(fs->fs.h, zero_blocks_next, fs, blocks, completion);
    if (next == INVALID_ADDRESS) {
        apply(completion, timm("result", "failed to allocate completion"));
        return;
    }
    apply(next, STATUS_OK);
}
#endif

closure_function(4, 1, boolean, read_extent,
                 tfs, fs, sg_list, sg, merge, m, range, blocks,
                 rmnode node)
{
    tfs fs = bound(fs);
    sg_list sg = bound(sg);
    extent e = (extent)node;
    range i = range_intersection(bound(blocks), node->r);
    u64 e_offset = i.start - node->r.start;
    u64 len = range_span(i);
    range blocks = irangel(e->start_block + e_offset, len);
    tfs_debug("%s: e %p, uninited %p, sg %p m %p blocks %R, i %R, len %ld, blocks %R\n",
              func_ss, e, e->uninited, bound(sg), bound(m), bound(blocks), i, len, blocks);
    uninited u = e->uninited;
    if (e->corrupt) {
        apply(apply_merge(bound(m)), timm("result", "extent metadata invalid", "fsstatus", "%d", -EIO));
    } else if (u == INVALID_ADDRESS && e->map) {
        /* Written pages from storage, the others as zeros, in order. */
        u64 bpp = tfs_blocks_per_page(fs);
        u64 b = e_offset, end = e_offset + len;
        while (b < end) {
            boolean set = map_bit(e->map, b / bpp);
            u64 run = b;
            while (run < end && map_bit(e->map, run / bpp) == set)
                run = MIN(end, (run / bpp + 1) * bpp);
            if (set)
                filesystem_storage_op(fs, sg, irange(e->start_block + b, e->start_block + run),
                                      false, apply_merge(bound(m)));
            else
                sg_zero_fill(sg, (run - b) << fs->fs.blocksize_order);
            b = run;
        }
    } else if (!u || ((u != INVALID_ADDRESS) && u->initialized)) {
        filesystem_storage_op(fs, sg, blocks, false, apply_merge(bound(m)));
    } else {
        sg_zero_fill(sg, range_span(blocks) << fs->fs.blocksize_order);
    }
    return true;
}

closure_function(3, 1, boolean, zero_hole,
                 tfs, fs, sg_list, sg, range, blocks,
                 range z)
{
    range i = range_intersection(bound(blocks), z);
    u64 length = range_span(i) << bound(fs)->fs.blocksize_order;
    tfs_debug("%s: i %R, length %ld\n", func_ss, i, length);
    sg_zero_fill(bound(sg), length);
    return true;
}

BSS_RO_AFTER_INIT io_status_handler ignore_io_status;

/* whole block reads, file length resolved in cache */
static void tfs_read(fsfile fsf,
                 sg_list sg, range q, status_handler complete)
{
    tfs fs = (tfs)fsf->fs;
    tfsfile f = (tfsfile)fsf;
    merge m = allocate_merge(fs->fs.h, complete);
    status_handler k = apply_merge(m);
    tfs_debug("%s: fsfile %p, sg %p, q %R, sh %F\n", func_ss, f, sg, q, complete);

    /* read extent data and zero gaps */
    q.end = MIN(q.end, fsf->length);
    range blocks = range_rshift_pad(q, fs->fs.blocksize_order);
    filesystem_lock(&fs->fs);
    rangemap_range_lookup_with_gaps(f->extentmap, blocks,
                                    stack_closure(read_extent, fs, sg, m, blocks),
                                    stack_closure(zero_hole, fs, sg, blocks));
    filesystem_unlock(&fs->fs);
    apply(k, STATUS_OK);
}

#ifndef TFS_READ_ONLY

static int tfs_pub_register(tfsfile f, extent ex, boolean init, u64 length);
static status_handler tfs_data_handler(tfs fs, status_handler sh);
#ifdef KERNEL
static void tfs_pub_detach(tfs fs, extent ex);
static void tfs_pub_cycle(tfs fs, status_handler sh);
static int tfs_pub_register_pages(tfsfile f, extent ex, u64 first, u64 last);
#endif
static tuple cleanup_directory(tuple dir);

int filesystem_write_tuple(tfs fs, tuple t)
{
    if (fs->fs.ro)
        return -EROFS;
    if (log_write(fs->tl, t) && (!fs->temp_log || log_write(fs->temp_log, t)))
        return 0;
    else
        return -ENOSPC;
}

int filesystem_write_eav(tfs fs, tuple t, symbol a, value v, boolean cleanup)
{
    if (fs->fs.ro)
        return -EROFS;
    tuple parent = cleanup ? cleanup_directory(v) : 0;
    boolean success = log_write_eav(fs->tl, t, a, v);
    if (success && fs->temp_log) {
        /* The above log_write_eav() call might have started a TFS log rebuild, in which case the
         * entire directory tree has been fixed up again and we have to do another cleanup. */
        if (cleanup)
            cleanup_directory(v);
        success = log_write_eav(fs->temp_log, t, a, v);
    }
    if (cleanup)
        fixup_directory(parent, v);
    if (success)
        return 0;
    else
        return -ENOSPC;
}

static int tfs_shrink(tfsfile f, u64 len);

static int tfs_truncate(filesystem fs, fsfile f, u64 len)
{
    boolean shrinking = len < f->length;
    int s = 0;
    if (shrinking) {
        s = tfs_shrink((tfsfile)f, len);
        if (s != 0)
            goto out;
    }
    if (f->md) {
        value v = value_from_u64(len);
        if (v == INVALID_ADDRESS) {
            s = -ENOMEM;
            goto out;
        }
        symbol l = sym(filelength);
        s = filesystem_write_eav((tfs)fs, f->md, l, v, false);
        if (s != 0)
            goto out;
        set(f->md, l, v);
        f->status |= FSF_DIRTY_DATASYNC;
    }
    if (shrinking)
        fsfile_set_length(f, len);
out:
#ifdef KERNEL
    if (shrinking)
        pagecache_node_end_truncate(f->cache_node);
#endif
    return s;
}

/* create a new extent in the filesystem

   The life an extent depends on a particular allocation of contiguous
   storage space. The extent is tied to this allocated area (nominally
   page size). Only the extent data length and allocation size may be
   updated; the file offset and block start are immutable. As an
   optimization, adjacent extents on the disk could be joined into
   larger extents with only a meta update.

*/

static int create_extent(tfs fs, range blocks, boolean uninited, extent *ex)
{
    assert(!fs->fs.ro);
    heap h = fs->fs.h;
    u64 nblocks = MAX(range_span(blocks), MIN_EXTENT_SIZE >> fs->fs.blocksize_order);
    nblocks = MIN(range_span(blocks), MAX_EXTENT_SIZE >> fs->fs.blocksize_order);

    tfs_debug("create_extent: blocks %R, uninited %p, nblocks %ld\n", blocks, uninited, nblocks);
    if (!filesystem_reserve_log_space(fs, &fs->next_extend_log_offset, 0, 0) ||
        !filesystem_reserve_log_space(fs, &fs->next_new_log_offset, 0, 0))
        return -ENOSPC;

    u64 start_block = filesystem_allocate_storage(fs, nblocks);
    while (start_block == u64_from_pointer(INVALID_ADDRESS)) {
        if (nblocks <= (MIN_EXTENT_ALLOC_SIZE >> fs->fs.blocksize_order))
            break;
        nblocks /= 2;
        start_block = filesystem_allocate_storage(fs, nblocks);
    }
    if (start_block == u64_from_pointer(INVALID_ADDRESS))
        return -ENOSPC;

    range storage_blocks = irangel(start_block, nblocks);
    tfs_debug("   storage_blocks %R\n", storage_blocks);
    if ((nblocks < range_span(blocks)))
        blocks.end = blocks.start + nblocks;
    *ex = allocate_extent(h, blocks, storage_blocks);
    if (*ex == INVALID_ADDRESS)
        return -ENOMEM;
    (*ex)->md = 0;
    if (uninited)
        (*ex)->uninited = INVALID_ADDRESS;
    return 0;
}

static void deallocate_extent(tfs fs, extent ex)
{
#ifdef KERNEL
    if (ex->pubs)
        tfs_pub_detach(fs, ex);
#endif
    tfs_map_free(fs, ex);
    if (ex->uninited && ex->uninited != INVALID_ADDRESS)
        refcount_release(&ex->uninited->refcount);
    deallocate(fs->fs.h, ex, sizeof(*ex));
}

static void destroy_extent(tfs fs, extent ex)
{
    filesystem_release_storage(fs, irangel(ex->start_block, ex->allocated));
    deallocate_extent(fs, ex);
}

static int add_extent_to_file(tfsfile f, extent ex)
{
    tuple md = f->f.md;
    if (md) {
        tfs fs = tfs_from_file(f);
        tuple extents;
        symbol a = sym(extents);
        if (!(extents = get_tuple(md, a))) {
            extents = allocate_tuple();
            int s = filesystem_write_eav(fs, md, a, extents, false);
            if (s != 0) {
                deallocate_value(extents);
                return s;
            }
            set(md, a, extents);
        }

        // XXX encode this as an immediate bitstring
        tuple e = allocate_tuple();
        ex->md = e;
        set(e, sym(offset), value_from_u64(ex->start_block));
        set(e, sym(length), value_from_u64(range_span(ex->node.r)));
        set(e, sym(allocated), value_from_u64(ex->allocated));
        if (ex->uninited == INVALID_ADDRESS)
            set(e, sym(uninited), null_value);
        symbol offs = intern_u64(ex->node.r.start);
        int s = filesystem_write_eav(fs, extents, offs, e, false);
        if (s != 0) {
            destruct_value(e, true);
            return s;
        }
        set(extents, offs, e);
        f->f.status |= FSF_DIRTY_DATASYNC;
    }
    tfs_debug("%s: f %p, reserve %R\n", func_ss, f, ex->node.r);
    if (!rangemap_insert(f->extentmap, &ex->node)) {
        rbtree_dump(&f->extentmap->t, RB_INORDER);
        assert(0);
    }
    return 0;
}

static int remove_extent_from_file(tfsfile f, extent ex)
{
    /* The tuple corresponding to this extent will be destroyed when the
     * filesystem log is compacted. */

    tuple md = f->f.md;
    if (md) {
        tuple extents = get(md, sym(extents));
        assert(extents);
        symbol offs = intern_u64(ex->node.r.start);
        int s = filesystem_write_eav(tfs_from_file(f), extents, offs, 0, false);
        if (s != 0)
            return s;
        set(extents, offs, 0);
    }
    rangemap_remove_node(f->extentmap, &ex->node);
    return 0;
}

define_closure_function(2, 1, void, uninited_complete,
                        uninited, u, status_handler, complete,
                        status s)
{
    uninited u = bound(u);
    if (!is_ok(s))
        s = timm_up(s, "result", "failed to convert uninited extent");
#ifdef KERNEL
    tfs fs = u->fs;
    u64 flags = spin_lock_irq(&fs->pub_lock);
    u->initialized = true;
    uninited_write w = u->deferred;
    u->deferred = 0;
    spin_unlock_irq(&fs->pub_lock, flags);
    while (w) {
        uninited_write next = w->next;
        if (!is_ok(s))
            apply(w->sh, timm("result", "failed to convert uninited extent"));
        else if (w->sg)
            filesystem_storage_op(fs, w->sg, w->r, true, w->sh);
        else
            zero_blocks(fs, w->r, w->sh);
        deallocate(fs->fs.h, w, sizeof(*w));
        w = next;
    }
    apply(bound(complete), s);
#else
    apply(bound(complete), s);
    u->initialized = true;
#endif
    refcount_release(&u->refcount);
}

define_closure_function(2, 0, void, free_uninited,
                        heap, h, uninited, u)
{
    tfs_debug("%s: %p\n", func_ss, bound(u));
    deallocate(bound(h), bound(u), sizeof(struct uninited));
}

static uninited allocate_uninited(tfs fs, status_handler sh)
{
    heap h = fs->fs.h;
    uninited u = allocate(h, sizeof(struct uninited));
    if (u == INVALID_ADDRESS)
        return u;
    u->fs = fs;
    init_refcount(&u->refcount, 2, init_closure(&u->free, free_uninited, h, u));
    u->initialized = false;
    u->deferred = 0;
    init_closure(&u->complete, uninited_complete, u, sh);
    return u;
}

/* Write data (or zeros without sg) to file blocks, counted for publication. */
static void tfs_data_write(tfs fs, sg_list sg, range r, merge m)
{
    status_handler sh = apply_merge(m);
    status_handler c = tfs_data_handler(fs, sh);
    if (c == INVALID_ADDRESS) {
        apply(sh, timm("result", "failed to allocate write completion"));
        return;
    }
    if (sg)
        filesystem_storage_op(fs, sg, r, true, c);
    else
        zero_blocks(fs, r, c);
}

#ifdef KERNEL
/* The zeros converting an uninited extent are written after its first write
 * was issued, and may cover blocks that later writes also reach: a later
 * write issued before the conversion completes could then be overwritten by
 * zeros, since the device need not apply overlapping requests in order
 * (Firecracker's Async engine does not). Such writes are counted for
 * publication now and issued once the conversion completes. False when the
 * conversion has already completed: write as usual. */
static boolean tfs_uninited_defer(tfs fs, uninited u, sg_list sg, range r, merge m)
{
    u64 flags = spin_lock_irq(&fs->pub_lock);
    boolean done = u->initialized;
    spin_unlock_irq(&fs->pub_lock, flags);
    if (done)
        return false;
    status_handler sh = apply_merge(m);
    status_handler c = tfs_data_handler(fs, sh);
    if (c == INVALID_ADDRESS) {
        apply(sh, timm("result", "failed to allocate write completion"));
        return true;
    }
    uninited_write w = allocate(fs->fs.h, sizeof(*w));
    if (w == INVALID_ADDRESS) {
        apply(c, timm("result", "failed to allocate deferred write"));
        return true;
    }
    w->next = 0;
    w->sg = sg;
    w->r = r;
    w->sh = c;
    flags = spin_lock_irq(&fs->pub_lock);
    if (!u->initialized) {
        uninited_write *tail = &u->deferred;
        while (*tail)
            tail = &(*tail)->next;
        *tail = w;
        w = 0;
    }
    spin_unlock_irq(&fs->pub_lock, flags);
    if (w) {
        /* Completed meanwhile. */
        if (sg)
            filesystem_storage_op(fs, sg, r, true, c);
        else
            zero_blocks(fs, r, c);
        deallocate(fs->fs.h, w, sizeof(*w));
    }
    return true;
}
#endif

static u64 write_extent(tfsfile f, extent ex, sg_list sg, range blocks, merge m)
{
    tfs fs = tfs_from_file(f);
    range i = range_intersection(blocks, ex->node.r);
    u64 data_offset = i.start - ex->node.r.start;
    range r = irangel(ex->start_block + data_offset, range_span(i));

    tfs_debug("   %s: ex %p, uninited %p, sg %p, m %p, blocks %R, write %R\n",
              func_ss, ex, ex->uninited, sg, m, blocks, r);

    if (ex->corrupt) {
        apply(apply_merge(m), timm("result", "extent metadata invalid", "fsstatus", "%d", -EIO));
        return i.end;
    }
#ifdef KERNEL
    /* v6: write only the pages concerned (see tfs_map_*). Only extents filled
     * from their start use a page map: on macOS, random first writes into
     * extents with page maps measured slower (randwrite -20 %) than
     * converting the whole extent, while sequential fills gain (seqwrite
     * +55 %). A random first write converts the extent as on v5. */
    if (ex->uninited == INVALID_ADDRESS && f->f.md && fs->version >= TFS_VERSION &&
        (ex->map || (i.start == ex->node.r.start && tfs_map_alloc(fs, ex, true)))) {
        u64 bpp = tfs_blocks_per_page(fs);
        u64 rs = i.start - ex->node.r.start, re = i.end - ex->node.r.start;
        u64 p0 = rs / bpp, p1 = (re - 1) / bpp;
        int fss = tfs_pub_register_pages(f, ex, p0, p1);
        if (fss != 0) {
            status s = timm("result", "failed to register page publication");
            apply(apply_merge(m), timm_append(s, "fsstatus", "%d", fss));
            return i.end;
        }
        /* Uncovered blocks of a new partial page must not keep old contents. */
        if ((rs % bpp) && !map_bit(ex->map, p0))
            tfs_data_write(fs, 0, irange(ex->start_block + p0 * bpp, ex->start_block + rs), m);
        u64 tail = MIN((p1 + 1) * bpp, range_span(ex->node.r));
        if (re < tail && !map_bit(ex->map, p1))
            tfs_data_write(fs, 0, irange(ex->start_block + re, ex->start_block + tail), m);
        tfs_data_write(fs, sg, r, m);
        for (u64 p = p0; p <= p1; p++)
            map_set(ex->map, p);
        return i.end;
    }
#endif
    if (ex->uninited == INVALID_ADDRESS) {
        /* Begin process of normalizing uninited extent. The log entry that
         * marks it initialized is published once the zeros and data written
         * here are durable (tfs_pub_register()). */
        int fss = tfs_pub_register(f, ex, true, 0);
        if (fss != 0) {
            status s = timm("result", "failed to write log");
            apply(apply_merge(m), timm_append(s, "fsstatus", "%d", fss));
            return i.end;
        }
        ex->uninited = allocate_uninited(fs, apply_merge(m));
        tfs_debug("%s: new uninited %p\n", func_ss, ex->uninited);
        if (ex->uninited == INVALID_ADDRESS)
            goto alloc_fail;
        m = allocate_merge(fs->fs.h, (status_handler)&ex->uninited->complete);
        if (m == INVALID_ADDRESS)
            goto alloc_fail;
        status_handler k = apply_merge(m);
        if (sg) {
            u64 data_end = i.end - ex->node.r.start;
            u64 extent_end = range_span(ex->node.r);
            if (data_offset > 0)
                tfs_data_write(fs, 0, range_add(irange(0, data_offset), ex->start_block), m);
            if (data_end < extent_end)
                tfs_data_write(fs, 0, range_add(irange(data_end, extent_end), ex->start_block), m);
            tfs_data_write(fs, sg, r, m);
        } else {
            tfs_data_write(fs, 0, r, m);
        }
        apply(k, STATUS_OK);
        return i.end;
    }
#ifdef KERNEL
    if (ex->uninited && tfs_uninited_defer(fs, ex->uninited, sg, r, m))
        return i.end;
#endif
    tfs_data_write(fs, sg, r, m);
    return i.end;
  alloc_fail:
    apply(apply_merge(m), timm("result", "unable to allocate memory for uninited write"));
    return i.end;
}

static int fill_gap(tfsfile f, sg_list sg, range blocks, merge m, u64 *edge)
{
    tfs_debug("   %s: writing new extent blocks %R\n", func_ss, blocks);
    extent ex;
    tfs fs = tfs_from_file(f);
    /* Logged uninited even when written now: write_extent() publishes it
     * initialized once its data is durable. */
    int fss = create_extent(fs, blocks, true, &ex);
    if (fss != 0)
        return fss;
    blocks = ex->node.r;
    fss = add_extent_to_file(f, ex);
    if (fss != 0) {
        destroy_extent(fs, ex);
        return fss;
    }
    if (m)
        write_extent(f, ex, sg, blocks, m);
    *edge = blocks.end;
    return 0;
}

static int update_extent(tfsfile f, extent ex, symbol l, u64 val)
{
    if (f->f.md) {
        assert(ex->md);
        value v = value_from_u64(val);
        int s = filesystem_write_eav(tfs_from_file(f), ex->md, l, v, false);
        if (s != 0)
            return s;
        value oldval = get(ex->md, l);
        assert(oldval);
        deallocate_value(oldval);
        set(ex->md, l, v);
        f->f.status |= FSF_DIRTY_DATASYNC;
    }
    return 0;
}

static int update_extent_allocated(tfsfile f, extent ex, u64 allocated)
{
    int s = update_extent(f, ex, sym(allocated), allocated);
    if (s != 0)
        return s;
    tfs_debug("   %s: was 0x%lx, now 0x%lx\n", func_ss, ex->allocated, allocated);
    ex->allocated = allocated;
    return 0;
}

static int update_extent_length(tfsfile f, extent ex, u64 new_length)
{
    int s = update_extent(f, ex, sym(length), new_length);
    if (s != 0)
        return s;

    /* TODO cheating; should be reinsert - update rangemap interface? */
    tfs_debug("   %s: was %R\n", func_ss, ex->node.r);
    ex->node.r = irangel(ex->node.r.start, new_length);
    tfs_debug("   %s: now %R\n", func_ss, ex->node.r);
    return 0;
}

/* Data publication.
 *
 * Log entries that make written blocks readable (an extent marked initialized,
 * an extent grown by a write) must not become durable before those blocks: a
 * crash would expose zeros or earlier contents, possibly another file's. Such
 * entries are therefore not staged when the write is issued. The extent's
 * in-memory state changes at once, its metadata tuple keeps the published
 * state, and a pending publication is registered for the current write
 * generation. Every data write is counted in its generation. A publication
 * cycle switches the generation, waits for the old generation's writes to
 * complete, flushes the device and only then stages the entries; the log
 * flush and the final device flush of fsync follow. A failed data write or
 * flush stops publication for the volume: later cycles fail with EIO. */
#ifdef KERNEL
struct tfs_pub {
    extent ex;          /* 0 once the extent is gone */
    tfsfile f;
    int gen;
    u64 length;         /* extent length to publish (0: none) */
    boolean init;       /* publish the removal of uninited */
    u8 *bits;           /* pages to publish in the extent's map, or 0 */
    u32 bits_bytes;
};

/* A cycle completes when a device flush that was issued after its
 * generation drained has succeeded: either its own flush (eager cycles, for
 * fsync and sync) or any other flush of this volume (lazy cycles, for
 * memory-reclaim syncs and the timer, which then add no flush of their own
 * unless none comes within TFS_PUBLISH_DELAY_SECONDS). */

closure_function(3, 1, void, tfs_data_write_done,
                 tfs, fs, int, gen, status_handler, sh,
                 status s)
{
    tfs fs = bound(fs);
    int gen = bound(gen);
    if (!is_ok(s))
        fs->pub_error = true;
    thunk launch = 0;
    u64 flags = spin_lock_irq(&fs->pub_lock);
    assert(fs->inflight[gen] > 0);
    if (--fs->inflight[gen] == 0 && fs->pub_active && fs->cycle_gen == gen) {
        fs->pub_drained = true;
        if (fs->pub_eager && !fs->pub_flush_issued) {
            fs->pub_flush_issued = true;
            launch = fs->pub_flush;
        }
    }
    spin_unlock_irq(&fs->pub_lock, flags);
    apply(bound(sh), s);
    if (launch)
        async_apply_bh(launch);
    closure_finish();
}

/* Filesystem lock held: the generation only changes under it. */
static status_handler tfs_data_handler(tfs fs, status_handler sh)
{
    int gen = fs->wgen;
    status_handler c = closure(fs->fs.h, tfs_data_write_done, fs, gen, sh);
    if (c == INVALID_ADDRESS)
        return c;
    u64 flags = spin_lock_irq(&fs->pub_lock);
    fs->inflight[gen]++;
    spin_unlock_irq(&fs->pub_lock, flags);
    return c;
}

static void tfs_pub_finish(tfs fs);

/* Runs from the runqueue: it takes the filesystem mutex, which must never
 * block the queue that delivers storage and syscall completions. */
closure_function(3, 0, void, tfs_flush_finish,
                 tfs, fs, u64, cycle, boolean, ok)
{
    tfs fs = bound(fs);
    filesystem_lock(&fs->fs);
    if (!bound(ok))
        fs->pub_error = true;
    if (fs->pub_active && fs->pub_cycle == bound(cycle))
        tfs_pub_finish(fs);
    filesystem_unlock(&fs->fs);
    filesystem_release(&fs->fs);
    closure_finish();
}

/* The caller's completion (possibly a syscall's contextual closure) is handed
 * on asynchronously, as the storage layer does. */
closure_function(2, 1, void, tfs_flush_done,
                 tfs, fs, u64, cycle,
                 status s)
{
    tfs fs = bound(fs);
    thunk t = closure(fs->fs.h, tfs_flush_finish, fs, bound(cycle), is_ok(s));
    if (t != INVALID_ADDRESS)
        async_apply(t);
    else
        filesystem_release(&fs->fs);    /* the timer finishes the cycle */
    closure_finish();
}

closure_function(2, 1, void, tfs_flush_relay,
                 status_handler, observer, status_handler, sh,
                 status s)
{
    apply(bound(observer), is_ok(s) ? STATUS_OK : timm("result", "device flush failed"));
    async_apply_status_handler(bound(sh), s);
    closure_finish();
}

/* Every device flush of the volume goes through here, so that it can
 * complete the publication cycle whose writes it covers. */
static void tfs_device_flush(tfs fs, status_handler sh)
{
    u64 cycle = 0;
    u64 flags = spin_lock_irq(&fs->pub_lock);
    if (fs->pub_active && fs->pub_drained)
        cycle = fs->pub_cycle;
    spin_unlock_irq(&fs->pub_lock, flags);
    status_handler c = sh;
    if (cycle) {
        /* On allocation failure this flush just does not complete the cycle. */
        status_handler observer = closure(fs->fs.h, tfs_flush_done, fs, cycle);
        if (observer != INVALID_ADDRESS) {
            c = closure(fs->fs.h, tfs_flush_relay, observer, sh);
            if (c == INVALID_ADDRESS) {
                deallocate_closure(observer);
                c = sh;
            } else {
                filesystem_reserve(&fs->fs);    /* released by tfs_flush_finish */
            }
        }
    }
    struct storage_req req = {
        .op = STORAGE_OP_FLUSH,
        .blocks = irange(0, 0),
        .completion = c,
    };
    apply(fs->req_handler, &req);
}

closure_function(1, 1, void, tfs_pub_flush_status,
                 tfs, fs,
                 status s)
{
    if (!is_ok(s)) {
        msg_err("TFS: data publication flush failed: %v", s);
        timm_dealloc(s);
    }
    filesystem_release(&bound(fs)->fs);
    closure_finish();
}

closure_function(2, 0, void, tfs_pub_flush,
                 tfs, fs, status_handler, sh)
{
    tfs_device_flush(bound(fs), bound(sh));
    closure_finish();
}

static void tfs_pub_make_eager(tfs fs);
static void tfs_pub_lazy(tfs fs);

closure_function(1, 2, void, tfs_pub_timer_expired,
                 tfs, fs,
                 u64 expiry, u64 overruns)
{
    tfs fs = bound(fs);
    if (overruns != timer_disabled) {
        filesystem_lock(&fs->fs);
        fs->pub_timer_armed = false;
        if (fs->pub_active)
            tfs_pub_make_eager(fs);     /* no flush came along */
        else
            tfs_pub_lazy(fs);
        filesystem_unlock(&fs->fs);
    }
    closure_finish();
}

static void tfs_pub_arm_timer(tfs fs)
{
    if (fs->pub_timer_armed)
        return;
    timer_handler th = closure(fs->fs.h, tfs_pub_timer_expired, fs);
    if (th == INVALID_ADDRESS)
        return;     /* published by the next sync */
    fs->pub_timer_armed = true;
    register_timer(kernel_timers, &fs->pub_timer, CLOCK_ID_MONOTONIC_RAW,
                   seconds(TFS_PUBLISH_DELAY_SECONDS), false, 0, th);
}

/* The current generation's publication entry for an extent. */
static struct tfs_pub *tfs_pub_entry(tfsfile f, extent ex)
{
    tfs fs = tfs_from_file(f);
    struct tfs_pub *p;
    for (int i = vector_length(fs->pubs) - 1; i >= 0; i--) {
        p = vector_get(fs->pubs, i);
        if (p->gen != fs->wgen)
            break;
        if (p->ex == ex)
            return p;
    }
    p = allocate(fs->fs.h, sizeof(*p));
    if (p == INVALID_ADDRESS)
        return p;
    p->ex = ex;
    p->f = f;
    p->gen = fs->wgen;
    p->length = 0;
    p->init = false;
    p->bits = 0;
    p->bits_bytes = 0;
    vector_push(fs->pubs, p);
    ex->pubs++;
    f->f.status |= FSF_DIRTY_DATASYNC;
    tfs_pub_arm_timer(fs);
    return p;
}

/* Filesystem lock held; called before the writes it covers are issued. */
static int tfs_pub_register(tfsfile f, extent ex, boolean init, u64 length)
{
    if (!f->f.md)
        return 0;
    struct tfs_pub *p = tfs_pub_entry(f, ex);
    if (p == INVALID_ADDRESS)
        return -ENOMEM;
    p->init |= init;
    if (length > p->length)
        p->length = length;
    return 0;
}

/* Pages first..last of ex's map are being written in this generation. */
static int tfs_pub_register_pages(tfsfile f, extent ex, u64 first, u64 last)
{
    tfs fs = tfs_from_file(f);
    struct tfs_pub *p = tfs_pub_entry(f, ex);
    if (p == INVALID_ADDRESS)
        return -ENOMEM;
    if (!p->bits) {
        p->bits = allocate_zero(fs->fs.h, (ex->map_pages + 7) / 8);
        if (p->bits == INVALID_ADDRESS) {
            p->bits = 0;
            return -ENOMEM;
        }
        p->bits_bytes = (ex->map_pages + 7) / 8;
    }
    for (u64 i = first; i <= last; i++)
        map_set(p->bits, i);
    return 0;
}

static void tfs_pub_free(tfs fs, struct tfs_pub *p)
{
    if (p->bits)
        deallocate(fs->fs.h, p->bits, p->bits_bytes);
    deallocate(fs->fs.h, p, sizeof(*p));
}

static void map_clear_from(u8 *map, u32 from, u32 pages)
{
    for (u32 i = from; i < pages; i++)
        map[i >> 3] &= ~(1 << (i & 7));
}

/* After a shrink: pages wholly beyond the extent lose their bits, here and in
 * pending publications. A partial last page keeps its bit; the extent never
 * grows again, so its blocks beyond the length stay unreachable. */
static void tfs_map_truncate(tfs fs, extent ex)
{
    if (!ex->map)
        return;
    u32 used = tfs_map_used_pages(fs, ex);
    map_clear_from(ex->map, used, ex->map_pages);
    map_clear_from(ex->map_pub, used, ex->map_pages);
    struct tfs_pub *p;
    vector_foreach(fs->pubs, p)
        if (p->ex == ex && p->bits)
            map_clear_from(p->bits, used, p->bits_bytes * 8);
}

static void tfs_pub_detach(tfs fs, extent ex)
{
    struct tfs_pub *p;
    vector_foreach(fs->pubs, p) {
        if (p->ex == ex) {
            p->ex = 0;
            ex->pubs--;
        }
    }
}

/* Publish the pages of p in the extent's map; once every page of the extent
 * is published, the extent becomes an ordinary initialized one. */
static int tfs_pub_stage_map(struct tfs_pub *p)
{
    tfsfile f = p->f;
    extent ex = p->ex;
    tfs fs = tfs_from_file(f);
    u32 bytes = MIN(p->bits_bytes, (ex->map_pages + 7) / 8);
    for (u32 i = 0; i < bytes; i++)
        ex->map_pub[i] |= p->bits[i];
    u32 used = tfs_map_used_pages(fs, ex);
    boolean all = true;
    for (u32 i = 0; i < used && all; i++)
        all = map_bit(ex->map_pub, i);
    symbol d = sym(idesc);
    value old = get(ex->md, d);
    if (all) {
        int s = filesystem_write_eav(fs, ex->md, sym(uninited), 0, false);
        if (s == 0 && old)
            s = filesystem_write_eav(fs, ex->md, d, 0, false);
        if (s != 0)
            return s;
        set(ex->md, sym(uninited), 0);
        if (old) {
            set(ex->md, d, 0);
            deallocate_value(old);
        }
        tfs_map_free(fs, ex);
        ex->uninited = 0;
    } else {
        string s = tfs_map_encode(fs, ex);
        if (s == INVALID_ADDRESS)
            return -ENOMEM;
        int r = filesystem_write_eav(fs, ex->md, d, s, false);
        if (r != 0) {
            deallocate_string(s);
            return r;
        }
        set(ex->md, d, s);
        if (old)
            deallocate_value(old);
    }
    f->f.status |= FSF_DIRTY_DATASYNC;
    return 0;
}

static int tfs_pub_stage(struct tfs_pub *p)
{
    tfsfile f = p->f;
    extent ex = p->ex;
    /* A truncate since registration logged the shorter length itself. */
    u64 length = MIN(p->length, range_span(ex->node.r));
    u64 logged;
    if (length && get_u64(ex->md, sym(length), &logged) && length > logged) {
        int s = update_extent(f, ex, sym(length), length);
        if (s != 0)
            return s;
    }
    /* Length first: a torn log may then show a longer uninited extent. */
    symbol a = sym(uninited);
    if (p->init && get(ex->md, a)) {
        int s = filesystem_write_eav(tfs_from_file(f), ex->md, a, 0, false);
        if (s != 0)
            return s;
        set(ex->md, a, 0);
        f->f.status |= FSF_DIRTY_DATASYNC;
    }
    if (p->bits && ex->map)
        return tfs_pub_stage_map(p);
    return 0;
}

/* fss: 0, or the negative errno the waiters fail with. */
static void tfs_pub_wake(vector waiters, int fss)
{
    status_handler sh;
    vector_foreach(waiters, sh)
        async_apply_status_handler(sh, fss == 0 ? STATUS_OK :
            timm("result", "data publication failed", "fsstatus", "%d", fss));
    vector_clear(waiters);
}

static void tfs_pub_drop(tfs fs)
{
    struct tfs_pub *p;
    vector_foreach(fs->pubs, p) {
        if (p->ex)
            p->ex->pubs--;
        tfs_pub_free(fs, p);
    }
    vector_clear(fs->pubs);
}

/* Filesystem lock held, pubs not empty, no cycle in flight. */
static void tfs_pub_start(tfs fs, boolean eager)
{
    status_handler sh = closure(fs->fs.h, tfs_pub_flush_status, fs);
    thunk t = sh == INVALID_ADDRESS ? INVALID_ADDRESS :
              (thunk)closure(fs->fs.h, tfs_pub_flush, fs, sh);
    if (t == INVALID_ADDRESS) {
        if (sh != INVALID_ADDRESS)
            deallocate_closure(sh);
        tfs_pub_wake(fs->pub_waiters, -EIO);
        return;
    }
    filesystem_reserve(&fs->fs);    /* the cycle */
    filesystem_reserve(&fs->fs);    /* its own flush: tfs_pub_flush_status or finish */
    u64 flags = spin_lock_irq(&fs->pub_lock);
    fs->pub_active = true;
    fs->pub_cycle++;
    fs->cycle_gen = fs->wgen;
    fs->wgen ^= 1;
    fs->pub_drained = fs->inflight[fs->cycle_gen] == 0;
    fs->pub_eager = eager;
    fs->pub_flush = t;
    fs->pub_flush_sh = sh;
    boolean launch = eager && fs->pub_drained;
    fs->pub_flush_issued = launch;
    spin_unlock_irq(&fs->pub_lock, flags);
    if (launch) {
        async_apply(t);
    } else if (!eager) {
        tfs_pub_arm_timer(fs);          /* fallback if no flush comes */
    }
}

/* Make the cycle in flight issue its own flush. Filesystem lock held. */
static void tfs_pub_make_eager(tfs fs)
{
    thunk launch = 0;
    u64 flags = spin_lock_irq(&fs->pub_lock);
    fs->pub_eager = true;
    if (fs->pub_drained && !fs->pub_flush_issued) {
        fs->pub_flush_issued = true;
        launch = fs->pub_flush;
    }
    spin_unlock_irq(&fs->pub_lock, flags);
    if (launch)
        async_apply(launch);
}

/* A covering flush succeeded (or failed): stage the cycle's publications.
 * A log without room to grow (ENOSPC) is not a storage failure: the
 * publications not staged yet stay registered for a later cycle, which
 * retries them once space is freed (staging is idempotent), and this cycle's
 * waiters fail with ENOSPC. Any other failure stops publication for the
 * volume. Filesystem lock held. */
static void tfs_pub_finish(tfs fs)
{
    int fss = fs->pub_error ? -EIO : 0;
    int kept = 0, n = vector_length(fs->pubs);
    for (int i = 0; i < n; i++) {
        struct tfs_pub *p = vector_get(fs->pubs, i);
        if (p->gen != fs->cycle_gen) {
            vector_set(fs->pubs, kept++, p);
            continue;
        }
        if (p->ex) {
            if (fss == 0) {
                int r = tfs_pub_stage(p);
                if (r == -ENOSPC) {
                    msg_err("TFS: no log space to publish written data; retrying later");
                    fss = r;
                } else if (r != 0) {
                    msg_err("TFS: failed to stage data publication (%d)", r);
                    fs->pub_error = true;
                    fss = -EIO;
                }
            }
            if (fss == -ENOSPC) {
                p->gen = fs->wgen;      /* the next cycle's generation */
                vector_set(fs->pubs, kept++, p);
                continue;
            }
            p->ex->pubs--;
        }
        tfs_pub_free(fs, p);
    }
    vector_delete_range(fs->pubs, kept, n);
    u64 flags = spin_lock_irq(&fs->pub_lock);
    fs->pub_active = false;
    boolean issued = fs->pub_flush_issued;
    spin_unlock_irq(&fs->pub_lock, flags);
    if (!issued) {
        deallocate_closure(fs->pub_flush);
        deallocate_closure(fs->pub_flush_sh);
        filesystem_release(&fs->fs);
    }
    fs->pub_flush = 0;
    fs->pub_flush_sh = 0;
    tfs_pub_wake(fs->pub_waiters, fss);
    if (vector_length(fs->pub_next_waiters))
        tfs_pub_cycle(fs, 0);
    else if (vector_length(fs->pubs))
        tfs_pub_arm_timer(fs);
    filesystem_release(&fs->fs);
}

/* Filesystem lock held. Completes sh, if given, once every publication
 * registered before this call is staged in the log (or with EIO); issues the
 * flush this needs. */
static void tfs_pub_cycle(tfs fs, status_handler sh)
{
    if (fs->pub_active) {
        if (sh) {
            vector_push(fs->pub_next_waiters, sh);
            tfs_pub_make_eager(fs);
        }
        return;
    }
    if (sh)
        vector_push(fs->pub_waiters, sh);
    status_handler w;
    vector_foreach(fs->pub_next_waiters, w)
        vector_push(fs->pub_waiters, w);
    vector_clear(fs->pub_next_waiters);
    if (fs->pub_error) {
        tfs_pub_drop(fs);
        tfs_pub_wake(fs->pub_waiters, -EIO);
        return;
    }
    if (!vector_length(fs->pubs)) {
        tfs_pub_wake(fs->pub_waiters, 0);
        return;
    }
    tfs_pub_start(fs, true);
}

/* Start a cycle that completes with the next covering flush. */
static void tfs_pub_lazy(tfs fs)
{
    if (!fs->pub_active && !fs->pub_error && vector_length(fs->pubs))
        tfs_pub_start(fs, false);
}

static int tfs_extent_grow(tfsfile f, extent ex, u64 length)
{
    if (f->f.md) {
        int s = tfs_pub_register(f, ex, false, length);
        if (s != 0)
            return s;
        ex->node.r = irangel(ex->node.r.start, length);
        return 0;
    }
    return update_extent_length(f, ex, length);
}
#else
/* mkfs: no crash model; publish at once. */
static status_handler tfs_data_handler(tfs fs, status_handler sh)
{
    return sh;
}

static void tfs_map_truncate(tfs fs, extent ex)
{
    /* mkfs never creates page maps */
}

static int tfs_pub_register(tfsfile f, extent ex, boolean init, u64 length)
{
    if (!f->f.md || !init)
        return 0;
    assert(ex->md);
    symbol a = sym(uninited);
    int fss = filesystem_write_eav(tfs_from_file(f), ex->md, a, 0, false);
    if (fss != 0)
        return fss;
    set(ex->md, a, 0);
    f->f.status |= FSF_DIRTY_DATASYNC;
    return 0;
}

static int tfs_extent_grow(tfsfile f, extent ex, u64 length)
{
    return update_extent_length(f, ex, length);
}
#endif

/* Shrinking needs no barrier, but must not publish a pending growth. */
static int tfs_extent_shrink(tfsfile f, extent ex, u64 keep)
{
    u64 logged;
    if (f->f.md && ex->md && get_u64(ex->md, sym(length), &logged) && keep >= logged) {
        ex->node.r = irangel(ex->node.r.start, keep);
        return 0;
    }
    return update_extent_length(f, ex, keep);
}

/* Called with the filesystem mutex held. Drop it before draining the cache:
 * writeback needs that mutex, and never waits for the cache writer gate. */
static int tfs_shrink(tfsfile f, u64 len)
{
    tfs fs = tfs_from_file(f);
    int result = 0;
    buffer releases = 0;
#ifdef KERNEL
    pagecache_node pn = f->f.cache_node;
    filesystem_unlock(&fs->fs);
    pagecache_node_begin_truncate(pn);
    filesystem_lock(&fs->fs);
    /* Another truncation may have completed while this caller waited for the
     * writer gate. Recheck before constructing a tail range. */
    u64 old_length = f->f.length;
    if (len >= old_length)
        return 0;
    filesystem_unlock(&fs->fs);
    status s = pagecache_node_sync_locked(pn);
    if (is_ok(s) && (len & MASK(fs->fs.blocksize_order))) {
        /* Only a retained, initialized partial block needs disk zeroing. A
         * hole or an uninitialized extent is already zero: allocating a block
         * for it would make shrinking a full sparse filesystem fail ENOSPC. */
        filesystem_lock(&fs->fs);
        extent ex = (extent)rangemap_lookup(f->extentmap, len >> fs->fs.blocksize_order);
        boolean clear_tail = ex != INVALID_ADDRESS && (!ex->uninited ||
            (ex->uninited != INVALID_ADDRESS && ex->uninited->initialized) ||
            (ex->map && map_bit(ex->map, ((len >> fs->fs.blocksize_order) - ex->node.r.start) /
                                         tfs_blocks_per_page(fs))));
        filesystem_unlock(&fs->fs);
        if (clear_tail) {
            s = pagecache_node_zero_locked(pn, irange(len,
                MIN(old_length, pad(len, fs_blocksize(&fs->fs)))));
            if (is_ok(s))
                s = pagecache_node_sync_locked(pn);
        }
    }
    filesystem_lock(&fs->fs);
    if (!is_ok(s)) {
        s64 fsstatus;
        result = get_s64(s, sym(fsstatus), &fsstatus) ? fsstatus : -EIO;
        timm_dealloc(s);
        goto out;
    }
#endif
    releases = allocate_buffer(fs->fs.h, rangemap_count(f->extentmap) * sizeof(range));
    if (releases == INVALID_ADDRESS) {
        result = -ENOMEM;
        goto out;
    }
    u64 end = pad(len, fs_blocksize(&fs->fs)) >> fs->fs.blocksize_order;
    u64 removed_end = end;
    rmnode n = rangemap_first_node(f->extentmap);
    while (n != INVALID_ADDRESS) {
        extent ex = (extent)n;
        n = rangemap_next_node(f->extentmap, n);
        range removed = range_intersection(irange(end, infinity), ex->node.r);
        if (ex->node.r.start >= end) {
            result = remove_extent_from_file(f, ex);
            if (result != 0)
                break;
            range release = irangel(ex->start_block, ex->allocated);
            assert(buffer_write(releases, &release, sizeof(release)));
            deallocate_extent(fs, ex);
        } else if (ex->node.r.end > end) {
            u64 keep = end - ex->node.r.start;
            result = tfs_extent_shrink(f, ex, keep);
            if (result != 0)
                break;
            tfs_map_truncate(fs, ex);
            removed_end = removed.end;
            /* Log both fields before freeing storage. If the second update
             * fails, retain the old allocation; it cannot alias another file. */
            range release = irange(ex->start_block + keep, ex->start_block + ex->allocated);
            result = update_extent_allocated(f, ex, keep);
            if (result != 0)
                break;
            if (!range_empty(release))
                assert(buffer_write(releases, &release, sizeof(release)));
        }
        if (!range_empty(removed))
            removed_end = removed.end;
    }
#ifdef KERNEL
    filesystem_unlock(&fs->fs);
    pagecache_node_zero_cached_range(pn, range_lshift(irange(end, removed_end), fs->fs.blocksize_order));
    filesystem_lock(&fs->fs);
#else
    (void)removed_end;
#endif
    /* Old reads may still have used these blocks. The cache barrier above
     * finishes them before another file can reuse the physical storage, and
     * the release waits until the shrink is durable. */
    while (buffer_length(releases)) {
        range release;
        assert(buffer_read(releases, &release, sizeof(release)));
        filesystem_release_storage(fs, release);
    }
out:
    if (releases && releases != INVALID_ADDRESS)
        deallocate_buffer(releases);
    /* tfs_truncate publishes length before releasing the writer gate. */
    return result;
}

static int extend(tfsfile f, extent ex, sg_list sg, range blocks, merge m, u64 *edge)
{
    tfs fs = tfs_from_file(f);
    /* A reservation (no merge) only records metadata: no data or zeros are
     * written now, and the later writeback finds the range already mapped and
     * writes just its own data. Extending an initialized extent here would map
     * the gap and the reserved range onto blocks that still hold earlier data,
     * possibly from another file. An uninitialized extent reads as zeros and
     * is fully zeroed on conversion, so only that kind may grow here, and only
     * contiguously: growing it across a gap would make its conversion zero-fill
     * that whole gap. Otherwise the caller maps the range with a new
     * uninitialized extent, leaving any gap as a hole. */
    if ((!m && (ex->uninited != INVALID_ADDRESS || blocks.start != ex->node.r.end)) || ex->map) {
        /* An extent with a page map never grows (see tfs_map_*). */
        *edge = blocks.start;
        return 0;
    }
    blocks.end = MIN(blocks.end, ex->node.r.start + (MAX_EXTENT_SIZE >> fs->fs.blocksize_order));
    u64 free = ex->allocated - range_span(ex->node.r);
    range r = irangel(ex->node.r.end, free);
    if (blocks.end > r.end) {
        range new = irangel(ex->start_block + ex->allocated, blocks.end - r.end);
        u64 limit = fs->fs.size >> fs->fs.blocksize_order;
        if (new.end > limit) {
            blocks.end -= new.end - limit;
            new.end = limit;
        }
        if (range_span(new) && filesystem_reserve_storage(fs, new)) {
            int s = update_extent_allocated(f, ex, ex->allocated + range_span(new));
            if (s == 0) {
                r.end = blocks.end;
                free = r.end - ex->node.r.end;
            } else {
                filesystem_free_storage(fs, new);
            }
        }
    }
    range i = range_intersection(r, blocks);
    tfs_debug("   %s: node %R, free 0x%lx (%R), i %R\n", func_ss, ex->node.r, free, r, i);
    if (range_span(i) == 0) {
        *edge = blocks.start;
        return 0;
    }
    assert(blocks.start >= ex->node.r.end); // XXX temp
    assert(ex->node.r.end <= i.start); // XXX temp
    range z = irange(ex->node.r.end, i.start);
    /* A write publishes the longer extent once its data is durable; a
     * reservation only grows uninited extents, which read as zeros. */
    int s = m ? tfs_extent_grow(f, ex, i.end - ex->node.r.start) :
                update_extent_length(f, ex, i.end - ex->node.r.start);
    if (s == 0) {
        if (m) {
            if (range_span(z) > 0) {
                tfs_debug("      zero %R\n", z);
                write_extent(f, ex, 0, z, m);
            }
            tfs_debug("      write %R\n", i);
            write_extent(f, ex, sg, i, m);
        }
        *edge = i.end;
    }
    return s;
}

static status extents_range_handler(tfs fs, tfsfile f, range q, sg_list sg, merge m)
{
    assert(range_span(q) > 0);
    range blocks = range_rshift_pad(q, fs->fs.blocksize_order);
    tfs_debug("%s: file %p blocks %R sg %p m %p\n", func_ss, f, blocks, sg, m);
    assert(!sg || sg->count >= range_span(blocks) << fs->fs.blocksize_order);

    rmnode prev;            /* prior to edge, but could be extended */
    rmnode next;            /* intersecting or succeeding */
    prev = rangemap_lookup_max_lte(f->extentmap, blocks.start);
    if (prev == INVALID_ADDRESS) {
        /* gap */
        next = rangemap_first_node(f->extentmap);
    } else if (prev->r.end > blocks.start) {
        /* intersection */
        next = prev;
        prev = INVALID_ADDRESS;
    } else {
        next = rangemap_next_node(f->extentmap, prev);
    }

    do {
        tfs_debug("   prev %p, next %p\n", prev, next);
        u64 limit = next == INVALID_ADDRESS ? blocks.end : MIN(blocks.end, next->r.start);
        int fss;
        if (!m || sg) {
            if (blocks.start < limit) {
                /* try to extend previous node */
                if (prev != INVALID_ADDRESS && prev->r.end < limit) {
                    tfs_debug("   extent start 0x%lx, limit 0x%lx\n", blocks.start, limit);
                    fss = extend(f, (extent)prev, sg, irange(blocks.start, limit), m, &blocks.start);
                    if (fss != 0) {
                        status s = timm("result", "unable to extend extent");
                        return timm_append(s, "fsstatus", "%d", fss);
                    }
                }

                /* fill space */
                while (blocks.start < limit) {
                    tfs_debug("   fill start 0x%lx, limit 0x%lx\n", blocks.start, limit);
                    fss = fill_gap(f, sg, irange(blocks.start, limit), m, &blocks.start);
                    if (fss != 0) {
                        status s = timm("result", "unable to create extent");
                        return timm_append(s, "fsstatus", "%d", fss);
                    }
                }
            }
        } else {
            /* zero: skip to start of next node */
            blocks.start = limit;
        }

        prev = next;
        if (next != INVALID_ADDRESS) {
            extent ex = (extent)next;
            next = rangemap_next_node(f->extentmap, next);

            if (m && !sg && range_contains(blocks, ex->node.r)) {
                blocks.start = ex->node.r.end;
                fss = remove_extent_from_file(f, ex);
                if (fss != 0)
                    return timm("result", "unable to remove extent", "fsstatus", "%d", fss);
                destroy_extent(fs, ex);
                prev = INVALID_ADDRESS; /* prev isn't used in zero, but just to be safe */
            } else if (blocks.end > ex->node.r.start) {
                /* TODO: improve write_extent to trim extent on zero */
                if (m)
                    blocks.start = write_extent(f, ex, sg, blocks, m);
                else
                    blocks.start = range_intersection(blocks, ex->node.r).end;
            }
        }
        assert(blocks.start <= blocks.end); // XXX tmp
    } while (range_span(blocks) > 0);

    if (fsfile_get_length(&f->f) < q.end) {
        tfs_debug("   append; update length to %ld\n", q.end);
        int fss = filesystem_truncate_locked(&fs->fs, &f->f, q.end);
        if (fss != 0) {
            status s = timm("result", "unable to set file length");
            return timm_append(s, "fsstatus", "%d", fss);
        }
    }
    return STATUS_OK;
}

static void tfs_write(fsfile fsf,
                 sg_list sg, range q, status_handler complete)
{
    tfs fs = (tfs)fsf->fs;
    tfsfile f = (tfsfile)fsf;
    assert((q.start & MASK(fs->fs.blocksize_order)) == 0);
    tfs_debug("%s: fsfile %p, q %R, sg %p, sg count 0x%lx, complete %F\n", func_ss,
              f, q, sg, sg ? sg->count : 0, complete);
    if (fs->fs.ro) {
        status s = timm("result", "read-only filesystem");
        apply(complete, timm_append(s, "fsstatus", "%d", -EROFS));
        return;
    }

    merge m = allocate_merge(fs->fs.h, complete);
    status_handler sh = apply_merge(m);

    filesystem_lock(&fs->fs);
    status s = extents_range_handler(fs, f, q, sg, m);
    filesystem_unlock(&fs->fs);
    apply(sh, s);
}

closure_function(5, 1, void, fs_cache_sync_complete,
                 tfs, fs, status_handler, completion, boolean, flush_log, boolean, publish,
                 boolean, reclaim,
                 status s)
{
    if (!is_ok(s)) {
#ifdef KERNEL
        async_apply_status_handler(bound(completion), s);
#else
        apply(bound(completion), s);
#endif
        closure_finish();
        return;
    }
    if (bound(flush_log)) {
        tfs fs = bound(fs);
        filesystem_lock(&fs->fs);
#ifdef KERNEL
        /* The cache sync completed the data writes; publish them first. */
        if (bound(publish)) {
            bound(publish) = false;
            tfs_pub_cycle(fs, (status_handler)closure_self());
            filesystem_unlock(&fs->fs);
            return;
        }
        /* Memory reclaim promises no durability: let this sync's own flush
         * publish what it covers instead of adding one. */
        if (bound(reclaim))
            tfs_pub_lazy(fs);
#endif
        bound(flush_log) = false;
        log_flush(fs->tl, (status_handler)closure_self());
        filesystem_unlock(&fs->fs);
        return;
    }
#ifdef KERNEL
    tfs_device_flush(bound(fs), bound(completion));
#else
    struct storage_req req = {
        .op = STORAGE_OP_FLUSH,
        .blocks = irange(0, 0),
        .completion = bound(completion),
    };
    apply(bound(fs)->req_handler, &req);
#endif
    closure_finish();
}

static status_handler tfs_get_sync_handler(filesystem fs, fsfile fsf, boolean datasync,
                                           status_handler completion)
{
    boolean flush_log, reclaim = false;
    if (fsf) {
        flush_log = datasync ? (fsf->status & FSF_DIRTY_DATASYNC) : (fsf->status & FSF_DIRTY);
    } else {
        flush_log = true;
        reclaim = datasync;     /* see filesystem_flush_reclaim() */
    }
    boolean publish = flush_log && !reclaim;
#ifndef KERNEL
    publish = false;
#endif
    return closure(fs->h, fs_cache_sync_complete, (tfs)fs, completion, flush_log, publish, reclaim);
}

#ifdef KERNEL
closure_function(2, 1, status, filesystem_check_or_reserve_extent,
                 tfs, fs, tfsfile, f,
                 range q)
{
    tfs fs = bound(fs);
    tfsfile f = bound(f);
    tfs_debug("%s: file %p range %R\n", func_ss, f, q);
    if (fs->fs.ro) {
        status s = timm("result", "read-only filesystem");
        return timm_append(s, "fsstatus", "%d", -EROFS);
    }
    filesystem_lock(&fs->fs);
    status s = extents_range_handler(fs, f, q, 0, 0);
    /* Released storage becomes reusable once its release is durable: wait
     * for that while it makes progress, instead of failing with ENOSPC. */
    while (tfs_status_enospc(s) && (buffer_length(fs->deferred_frees) || fs->release_active)) {
        u64 generation = fs->release_generation;
        filesystem_unlock(&fs->fs);
        status w = wait_for_task((async_task)stack_closure(tfs_release_wait, fs));
        if (!is_ok(w))
            timm_dealloc(w);
        filesystem_lock(&fs->fs);
        if (fs->release_generation == generation)
            break;
        timm_dealloc(s);
        s = extents_range_handler(fs, f, q, 0, 0);
    }
    filesystem_unlock(&fs->fs);
    return s;
}

static int add_extents(tfs fs, range i, rangemap rm)
{
    extent ex;
    int fss;
    while (range_span(i)) {
        fss = create_extent(fs, i, true, &ex);
        if (fss != 0)
            return fss;
        assert(rangemap_insert(rm, &ex->node));
        i.start = ex->node.r.end;
    }
    return 0;
}

closure_function(1, 1, void, filesystem_op_complete,
                 fs_status_handler, sh,
                 status s)
{
    tfs_debug("%s: status %v\n", func_ss, s);
    apply(bound(sh), is_ok(s) ? 0 : -EIO);
    closure_finish();
}

closure_function(1, 1, boolean, destroy_extent_node,
                 tfs, fs,
                 rmnode n)
{
    destroy_extent(bound(fs), (extent)n);
    return true;
}

closure_func_basic(rmnode_handler, boolean, assert_no_node,
                   rmnode n)
{
    halt("tfs: temporary rangemap not empty on dealloc\n");
}

static int add_extents_to_file(tfsfile f, rangemap rm)
{
    tfs_debug("%s: tuple %p\n", func_ss, f->f.md);
    rangemap_foreach(rm, node) {
        rangemap_remove_node(rm, node);
        int s = add_extent_to_file(f, (extent) node);
        if (s != 0)
            return s;
    }
    return 0;
}

/* no longer async, but keep completion to match dealloc... */
void filesystem_alloc(fsfile f, long offset, long len,
                      boolean keep_size, fs_status_handler completion)
{
    assert(f);
    filesystem fs = f->fs;
    if (!fs_is_tfs(fs)) {
        apply(completion, -EINVAL);
        return;
    }

    range blocks = range_rshift_pad(irangel(offset, len), fs->blocksize_order);
    tfs_debug("%s: blocks %R%s\n", func_ss, blocks,
              keep_size ? ss(" (keep size)") : sstring_empty());

    rangemap new_rm = allocate_rangemap(fs->h);
    assert(new_rm != INVALID_ADDRESS);
    int status = 0;

    tfsfile fsf = (tfsfile)f;
    tfs tfs = (struct tfs *)fs;
    filesystem_lock(fs);
    u64 lastedge = blocks.start;
    rmnode curr = rangemap_first_node(fsf->extentmap);
    while (curr != INVALID_ADDRESS) {
        u64 edge = curr->r.start;
        range i = range_intersection(irange(lastedge, edge), blocks);
        if (range_span(i)) {
            status = add_extents(tfs, i, new_rm);
            if (status != 0)
                goto done;
        }
        lastedge = curr->r.end;
        curr = rangemap_next_node(fsf->extentmap, curr);
    }

    /* check for a gap between the last node and blocks.end */
    range i = range_intersection(irange(lastedge, blocks.end), blocks);
    if (range_span(i)) {
        status = add_extents(tfs, i, new_rm);
        if (status != 0)
            goto done;
    }

    status = add_extents_to_file(fsf, new_rm);
    if (status != 0)
        goto done;
    u64 end = offset + len;
    if (!keep_size && (end > fsfile_get_length(f))) {
        status = filesystem_truncate_locked(fs, f, end);
    }
done:
    filesystem_unlock(fs);
    deallocate_rangemap(new_rm, (status == 0 ?
                                 stack_closure_func(rmnode_handler, assert_no_node) :
                                 stack_closure(destroy_extent_node, tfs)));
    apply(completion, status);
}

void filesystem_dealloc(fsfile f, long offset, long len,
                        fs_status_handler completion)
{
    assert(f);
    /* A write with !sg indicates that the pagecache should zero the
       range. The null sg is propagated to the storage write for
       extent removal. */
    status_handler sh;
    sh = contextual_closure(filesystem_op_complete, completion);
    apply(pagecache_node_get_writer(fsfile_get_cachenode(f)), 0, irangel(offset, len), sh);
}
#endif

closure_func_basic(binding_handler, boolean, cleanup_directory_each,
                   value s, value v)
{
    if (is_tuple(v))
        cleanup_directory(v);
    return true;
}

static tuple cleanup_directory(tuple n)
{
    tuple parent = table_remove(&n->t, sym(..));
    if (!parent)
        return 0;
    tuple c = children(n);
    if (c)
        iterate(c, stack_closure_func(binding_handler, cleanup_directory_each));
    return parent;
}

static void destruct_dir_entry(tuple n)
{
    cleanup_directory(n);
    destruct_value(n, true);
}

static boolean tfs_file_unlink(tfs fs, tuple t)
{
    u64 link_count = fs_unlink(fs->files, t);
    if (link_count > 0) {
        filesystem_write_eav(fs, t, sym(nlink), value_from_u64(link_count), false);
        return false;
    }

    /* If a tuple is not present in the filesystem log dictionary, it can (and should) be destroyed
     * now (it won't be destroyed when the filesystem log is rebuilt). */
    return (get(t, sym(no_encode)) != 0);
}

static int do_mkentry(tfs fs, tuple parent, sstring name, tuple entry,
                            boolean persistent)
{
    symbol name_sym = sym_sstring(name);
    tuple c = children(parent);
    int s;

    /* XXX rather than ignore, there should be a wakeup on a sync blockq */
    if (persistent) {
        s = filesystem_write_eav(fs, c, name_sym, entry, false);
    } else {
        set(entry, sym(no_encode), null_value);
        s = 0;
    }

    if (s == 0) {
        set(c, name_sym, entry);
        table_set(fs->files, entry, INVALID_ADDRESS);
        fs_notify_create(entry, parent, name_sym);
    }
    fixup_directory(parent, entry);
    return s;
}

int filesystem_mkentry(filesystem fs, tuple cwd, sstring fp, tuple entry, boolean persistent, boolean recursive)
{
    if (fs->ro)
        return -EROFS;
    if (!fs_is_tfs(fs))
        return -EINVAL;
    tfs tfs = (struct tfs *)fs;
    filesystem_lock(fs);
    tuple parent = cwd ? cwd : fs->root;
    assert(children(parent));

    sstring token, rest;
    int status = 0;

    /* find the folder we need to mkentry in */
    sstring delim = ss("/");
    token = runtime_strtok_r(&fp, delim, &rest);
    while (!sstring_is_null(token)) {
        boolean final = sstring_is_empty(rest);
        tuple t = lookup(parent, sym_sstring(token));
        if (!t) {
            if (!final) {
                if (recursive) {
                    /* create intermediate directory */
                    tuple dir = fs_new_entry(fs);
                    set(dir, sym(children), allocate_tuple());
                    status = do_mkentry(tfs, parent, token, dir, persistent);
                    if (status != 0)
                        break;

                    parent = dir;
                    token = runtime_strtok_r(0, delim, &rest);
                    continue;
                }

                msg_err("%s: a path component (\"%s\") is missing", func_ss, token);
                status = -ENOENT;
                break;
            }

            status = do_mkentry(tfs, parent, token, entry, persistent);
            break;
        }

        if (final) {
            msg_debug("final path component (\"%s\") already exists\n", token);
            status = -EEXIST;
            break;
        }

        if (!children(t)) {
            msg_debug("a path component (\"%s\") is not a folder\n", token);
            status = -ENOTDIR;
            break;
        }

        parent = t;
        token = runtime_strtok_r(0, delim, &rest);
    }

    filesystem_unlock(fs);
    return status;
}

int filesystem_mkdirpath(filesystem fs, tuple cwd, sstring fp,
        boolean persistent)
{
    if (!fs_is_tfs(fs))
        return -EINVAL;
    tuple dir = fs_new_entry(fs);
    /* 'make it a folder' by attaching a children node to the tuple */
    set(dir, sym(children), allocate_tuple());

    return filesystem_mkentry(fs, cwd, fp, dir, persistent, true);
}

tfsfile allocate_fsfile(tfs fs, tuple md);

static void deallocate_fsfile(tfs fs, tfsfile f, rmnode_handler extent_destructor)
{
    deallocate_rangemap(f->extentmap, extent_destructor);
#ifdef KERNEL
    pagecache_deallocate_node(f->f.cache_node);
#endif
    deallocate(fs->fs.h, f, sizeof(*f));
}

closure_function(1, 1, boolean, free_extent,
                 tfs, fs,
                 rmnode n)
{
    destroy_extent(bound(fs), (extent)n);
    return true;
}

static int tfs_create(filesystem fs, tuple parent, string name, tuple md, fsfile *f)
{
    tfs tfs = (struct tfs *)fs;
    tfsfile fsf = 0;
    int fss;
    if (!md || is_regular(md)) {
        if (md)
            set(md, sym(extents), allocate_tuple());
        fsf = allocate_fsfile(tfs, md);
        if (fsf == INVALID_ADDRESS)
            return -ENOMEM;
        if (f) {
            *f = &fsf->f;
        }
    }
    if (parent && name && md) {
        fss = filesystem_write_eav(tfs, children(parent), intern(name), md, false);
        if (fsf) {
            if (fss == 0) {
                fsfile_reserve(&fsf->f);
            } else {
                table_set(tfs->files, md, 0);
                deallocate_fsfile(tfs, fsf, stack_closure(free_extent, tfs));
            }
        }
    } else {
        fss = 0;
    }
    if ((fss == 0) && !fsf)
        table_set(tfs->files, md, INVALID_ADDRESS);
    return fss;
}

static int tfs_link(filesystem fs, tuple parent, string name, tuple md)
{
    tfs tfs = (struct tfs *)fs;
    int ret = filesystem_write_eav(tfs, children(parent), intern(name), md, false);
    if (ret)
        return ret;
    symbol nlink = sym(nlink);
    return filesystem_write_eav(tfs, md, nlink, get(md, nlink), false);
}

static int tfs_unlink(filesystem fs, tuple parent, string name, tuple md,
                            boolean *destruct_md)
{
    tfs tfs = (struct tfs *)fs;
    int fss = filesystem_write_eav(tfs, children(parent), intern(name), 0, false);
    if (fss == 0) {
        *destruct_md = tfs_file_unlink(tfs, md);
    }
    return fss;
}

static int tfs_rename(filesystem fs, tuple old_parent, string old_name, tuple old_md,
                            tuple new_parent, string new_name, tuple new_md, boolean exchange,
                            boolean *destruct_md)
{
    int s = fs_check_rename(old_parent, old_md, new_parent, new_md, exchange);
    if (s != 0)
        return s;
    tfs tfs = (struct tfs *)fs;
    s = filesystem_write_eav(tfs, children(new_parent), intern(new_name), old_md, true);
    if (s == 0)
        s = filesystem_write_eav(tfs, children(old_parent), intern(old_name), exchange ? new_md : 0,
                                 exchange && new_md);
    if ((s == 0) && !exchange && new_md)
        *destruct_md = tfs_file_unlink(tfs, new_md);
    return s;
}

static int tfs_write_attr(filesystem fs, tuple md, sstring name, buffer value)
{
	tfs tfs = (struct tfs *)fs;
	return filesystem_write_eav(tfs, md, sym_sstring(name), value, false);
}

closure_function(1, 1, boolean, tfs_storage_freeblocks,
                 u64 *, free_blocks,
                 range r)
{
    *bound(free_blocks) += range_span(r);
    return true;
 }

static u64 tfs_freeblocks(filesystem fs)
{
    u64 free_blocks = 0;
    range q = irangel(0, fs->size >> fs->blocksize_order);
    tfs tfs = (struct tfs *)fs;
    tfs_storage_lock(tfs);
    rangemap_range_find_gaps(tfs->storage, q, stack_closure(tfs_storage_freeblocks, &free_blocks));
    tfs_storage_unlock(tfs);
    return free_blocks;
}

void filesystem_log_rebuild(tfs fs, log new_tl, status_handler sh)
{
    tfs_debug("%s(%F)\n", func_ss, sh);
    tuple root = fs->fs.root;
    cleanup_directory(root);
    boolean ok = log_write(new_tl, root);
    fixup_directory(root, root);
    if (ok) {
        fs->temp_log = new_tl;
        log_flush(new_tl, sh);
    } else {
        filesystem_unlock(&fs->fs);
        apply(sh, timm("result", "failed to write log"));
        filesystem_lock(&fs->fs);
    }
}

void filesystem_log_rebuild_done(tfs fs, log new_tl)
{
    tfs_debug("%s\n", func_ss);
    fs->tl = new_tl;
    fs->temp_log = 0;
}

closure_func_basic(status_handler, void, tfsfile_sync_complete,
                   status s)
{
    if (!is_ok(s)) {
        msg_err("TFS: failed to purge page cache node: %v", s);
        timm_dealloc(s);
    }
    tfsfile f = (tfsfile)struct_from_closure(fsfile, sync_complete);
    tfs fs = tfs_from_file(f);
    deallocate_fsfile(fs, f, stack_closure(free_extent, fs));
}

closure_function(1, 0, void, free_extents,
                 fsfile, f)
{
    fsfile f = bound(f);
    status_handler sh = init_closure_func(&f->sync_complete, status_handler, tfsfile_sync_complete);
#ifdef KERNEL
    pagecache_purge_node(f->cache_node, sh);
#else
    apply(sh, STATUS_OK);
#endif
}

#endif /* !TFS_READ_ONLY */

tfsfile allocate_fsfile(tfs fs, tuple md)
{
    heap h = fs->fs.h;
    tfsfile f = allocate(h, sizeof(struct tfsfile));
    if (f == INVALID_ADDRESS)
        return f;
    fsfile fsf = &f->f;
#ifdef KERNEL
    pagecache_node_reserve fs_reserve =
        closure(h, filesystem_check_or_reserve_extent, fs, f);
#endif
    thunk fs_free =
#ifndef TFS_READ_ONLY
        closure(h, free_extents, fsf);
#else
        0;
#endif
    if (fsfile_init(&fs->fs, fsf, md,
#ifdef KERNEL
                    fs_reserve,
#endif
                    fs_free) != 0) {
#ifdef KERNEL
        if (fs_reserve)
            deallocate_closure(fs_reserve);
#endif
        if (fs_free)
            deallocate_closure(fs_free);
        deallocate(h, f, sizeof(struct tfsfile));
        return INVALID_ADDRESS;
    }
    f->extentmap = allocate_rangemap(h);
    fsf->get_blocks = tfsfile_get_blocks;
    if (md)
        table_set(fs->files, md, f);

    return f;
}

fsfile fsfile_from_node(filesystem fs, tuple n)
{
    fsfile fsf = table_find(((tfs)fs)->files, n);
    return (fsf != INVALID_ADDRESS) ? fsf : 0;
}

closure_function(2, 1, void, log_complete,
                 filesystem_complete, fc, tfs, fs,
                 status s)
{
    tfs_debug("%s: complete %p, fs %p, status %v\n", func_ss, bound(fc), bound(fs), s);
    tfs fs = bound(fs);
    if (is_ok(s)) {
        tuple root = fs->fs.root;
        if (enumerate_dir_entries(fs, root)) {
#ifndef TFS_READ_ONLY
            fixup_directory(root, root);
#endif
        } else {
            s = timm("result", "failed to enumerate directory entries");
        }
    }
    apply(bound(fc), &fs->fs, s);
    closure_finish();
}

closure_func_basic(io_status_handler, void, ignore_io,
                 status s, bytes length)
{
}

sstring filesystem_get_label(filesystem fs)
{
    tfs t_fs = (tfs)fs;
    return sstring_from_cstring(t_fs->label, sizeof(t_fs->label));
}

/* Returns the byte offset just past the highest allocated or reserved storage
 * range (0 when nothing is allocated). Used by mkfs to size a partition to
 * what its filesystem actually occupies. */
u64 filesystem_storage_end(filesystem fs)
{
    tfs t = (tfs)fs;
    u64 end = 0;
    if (t->storage) {
        tfs_storage_lock(t);
        rangemap_foreach(t->storage, n) {
            if (n->r.end > end)
                end = n->r.end;
        }
        tfs_storage_unlock(t);
    }
    return end << fs->blocksize_order;
}

void filesystem_get_uuid(filesystem fs, u8 *uuid)
{
    runtime_memcpy(uuid, ((tfs)fs)->uuid, UUID_LEN);
}

boolean filesystem_reserve_log_space(tfs fs, u64 *next_offset, u64 *offset, u64 size)
{
    if (size == 0)
        size = filesystem_log_blocks(fs);
    if (*next_offset == INVALID_PHYSICAL) {
        *next_offset = filesystem_allocate_storage(fs, size);
        if (*next_offset == INVALID_PHYSICAL)
            return false;
    }
    if (offset) {
        *offset = *next_offset;
        *next_offset = filesystem_allocate_storage(fs, size);
    }
    return true;
}

static int tfs_get_fsfile(filesystem fs, tuple n, fsfile *f)
{
    return fs_get_fsfile(((tfs)fs)->files, n, f);
}

void create_filesystem(heap h,
                       u64 blocksize,
                       u64 size,
                       storage_req_handler req_handler,
                       boolean ro,
                       sstring label,
                       filesystem_complete complete)
{
    tfs_debug("%s\n", func_ss);
    tfs fs = allocate(h, sizeof(struct tfs));
    assert(fs != INVALID_ADDRESS);
    status s = filesystem_init(&fs->fs, h, size, blocksize, ro);
    if (!is_ok(s)) {
        deallocate(h, fs, sizeof(struct tfs));
        apply(complete, INVALID_ADDRESS, timm("result", "failed to init fs"));
        return;
    }
    if (!ignore_io_status)
        ignore_io_status = closure_func(h, io_status_handler, ignore_io);
    fs->files = allocate_table(h, identity_key, pointer_equal);
    fs->req_handler = req_handler;
    fs->fs.lookup = fs_lookup;
    fs->fs.get_fsfile = tfs_get_fsfile;
    fs->fs.file_read = tfs_read;
    fs->fs.get_inode = fs_get_inode;
    fs->fs.get_meta = tmpfs_get_meta;
#ifndef TFS_READ_ONLY
    fs->fs.file_write = tfs_write;
    fs->fs.create = tfs_create;
    fs->fs.link = tfs_link;
    fs->fs.unlink = tfs_unlink;
    fs->fs.rename = tfs_rename;
    fs->fs.write_attr = tfs_write_attr;
    fs->fs.truncate = tfs_truncate;
    fs->fs.get_freeblocks = tfs_freeblocks;
    fs->fs.get_sync_handler = tfs_get_sync_handler;
    fs->fs.destroy_fs = destroy_filesystem;
    fs->storage = allocate_rangemap(h);
    assert(fs->storage != INVALID_ADDRESS);
#ifdef KERNEL
    spin_lock_init(&fs->storage_lock);
    fs->page_order = pagecache_get_page_order();
    fs->zero_page = pagecache_get_zero_page();
    fs->deferred_frees = allocate_buffer(h, 4 * sizeof(range));
    fs->releasing_frees = allocate_buffer(h, 4 * sizeof(range));
    fs->release_waiters = allocate_vector(h, 4);
    assert(fs->deferred_frees != INVALID_ADDRESS && fs->releasing_frees != INVALID_ADDRESS &&
           fs->release_waiters != INVALID_ADDRESS);
    fs->release_generation = 0;
    fs->release_active = fs->release_scheduled = false;
    spin_lock_init(&fs->pub_lock);
    fs->inflight[0] = fs->inflight[1] = 0;
    fs->wgen = fs->cycle_gen = 0;
    fs->pub_cycle = 0;
    fs->pub_drained = fs->pub_eager = fs->pub_flush_issued = false;
    fs->pub_flush_sh = 0;
    fs->pubs = allocate_vector(h, 8);
    fs->pub_waiters = allocate_vector(h, 4);
    fs->pub_next_waiters = allocate_vector(h, 4);
    assert(fs->pubs != INVALID_ADDRESS && fs->pub_waiters != INVALID_ADDRESS &&
           fs->pub_next_waiters != INVALID_ADDRESS);
    fs->pub_active = fs->pub_error = fs->pub_timer_armed = false;
    fs->pub_flush = 0;
    init_timer(&fs->pub_timer);
#else
    fs->page_order = PAGESIZE;
    fs->zero_page = allocate_zero(h, PAGESIZE);
#endif
    fs->temp_log = 0;
#else
    fs->storage = 0;
#endif
    if (!sstring_is_null(label)) {
        int label_len = label.len;
        if (label_len >= sizeof(fs->label))
            label_len = sizeof(fs->label) - 1;
        runtime_memcpy(fs->label, label.ptr, label_len);
        fs->label[label_len] = '\0';
    }
#ifdef KERNEL
    fs->dma = heap_dma();
#else
    fs->dma = h;
#endif
    fs->next_extend_log_offset = INVALID_PHYSICAL;
    fs->next_new_log_offset = INVALID_PHYSICAL;
    /* A new volume (mkfs) gets the current format; an existing one takes the version of
     * its log header when the log is read. */
    fs->version = sstring_is_null(label) ? TFS_VERSION_V5 : TFS_VERSION;
    fs->map_bytes = 0;
    fs->tl = log_create(h, fs, !sstring_is_null(label), closure(h, log_complete, complete, fs));
}

#ifndef BOOT

closure_function(1, 1, boolean, dealloc_extent_node,
                 filesystem, fs,
                 rmnode n)
{
    tfs_map_free((tfs)bound(fs), (extent)n);
    deallocate(bound(fs)->h, n, sizeof(struct extent));
    return true;
}

closure_function(1, 1, boolean, tfs_storage_destroy,
                 heap, h,
                 rmnode n)
{
    deallocate(bound(h), n, sizeof(*n));
    return false;
}

/* If the filesystem is not read-only, this function can only be called after flushing any pending
 * writes. */
void destroy_filesystem(filesystem fs)
{
    tfs_debug("%s %p\n", func_ss, fs);
    tfs tfs = (struct tfs *)fs;
#if defined(KERNEL) && !defined(TFS_READ_ONLY)
    /* Before the extents are freed. Unpublished data is lost, as on a crash. */
    if (tfs->pub_timer_armed)
        remove_timer(kernel_timers, &tfs->pub_timer, 0);
    tfs_pub_drop(tfs);
#endif
    log_destroy(tfs->tl);
    table_foreach(tfs->files, k, v) {
        fs_notify_release(k, true);
        if (v != INVALID_ADDRESS)
            deallocate_fsfile(tfs, v, stack_closure(dealloc_extent_node, fs));
    }
    if (fs->root)
        destruct_dir_entry(fs->root);
    filesystem_deinit(fs);
    deallocate_table(tfs->files);
#if defined(KERNEL) && !defined(TFS_READ_ONLY)
    /* No release or publication cycle can be pending: each one holds a
     * filesystem reference. */
    deallocate_vector(tfs->pubs);
    deallocate_vector(tfs->pub_waiters);
    deallocate_vector(tfs->pub_next_waiters);
    deallocate_buffer(tfs->deferred_frees);
    deallocate_buffer(tfs->releasing_frees);
    deallocate_vector(tfs->release_waiters);
#endif
    deallocate_rangemap(tfs->storage, stack_closure(tfs_storage_destroy, fs->h));
    deallocate(fs->h, fs, sizeof(*fs));
}

#endif
