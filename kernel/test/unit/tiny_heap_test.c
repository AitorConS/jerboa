#include <runtime.h>
#include <stdlib.h>
#include <string.h>

heap make_tiny_heap(heap parent);
static void *pages[8];
static unsigned live;

static u64 page_alloc(heap h, bytes size)
{
    assert(size == h->pagesize && live < 8);
    void *p = malloc(size);
    assert(p);
    memset(p, 0xa5, size);
    pages[live++] = p;
    return u64_from_pointer(p);
}

static void page_free(heap h, u64 addr, bytes size)
{
    assert(size == h->pagesize);
    void *p = pointer_from_u64(addr);
    unsigned i;
    for (i = 0; i < live && pages[i] != p; i++);
    assert(i < live);
    pages[i] = pages[--live];
    free(p);
}

int main(void)
{
    struct heap parent = {.alloc = page_alloc, .dealloc = page_free, .pagesize = 4096};
    for (int count = 1; count <= 200; count += 199) {
        heap h = make_tiny_heap(&parent);
        void *allocations[200];
        for (int i = 0; i < count; i++) {
            bytes size = (i % 67) + 1;
            void *p = allocate(h, size);
            assert(p != INVALID_ADDRESS);
            assert((u64_from_pointer(p) & 7) == 0);
            assert(tagof(p) == tag_unknown);
            memset(p, 0xa5, size);
            allocations[i] = p;
        }
        /* A later allocation's tag must not overwrite its predecessor. */
        for (int i = 0; i < count; i++) {
            unsigned char *p = allocations[i];
            for (bytes j = 0; j < (i % 67) + 1; j++)
                assert(p[j] == 0xa5);
        }
        assert(allocate(h, 4096) == INVALID_ADDRESS);
        destroy_heap(h);
        assert(live == 0);
    }
    return 0;
}
