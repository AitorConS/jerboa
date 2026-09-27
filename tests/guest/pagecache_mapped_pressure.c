/* Exercise retained file-page references during eviction. Use 128 MiB RAM and
 * a 1 GiB disk, then reboot the same disk to check shared-write persistence. */
#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <sched.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

#define PAGE 4096
#define MAPPED (16 * 1024 * 1024)
#define CHUNK (1024 * 1024)
#define PRESSURE_CHUNKS 384
static unsigned char *shared_map, *private_map;
static atomic_bool stop_reader;
static atomic_uint rounds;

static void *reader(void *unused)
{
    (void)unused;
    while (!atomic_load(&stop_reader)) {
        for (unsigned i = 0; i < MAPPED; i += PAGE) {
            assert(shared_map[i] == 0x31 && shared_map[i + PAGE - 1] == 0x31);
            assert(private_map[i] == 0xa7 && private_map[i + PAGE - 1] == 0x31);
        }
        atomic_fetch_add(&rounds, 1);
        sched_yield();
    }
    return NULL;
}

static void pressure(int fd, unsigned char *buffer)
{
    assert(ftruncate(fd, 0) == 0 && lseek(fd, 0, SEEK_SET) == 0);
    for (unsigned i = 0; i < PRESSURE_CHUNKS; i++) {
        memset(buffer, i % 251, CHUNK);
        assert(write(fd, buffer, CHUNK) == CHUNK);
    }
    assert(fsync(fd) == 0);
}

static void verify_file(int fd, unsigned char *buffer)
{
    assert(lseek(fd, 0, SEEK_SET) == 0);
    for (unsigned i = 0; i < MAPPED / CHUNK; i++) {
        assert(read(fd, buffer, CHUNK) == CHUNK);
        for (unsigned j = 0; j < CHUNK; j++)
            assert(buffer[j] == (j % PAGE == 0 ? 0x53 : 0x31));
    }
}

int main(void)
{
    unsigned char *buffer = malloc(CHUNK);
    assert(buffer);
    int marker = open("/mapped-pressure-complete", O_RDONLY);
    if (marker >= 0) {
        assert(close(marker) == 0);
        int fd = open("/mapped-pressure-data", O_RDONLY);
        assert(fd >= 0);
        verify_file(fd, buffer);
        assert(close(fd) == 0);
        free(buffer);
        puts("MAPPED PRESSURE RESTART PASS");
        return 0;
    }
    assert(errno == ENOENT);
    int fd = open("/mapped-pressure-data", O_CREAT | O_EXCL | O_RDWR, 0600);
    assert(fd >= 0);
    memset(buffer, 0x31, CHUNK);
    for (unsigned i = 0; i < MAPPED / CHUNK; i++)
        assert(write(fd, buffer, CHUNK) == CHUNK);
    assert(fsync(fd) == 0);
    shared_map = mmap(NULL, MAPPED, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    private_map = mmap(NULL, MAPPED, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
    assert(shared_map != MAP_FAILED && private_map != MAP_FAILED);
    for (unsigned i = 0; i < MAPPED; i += PAGE) {
        assert(private_map[i] == 0x31);
        private_map[i] = 0xa7;
    }
    pthread_t thread;
    assert(pthread_create(&thread, NULL, reader, NULL) == 0);
    while (!atomic_load(&rounds)) sched_yield();
    int temporary = open("/mapped-pressure-temporary", O_CREAT | O_EXCL | O_RDWR, 0600);
    assert(temporary >= 0);
    pressure(temporary, buffer);
    atomic_store(&stop_reader, 1);
    assert(pthread_join(thread, NULL) == 0 && atomic_load(&rounds) > 0);
    for (unsigned i = 0; i < MAPPED; i += PAGE) shared_map[i] = 0x53;
    assert(msync(shared_map, MAPPED, MS_SYNC) == 0 && fsync(fd) == 0);
    pressure(temporary, buffer);
    for (unsigned i = 0; i < MAPPED; i++) {
        assert(shared_map[i] == (i % PAGE == 0 ? 0x53 : 0x31));
        assert(private_map[i] == (i % PAGE == 0 ? 0xa7 : 0x31));
    }
    verify_file(fd, buffer);
    assert(munmap(private_map, MAPPED) == 0 && munmap(shared_map, MAPPED) == 0);
    assert(close(fd) == 0 && close(temporary) == 0);
    assert(unlink("/mapped-pressure-temporary") == 0);
    marker = open("/mapped-pressure-complete", O_CREAT | O_EXCL | O_WRONLY, 0600);
    assert(marker >= 0 && fsync(marker) == 0 && close(marker) == 0);
    free(buffer);
    puts("MAPPED PRESSURE PASS");
    return 0;
}
