#define _GNU_SOURCE
#include <assert.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define JOBS 4
#define BLOCK_SIZE 4096
#define BLOCKS 65536

static void sync_file(int fd)
{
    if (fsync(fd) != 0) {
        perror("fsync");
        abort();
    }
}

/* Each thread owns a 256 MiB file. The odd multiplier visits every block once
 * in a non-sequential order; deterministic contents can be checked after boot. */
static void *writer(void *argument)
{
    uintptr_t job = (uintptr_t)argument;
    char name[32];
    snprintf(name, sizeof(name), "/random-%lu", (unsigned long)job);
    int fd = open(name, O_RDWR | O_CREAT | O_TRUNC, 0600);
    assert(fd >= 0);
    assert(posix_fallocate(fd, 0, (off_t)BLOCKS * BLOCK_SIZE) == 0);
    unsigned char buffer[BLOCK_SIZE];
    for (unsigned i = 0; i < BLOCKS; i++) {
        unsigned block = (i * 40503U) % BLOCKS;
        memset(buffer, (block + job * 17) % 251, sizeof(buffer));
        assert(pwrite(fd, buffer, sizeof(buffer), (off_t)block * BLOCK_SIZE) == sizeof(buffer));
        if ((i + 1) % 4096 == 0) sync_file(fd);
    }
    sync_file(fd);
    assert(close(fd) == 0);
    return NULL;
}

static void verify(void)
{
    unsigned char buffer[BLOCK_SIZE];
    for (unsigned job = 0; job < JOBS; job++) {
        char name[32];
        snprintf(name, sizeof(name), "/random-%u", job);
        int fd = open(name, O_RDONLY);
        assert(fd >= 0);
        for (unsigned block = 0; block < BLOCKS; block++) {
            assert(read(fd, buffer, sizeof(buffer)) == sizeof(buffer));
            for (unsigned byte = 0; byte < sizeof(buffer); byte++)
                assert(buffer[byte] == (block + job * 17) % 251);
        }
        assert(close(fd) == 0);
    }
}

int main(void)
{
    if (access("/random-done", F_OK) == 0) {
        verify();
        puts("RANDOM RESTART PASS");
        return 0;
    }
    pthread_t threads[JOBS];
    for (uintptr_t i = 0; i < JOBS; i++) assert(pthread_create(&threads[i], NULL, writer, (void *)i) == 0);
    for (unsigned i = 0; i < JOBS; i++) assert(pthread_join(threads[i], NULL) == 0);
    verify();
    int fd = open("/random-done", O_WRONLY | O_CREAT, 0600);
    assert(fd >= 0 && fsync(fd) == 0 && close(fd) == 0);
    puts("RANDOM WRITE PASS");
    return 0;
}
