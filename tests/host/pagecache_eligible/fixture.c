#include <assert.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
typedef uint64_t u64;
typedef bool boolean;
#define struct_from_field(p,t,f) ((t)((char *)(p)-offsetof(__typeof__(*(t)0),f)))
#define MASK(n) ((1ULL<<(n))-1)
#define pagecache_debug(...) ((void)0)
#define halt(...) abort()
/* PRODUCTION_LIST */
/* PRODUCTION_PAGELIST */
/* PRODUCTION_STATES */
struct node { int refcount; };
typedef struct pagecache_page {
    struct list l, eligible_l;
    u64 state_offset;
    int refcount;
    bool evicted, delete_ok, deleted;
    struct node *node;
} *pagecache_page;
typedef struct pagecache {
    struct pagelist free, new, active, writing;
} *pagecache;
static void refcount_reserve(int *p) { ++*p; }
static void refcount_release(int *p) { assert(--*p>=0); }
/* PRODUCTION_HELPERS */
/* Only physical freeing and tree deletion are mocked. List/state transitions are real. */
static void pagecache_page_release_locked(pagecache pc,pagecache_page pp,bool full_delete)
{
    assert(pp->refcount>0);
    if (--pp->refcount) return;
    change_page_state_locked(pc,pp,PAGECACHE_PAGESTATE_FREE);
    if (full_delete && pp->delete_ok) {
        pagelist_remove(&pc->free,pp);
        pp->deleted=true;
    }
}
/* PRODUCTION_EVICT */
static struct pagecache cache;
static struct node node={.refcount=100000};
static struct pagecache_page pages[128];
static u64 digest=1469598103934665603ULL;
static void check(void)
{
    struct pagelist *lists[]={&cache.free,&cache.new,&cache.active,&cache.writing};
    for (unsigned i=0;i<4;i++) {
        struct pagelist *pl=lists[i];
        unsigned n=0,unmarked=0;
#ifdef HAS_ELIGIBLE_LIST
        list eligible=pl->eligible.next;
#endif
        list_foreach(&pl->l,l) {
            assert(l->next->prev==l && l->prev->next==l);
            pagecache_page p=struct_from_list(l,pagecache_page,l);
            n++; unmarked+=!p->evicted;
#ifdef HAS_ELIGIBLE_LIST
            if (!p->evicted) {
                assert(eligible==&p->eligible_l);
                assert(eligible->next->prev==eligible && eligible->prev->next==eligible);
                eligible=eligible->next;
            } else {
                assert(!p->eligible_l.next && !p->eligible_l.prev);
            }
#endif
            assert(!p->deleted && n<=128);
            digest=(digest^(u64)(p-pages+1))*1099511628211ULL;
        }
        assert(n==pl->pages);
#ifdef HAS_ELIGIBLE_LIST
        assert(eligible==&pl->eligible);
#endif
#ifdef HAS_ELIGIBLE
        assert(unmarked==pl->unmarked_pages);
#else
        (void)unmarked;
#endif
        digest=(digest^n)*1099511628211ULL;
    }
    for (unsigned i=0;i<128;i++) {
        assert(pages[i].refcount>=0);
#ifdef HAS_ELIGIBLE_LIST
        if (!pages[i].l.next) assert(!pages[i].eligible_l.next && !pages[i].eligible_l.prev);
#endif
        digest=(digest^(pages[i].state_offset+pages[i].refcount+pages[i].evicted))*1099511628211ULL;
    }
}
static void init_page(unsigned i,int refs,bool delete_ok)
{
    pagecache_page p=&pages[i];
    assert(!p->l.next && !p->l.prev);
    *p=(struct pagecache_page){.refcount=refs,.delete_ok=delete_ok,.node=&node,
                              .state_offset=(u64)PAGECACHE_PAGESTATE_ALLOC<<PAGECACHE_PAGESTATE_SHIFT};
}
static void state(unsigned i,int s) { change_page_state_locked(&cache,&pages[i],s);check(); }
static void evict(struct pagelist *pl,u64 requested,u64 expected)
{
    u64 actual=evict_from_list_locked(&cache,pl,requested);
    assert(actual==expected);digest=(digest^actual)*1099511628211ULL;check();
}
int main(void)
{
    page_list_init(&cache.free);page_list_init(&cache.new);
    page_list_init(&cache.active);page_list_init(&cache.writing);
    /* Outstanding references leave all pages marked on the list. */
    for (unsigned i=0;i<100;i++) { init_page(i,2,false);state(i,PAGECACHE_PAGESTATE_NEW); }
    evict(&cache.new,0,0);
    evict(&cache.new,100,0);
    for (unsigned n=0;n<100;n++) evict(&cache.new,100,0);
    /* A new reclaimable page beyond the marked prefix must still be found. */
    init_page(100,1,true);state(100,PAGECACHE_PAGESTATE_NEW);evict(&cache.new,1,1);
    /* Marked pages can be touched, moved, dirtied, written and completed. */
    for (unsigned i=0;i<100;i++) {
        state(i,PAGECACHE_PAGESTATE_ACTIVE);
        pagelist_touch(&cache.active,&pages[i]);check();
        state(i,PAGECACHE_PAGESTATE_DIRTY);
        state(i,PAGECACHE_PAGESTATE_WRITING);
        state(i,PAGECACHE_PAGESTATE_WRITING);
        state(i,PAGECACHE_PAGESTATE_DIRTY);
        state(i,PAGECACHE_PAGESTATE_WRITING);
        state(i,PAGECACHE_PAGESTATE_NEW);
    }
    evict(&cache.new,100,0);
    /* Release the last user ref, then reuse FREE -> ALLOC before clearing evicted. */
    for (unsigned i=0;i<100;i++) {
        pagecache_page_release_locked(&cache,&pages[i],false);check();
        state(i,PAGECACHE_PAGESTATE_ALLOC);
        pages[i].refcount=1;pages[i].evicted=false;check();
        state(i,PAGECACHE_PAGESTATE_READING);
        state(i,PAGECACHE_PAGESTATE_NEW);
        state(i,PAGECACHE_PAGESTATE_WRITING);
        state(i,PAGECACHE_PAGESTATE_DIRTY);
        state(i,PAGECACHE_PAGESTATE_WRITING);
        state(i,PAGECACHE_PAGESTATE_NEW);
        if (i%2) state(i,PAGECACHE_PAGESTATE_ACTIVE);
    }
    pagelist_touch(&cache.new,&pages[0]);check();
    pagelist_touch(&cache.active,&pages[1]);check();
    evict(&cache.new,7,7);evict(&cache.active,9,9);
    evict(&cache.new,100,43);evict(&cache.active,100,41);
    /* A never-published allocation can reach FREE without being marked. */
    init_page(101,0,false);state(101,PAGECACHE_PAGESTATE_FREE);
    state(101,PAGECACHE_PAGESTATE_ALLOC);
    pages[101].refcount=1;check();state(101,PAGECACHE_PAGESTATE_NEW);
    evict(&cache.new,1,1);
    printf("PAGECACHE LIST MODEL PASS %llu\n",(unsigned long long)digest);
}
