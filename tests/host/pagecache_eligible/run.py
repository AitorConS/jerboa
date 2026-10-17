"""Exercise production list/state/eviction bodies under ASAN/UBSAN."""
import subprocess,sys,tempfile
from pathlib import Path
here=Path(__file__).resolve().parent
repo=Path(sys.argv[1]) if len(sys.argv)>1 else here.parents[2]
def function(source,signature):
    start=source.index(signature);brace=source.index('{',start);depth=1;end=brace+1
    while depth:
        depth+=(source[end]=='{')-(source[end]=='}');end+=1
    return source[start:end]
list_all=(repo/'kernel/src/runtime/list.h').read_text()
list_source=list_all[:list_all.index('static inline void list_replace')]
list_source+='\n'+function(list_all,'static inline struct list *list_begin(')
list_source+='\n'+function(list_all,'static inline struct list *list_end(')
outputs=[]
for tag,root in [('current',repo)]:
    source=(root/'kernel/src/kernel/pagecache.c').read_text()
    header=(root/'kernel/src/kernel/pagecache_internal.h').read_text()
    pagelist=header[:header.index('typedef struct page_completion')]
    states=header[header.index('#define PAGECACHE_PAGESTATE_SHIFT'):header.index('typedef struct pagecache_page *')]
    names=['static inline int page_state(', 'static inline void pagelist_enqueue(',
           'static inline void pagelist_remove(', 'static inline void pagelist_move(',
           'static inline void pagelist_touch(', 'static inline void change_page_state_locked(',
           'static inline void page_list_init(']
    body=(here/'fixture.c').read_text().replace('/* PRODUCTION_LIST */',list_source)
    body=body.replace('/* PRODUCTION_PAGELIST */',pagelist).replace('/* PRODUCTION_STATES */',states)
    body=body.replace('/* PRODUCTION_HELPERS */','\n'.join(function(source,n) for n in names))
    body=body.replace('/* PRODUCTION_EVICT */',function(source,'static u64 evict_from_list_locked('))
    if 'struct list eligible' in pagelist:body='#define HAS_ELIGIBLE_LIST 1\n'+body
    with tempfile.TemporaryDirectory(prefix='pc-eligible-') as temp:
        c=Path(temp)/'model.c';binary=Path(temp)/'model';c.write_text(body)
        subprocess.run(['cc','-std=gnu11','-Wall','-Wextra','-Werror','-Wno-unused-function',
                        '-fsanitize=address,undefined','-fno-omit-frame-pointer','-g',str(c),
                        '-o',str(binary)],check=True)
        result=subprocess.check_output([str(binary)],text=True)
        outputs.append(result);print(tag,result.strip(),flush=True)
print('Filtered-list order equals independently filtered full list throughout all transitions.')
