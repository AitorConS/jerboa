#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

/* Uninitialized-extent integrity. Freed blocks are first filled with a stale
 * pattern (0xA5) so that any exposure of old data is detectable; no expected
 * byte ever equals 0xA5. A fallocated file then receives partial, unaligned,
 * overlapping and concurrent writes, and every byte is compared with a
 * deterministic shadow: written bytes must match, unwritten bytes must be 0.
 *
 * Modes (argv[1]):
 *   full    first boot: build and verify; fsync; truncate/regrow; mark done.
 *           Reboot on the same disk (no argument handling needed): verifies
 *           the persisted image again.
 *   crash   like full, then issues unsynced writes and prints UNINIT CRASH
 *           POINT and waits; the host kills the VM. Reboot with crash-verify.
 *   After a crash the reboot (same disk, same arguments) finds the synced
 *   /uninit-crash marker and checks every byte is the synced value or the
 *   unsynced value, never stale 0xA5 and never another value.
 * Run with a 512 MiB disk and 256 MiB RAM. */

#define FILE_SIZE (64u << 20)
#define PAGE 4096u
#define STALE 0xa5
#define THREADS 4

static unsigned char *shadow;       /* expected synced image */
static unsigned char *unsynced;     /* expected value for unsynced writes, 0 = none */

static uint64_t lcg(uint64_t *s)
{
    *s = *s * 6364136223846793005ull + 1442695040888963407ull;
    return *s >> 17;
}

static unsigned char value_for(uint64_t off, unsigned gen)
{
    unsigned char v = (unsigned char)((off * 131u + gen * 29u) % 250u) + 1u; /* 1..250 */
    return v == STALE ? 0x5a : v;
}

static void pwrite_all(int fd, const void *b, size_t n, off_t off)
{
    const unsigned char *p = b;
    while (n) {
        ssize_t w = pwrite(fd, p, n, off);
        assert(w > 0);
        p += w; n -= (size_t)w; off += w;
    }
}

static void write_range(int fd, unsigned char *image, off_t off, size_t len, unsigned gen)
{
    unsigned char *b = malloc(len);
    assert(b);
    for (size_t i = 0; i < len; i++)
        b[i] = value_for((uint64_t)off + i, gen);
    pwrite_all(fd, b, len, off);
    memcpy(image + off, b, len);
    free(b);
}

/* Deterministic sequence of partial, unaligned and overlapping writes. */
static void build_image(int fd, unsigned char *image)
{
    uint64_t s = 12345;
    for (unsigned i = 0; i < 3000; i++) {
        off_t off = (off_t)(lcg(&s) % FILE_SIZE);
        size_t len;
        switch (i % 4) {
        case 0: off &= ~(off_t)(PAGE - 1); len = PAGE; break;          /* aligned page */
        case 1: len = 1 + lcg(&s) % 700; break;                         /* small unaligned */
        case 2: len = PAGE + lcg(&s) % (3 * PAGE); break;               /* spans pages */
        default: off &= ~(off_t)(PAGE - 1); len = 64 * 1024; break;     /* larger run */
        }
        if ((uint64_t)off + len > FILE_SIZE)
            len = FILE_SIZE - (size_t)off;
        write_range(fd, image, off, len, i % 7);
    }
}

struct worker { int fd; unsigned id; };

/* Threads write disjoint interleaved pages of the same 32 MiB extent region. */
static void *worker(void *arg)
{
    struct worker *w = arg;
    for (unsigned p = w->id; p < (32u << 20) / PAGE; p += THREADS * 3)
        write_range(w->fd, shadow, (off_t)(16u << 20) + (off_t)p * PAGE, PAGE, 9 + w->id);
    return NULL;
}

static void verify(int fd, const unsigned char *image, const char *what)
{
    unsigned char *b = malloc(1 << 20);
    assert(b);
    for (off_t off = 0; off < FILE_SIZE; off += 1 << 20) {
        ssize_t r = pread(fd, b, 1 << 20, off);
        assert(r == 1 << 20);
        for (unsigned i = 0; i < 1u << 20; i++) {
            unsigned char want = image[off + i];
            if (b[i] != want) {
                fprintf(stderr, "%s MISMATCH at %lld: got 0x%02x want 0x%02x%s\n", what,
                        (long long)(off + i), b[i], want, b[i] == STALE ? " (STALE DATA)" : "");
                abort();
            }
        }
    }
    free(b);
    printf("%s VERIFY OK\n", what);
}

static void expose_stale_blocks(void)
{
    /* Allocate, fill and free blocks so later allocations likely reuse them. */
    int fd = open("/stale", O_CREAT | O_TRUNC | O_WRONLY, 0600);
    assert(fd >= 0);
    unsigned char *b = malloc(1 << 20);
    assert(b);
    memset(b, STALE, 1 << 20);
    for (unsigned i = 0; i < 2 * FILE_SIZE / (1 << 20); i++)
        pwrite_all(fd, b, 1 << 20, (off_t)i << 20);
    free(b);
    assert(fsync(fd) == 0 && close(fd) == 0);
    assert(unlink("/stale") == 0);
    sync();
    puts("STALE BLOCKS FREED");
}

static void build_expected(void)
{
    /* Regenerate the synced image deterministically (used after reboot). */
    int fd = open("/dev/null", O_WRONLY);
    assert(fd >= 0);
    memset(shadow, 0, FILE_SIZE);
    build_image(fd, shadow);
    struct worker w[THREADS];
    for (unsigned i = 0; i < THREADS; i++) {
        w[i] = (struct worker){ fd, i };
        worker(&w[i]);
    }
    /* truncate to 40 MiB + 123 and regrow: the tail must read as zeros */
    memset(shadow + (40u << 20) + 123, 0, FILE_SIZE - (40u << 20) - 123);
    close(fd);
}

static void crash_writes(int fd)
{
    uint64_t s = 999;
    for (unsigned i = 0; i < 400; i++) {
        off_t off = (off_t)(lcg(&s) % (FILE_SIZE / PAGE)) * PAGE;
        unsigned char v = value_for((uint64_t)off, 31);
        unsigned char b[PAGE];
        memset(b, v, sizeof(b));
        pwrite_all(fd, b, PAGE, off);
        memset(unsynced + off, v, PAGE);
    }
}

int main(int argc, char **argv)
{
    setbuf(stdout, NULL);
    const char *mode = argc > 1 ? argv[1] : "full";
    shadow = calloc(1, FILE_SIZE);
    unsynced = calloc(1, FILE_SIZE);
    assert(shadow && unsynced);

    int done = open("/uninit-done", O_RDONLY);
    if (done >= 0) {
        int fd = open("/uninit-target", O_RDONLY);
        assert(fd >= 0);
        struct stat st;
        assert(fstat(fd, &st) == 0 && st.st_size == FILE_SIZE);
        build_expected();
        if (access("/uninit-post", F_OK) == 0) {
            /* post mode: the post-truncate writes were synced before a clean
             * shutdown, so they are part of the expected image. */
            uint64_t s = 999;
            for (unsigned i = 0; i < 400; i++) {
                off_t off = (off_t)(lcg(&s) % (FILE_SIZE / PAGE)) * PAGE;
                memset(shadow + off, value_for((uint64_t)off, 31), PAGE);
            }
            verify(fd, shadow, "POST RESTART");
            puts("UNINIT POST RESTART PASS");
            return 0;
        }
        int crashed = open("/uninit-crash", O_RDONLY);
        if (crashed >= 0) {
            close(crashed);
            uint64_t s = 999;
            for (unsigned i = 0; i < 400; i++) {
                off_t off = (off_t)(lcg(&s) % (FILE_SIZE / PAGE)) * PAGE;
                memset(unsynced + off, value_for((uint64_t)off, 31), PAGE);
            }
            unsigned char *b = malloc(1 << 20);
            unsigned long bad_bytes = 0, bad_pages = 0, stale = 0;
            long long last_page = -1;
            for (off_t off = 0; off < FILE_SIZE; off += 1 << 20) {
                assert(pread(fd, b, 1 << 20, off) == 1 << 20);
                for (unsigned i = 0; i < 1u << 20; i++) {
                    unsigned char g = b[i], a = shadow[off + i], u = unsynced[off + i];
                    if (g != a && !(u && g == u)) {
                        long long page = (long long)(off + i) / PAGE;
                        bad_bytes++; stale += g == STALE;
                        if (page != last_page) {
                            bad_pages++; last_page = page;
                            if (bad_pages <= 16)
                                printf("CRASH MISMATCH page at %lld: got 0x%02x synced 0x%02x "
                                       "unsynced 0x%02x\n", page * PAGE, g, a, u);
                        }
                    }
                }
            }
            if (bad_bytes) {
                printf("CRASH MISMATCH SUMMARY bytes %lu pages %lu stale_a5 %lu\n",
                       bad_bytes, bad_pages, stale);
                return 1;
            }
            puts("UNINIT CRASH VERIFY PASS");
        } else {
            verify(fd, shadow, "RESTART");
            puts("UNINIT INTEGRITY RESTART PASS");
        }
        return 0;
    }
    assert(errno == ENOENT);

    expose_stale_blocks();
    int fd = open("/uninit-target", O_CREAT | O_EXCL | O_RDWR, 0600);
    assert(fd >= 0);
    assert(fallocate(fd, 0, 0, FILE_SIZE) == 0);
    verify(fd, shadow, "FALLOCATE ZERO");
    build_image(fd, shadow);
    verify(fd, shadow, "PARTIAL");
    pthread_t t[THREADS];
    struct worker w[THREADS];
    for (unsigned i = 0; i < THREADS; i++) {
        w[i] = (struct worker){ fd, i };
        assert(pthread_create(&t[i], NULL, worker, &w[i]) == 0);
    }
    for (unsigned i = 0; i < THREADS; i++)
        assert(pthread_join(t[i], NULL) == 0);
    verify(fd, shadow, "CONCURRENT");
    assert(fsync(fd) == 0);
    assert(ftruncate(fd, (40u << 20) + 123) == 0);
    assert(ftruncate(fd, FILE_SIZE) == 0);
    memset(shadow + (40u << 20) + 123, 0, FILE_SIZE - (40u << 20) - 123);
    verify(fd, shadow, "TRUNCATE REGROW");
    assert(fsync(fd) == 0);
    int d = open("/uninit-done", O_CREAT | O_WRONLY, 0600);
    assert(d >= 0 && fsync(d) == 0 && close(d) == 0);
    if (strcmp(mode, "post") == 0) {
        crash_writes(fd);
        for (off_t off = 0; off < FILE_SIZE; off += PAGE)
            if (unsynced[off])
                memcpy(shadow + off, unsynced + off, PAGE);
        verify(fd, shadow, "POST CACHED");
        assert(fsync(fd) == 0);
        d = open("/uninit-post", O_CREAT | O_WRONLY, 0600);
        assert(d >= 0 && fsync(d) == 0 && close(d) == 0);
        assert(close(fd) == 0);
        puts("UNINIT POST PASS");
        return 0;
    }
    if (strcmp(mode, "crash") == 0) {
        d = open("/uninit-crash", O_CREAT | O_WRONLY, 0600);
        assert(d >= 0 && fsync(d) == 0 && close(d) == 0);
        crash_writes(fd);
        puts("UNINIT CRASH POINT");
        for (;;)
            sleep(1);
    }
    assert(close(fd) == 0);
    puts("UNINIT INTEGRITY PASS");
    return 0;
}
