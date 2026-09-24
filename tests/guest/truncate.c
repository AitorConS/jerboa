#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/mman.h>
#include <unistd.h>

static const off_t cuts[] = {0, 1, 511, 512, 513, 4095, 4096, 4097, 65535, 65536, 65537};
static atomic_int stop_sync;
static void *syncer(void *arg)
{
    int fd = *(int *)arg;
    while (!atomic_load(&stop_sync)) {
        assert(fsync(fd) == 0);
        usleep(1000);
    }
    return NULL;
}
struct shrink_arg { int fd; off_t cut; pthread_barrier_t *barrier; };
static void *shrink(void *arg)
{
    struct shrink_arg *a = arg;
    pthread_barrier_wait(a->barrier);
    assert(ftruncate(a->fd, a->cut) == 0);
    return NULL;
}
static void fill(int fd, off_t length, unsigned char value)
{
    unsigned char b[4096];
    memset(b, value, sizeof(b));
    for (off_t off = 0; off < length; off += sizeof(b))
        assert(pwrite(fd, b, sizeof(b), off) == sizeof(b));
}
static void verify(int fd, off_t length, off_t cut, unsigned char value)
{
    unsigned char b[4096];
    struct stat st;
    assert(fstat(fd, &st) == 0 && st.st_size == length);
    for (off_t off = 0; off < length; off += sizeof(b)) {
        size_t count = length - off < (off_t)sizeof(b) ? (size_t)(length - off) : sizeof(b);
        assert(pread(fd, b, count, off) == (ssize_t)count);
        for (size_t i = 0; i < count; i++) {
            unsigned char expected = off + (off_t)i < cut ? value : 0;
            if (b[i] != expected) {
                fprintf(stderr, "truncate mismatch offset=%lld got=%u expected=%u cut=%lld\n",
                        (long long)(off + i), b[i], expected, (long long)cut);
                assert(b[i] == expected);
            }
        }
    }
}
int main(void)
{
    int marker = open("/truncate-complete", O_RDONLY);
    int restart = marker >= 0;
    if (restart) assert(close(marker) == 0);
    const off_t large = 32 * 1024 * 1024;
    int fd = open("/truncate-large", O_RDWR | (restart ? 0 : O_CREAT | O_TRUNC), 0600);
    assert(fd >= 0);
    if (!restart) {
        pthread_t thread;
        assert(pthread_create(&thread, NULL, syncer, &fd) == 0);
        for (unsigned round = 1; round <= 16; round++) {
            fill(fd, large, round);
            assert(ftruncate(fd, 4096) == 0);
            assert(ftruncate(fd, large) == 0);
            verify(fd, large, 4096, round);
            assert(fsync(fd) == 0);
            verify(fd, large, 4096, round);
        }
        atomic_store(&stop_sync, 1);
        assert(pthread_join(thread, NULL) == 0);
    }
    verify(fd, large, 4096, 16);
    assert(close(fd) == 0);
    for (unsigned j = 0; j < sizeof(cuts) / sizeof(*cuts); j++) {
        char path[64];
        snprintf(path, sizeof(path), "/truncate-boundary-%u", j);
        fd = open(path, O_RDWR | (restart ? 0 : O_CREAT | O_TRUNC), 0600);
        assert(fd >= 0);
        const off_t length = 128 * 1024;
        if (!restart) {
            fill(fd, length, j + 1);
            if (j & 1) assert(fsync(fd) == 0);
            assert(ftruncate(fd, cuts[j]) == 0);
            assert(ftruncate(fd, length) == 0);
            verify(fd, length, cuts[j], j + 1);
            assert(fsync(fd) == 0);
            errno = 0;
            assert(ftruncate(fd, -1) == -1 && errno == EINVAL);
        }
        verify(fd, length, cuts[j], j + 1);
        assert(close(fd) == 0);
        fd = open(path, O_RDONLY);
        assert(fd >= 0);
        assert(ftruncate(fd, 0) == -1);
        assert(close(fd) == 0);
    }
    if (!restart) {
        fd = open("/truncate-race", O_CREAT | O_RDWR | O_TRUNC, 0600);
        assert(fd >= 0);
        for (unsigned round = 0; round < 32; round++) {
            fill(fd, 131072, 0x52);
            pthread_barrier_t barrier;
            assert(pthread_barrier_init(&barrier, NULL, 2) == 0);
            struct shrink_arg args[2] = {{fd, 4096, &barrier}, {fd, 65536, &barrier}};
            pthread_t threads[2];
            for (unsigned i = 0; i < 2; i++) assert(pthread_create(&threads[i], NULL, shrink, &args[i]) == 0);
            for (unsigned i = 0; i < 2; i++) assert(pthread_join(threads[i], NULL) == 0);
            assert(pthread_barrier_destroy(&barrier) == 0);
            assert(ftruncate(fd, 131072) == 0);
            verify(fd, 131072, 4096, 0x52);
        }
        assert(close(fd) == 0);
    }
    fd = open("/truncate-mapped", O_RDWR | (restart ? 0 : O_CREAT | O_TRUNC), 0600);
    assert(fd >= 0);
    if (!restart) {
        assert(ftruncate(fd, 65536) == 0);
        unsigned char *mapping = mmap(NULL, 65536, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
        assert(mapping != MAP_FAILED);
        memset(mapping, 0x75, 65536);
        assert(msync(mapping, 65536, MS_SYNC) == 0);
        assert(ftruncate(fd, 513) == 0 && ftruncate(fd, 65536) == 0);
        verify(fd, 65536, 513, 0x75);
        for (unsigned i = 0; i < 65536; i++) assert(mapping[i] == (i < 513 ? 0x75 : 0));
        assert(munmap(mapping, 65536) == 0 && fsync(fd) == 0);
    }
    verify(fd, 65536, 513, 0x75);
    assert(close(fd) == 0);
    if (!restart) {
        fd = open("/truncate-open", O_CREAT | O_RDWR, 0600);
        assert(fd >= 0);
        fill(fd, 65536, 0x37);
        int other = open("/truncate-open", O_RDWR | O_TRUNC);
        assert(other >= 0 && ftruncate(other, 65536) == 0);
        verify(fd, 65536, 0, 0);
        assert(close(other) == 0 && close(fd) == 0);
        marker = open("/truncate-complete", O_CREAT | O_WRONLY, 0600);
        assert(marker >= 0 && fsync(marker) == 0 && close(marker) == 0);
    }
    puts(restart ? "TRUNCATE RESTART PASS" : "TRUNCATE PASS");
    return 0;
}
