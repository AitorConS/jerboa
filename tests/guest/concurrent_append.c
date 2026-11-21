/* Files that grow at the same time must not get an extent per write.
 *
 * Four threads append 4 KiB writes to their own file, 1 GiB each, without
 * preallocation. Their reservations take turns allocating storage, so the
 * block after each file's last extent is usually taken by another file. A
 * kernel that then gives every write its own extent registers some 250k
 * extents per GiB for publication and crashes on a failed allocation (see
 * tfs_append_headroom()). Run with 2 GiB RAM and a disk of at least 5 GiB,
 * then boot the same disk again to check that every byte persisted. */
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define THREADS 4
#define BLOCK 4096
#define FILE_BYTES (1024L * 1024 * 1024)
#define BLOCKS (FILE_BYTES / BLOCK)

static void fill(unsigned char *b, int file, long block)
{
    for (int i = 0; i < BLOCK; i += 16) {
        long v[2] = {file, block * BLOCK + i};
        memcpy(b + i, v, sizeof(v));
    }
}

static void *writer(void *arg)
{
    int file = (int)(long)arg;
    char path[32];
    unsigned char b[BLOCK];
    snprintf(path, sizeof(path), "/append-%d", file);
    int fd = open(path, O_CREAT | O_EXCL | O_WRONLY, 0600);
    assert(fd >= 0);
    for (long i = 0; i < BLOCKS; i++) {
        fill(b, file, i);
        assert(write(fd, b, BLOCK) == BLOCK);
    }
    assert(fsync(fd) == 0);
    assert(close(fd) == 0);
    return 0;
}

static void *checker(void *arg)
{
    int file = (int)(long)arg;
    char path[32];
    unsigned char b[BLOCK], want[BLOCK];
    snprintf(path, sizeof(path), "/append-%d", file);
    int fd = open(path, O_RDONLY);
    assert(fd >= 0);
    assert(lseek(fd, 0, SEEK_END) == FILE_BYTES);
    assert(lseek(fd, 0, SEEK_SET) == 0);
    for (long i = 0; i < BLOCKS; i++) {
        assert(read(fd, b, BLOCK) == BLOCK);
        fill(want, file, i);
        if (memcmp(b, want, BLOCK)) {
            fprintf(stderr, "file %d block %ld differs\n", file, i);
            abort();
        }
    }
    assert(close(fd) == 0);
    return 0;
}

int main(void)
{
    setvbuf(stdout, 0, _IONBF, 0);
    int existed = access("/append-0", F_OK) == 0;
    assert(existed || errno == ENOENT);
    pthread_t t[THREADS];
    if (!existed) {
        for (long i = 0; i < THREADS; i++)
            assert(pthread_create(&t[i], 0, writer, (void *)i) == 0);
        for (int i = 0; i < THREADS; i++)
            assert(pthread_join(t[i], 0) == 0);
    }
    for (long i = 0; i < THREADS; i++)
        assert(pthread_create(&t[i], 0, checker, (void *)i) == 0);
    for (int i = 0; i < THREADS; i++)
        assert(pthread_join(t[i], 0) == 0);
    puts(existed ? "CONCURRENT APPEND RESTART PASS" : "CONCURRENT APPEND PASS");
    fflush(stdout);
    return 0;
}
