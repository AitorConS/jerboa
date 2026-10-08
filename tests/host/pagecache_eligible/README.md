# Page-cache eviction-list regression model

Run `python3 tests/host/pagecache_eligible/run.py`. It extracts production list
helpers, state transitions, initialization and eviction code and compiles them
with ASAN/UBSAN against bounded page objects. The list implementation is also
taken from production. Physical freeing and tree deletion are mocked.

An independent traversal checks that the filtered list has exactly the unmarked
pages in the same order as the full list after every operation. Cases include
zero requests, retained marked pages, a reclaimable page behind a marked prefix,
move/touch, dirty/writeback transitions on marked and unmarked pages, deletion,
last-reference release, and reuse after FREE->ALLOC.

This serial model assumes the existing state lock. It does not establish real
SMP correctness, physical memory lifetime, filesystem integrity or performance.
Guest pressure/ENOSPC, concurrent writers, fsync/readback and reboot tests remain
required. The differential baseline comparison is preserved with the benchmark
evidence; this regression test runs the current production source.
