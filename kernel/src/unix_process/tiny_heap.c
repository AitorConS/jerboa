#include <runtime.h>

typedef struct tiny {
    struct heap h;
    void *next;
    u64 offset;
    void *base;
    heap parent;
}*tiny;
    
static u64 alloc(heap h, u64 size)
{
    tiny t = (tiny)h;

    /* Host x86-64 values keep their type tag in the byte before the
     * allocation. Every result must also preserve pointer/value alignment. */
    u64 prefix = 0;
#ifndef __aarch64__
    prefix = sizeof(u64);
#endif
    if (size > t->parent->pagesize)
        return INVALID_PHYSICAL;
    size = pad(size, sizeof(u64));
    if (size > t->parent->pagesize - sizeof(void *) - prefix)
        return INVALID_PHYSICAL;
    if ((t->offset + prefix + size) > t->parent->pagesize) {
        void *new = allocate(t->parent, t->parent->pagesize);
        if (new == INVALID_ADDRESS)
            return INVALID_PHYSICAL;
        *(void **)new = t->base;
        t->base = new;
        t->offset = sizeof(void *);
        return alloc(h, size);
    }
    u64 res = u64_from_pointer(t->base) + t->offset + prefix;
    t->offset += prefix + size;
#ifndef __aarch64__
    tag(pointer_from_u64(res), tag_unknown);
#endif
    return res;
}


static void destroy(heap h)
{
    tiny t = (tiny)h;
    heap p = t->parent;
    void * x=t->base;
    while(x != t) {
        void *next = *(void **)x;
        deallocate(p, x, p->pagesize);
        x = next;
    }
    deallocate(p, t, p->pagesize);
}

heap make_tiny_heap(heap parent)
{
    void *x = mem_alloc(parent, parent->pagesize, MEM_NOFAIL);
    tiny t = (tiny)x;
    t->h.alloc = alloc;
    t->h.dealloc = leak;
    t->h.destroy = destroy;
    t->base = x;
    t->parent = parent;
    t->offset = sizeof(struct tiny);
    return x;
}
