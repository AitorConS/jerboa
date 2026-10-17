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
#include <time.h>
#include <unistd.h>

#define PAGE 4096
#define MAPPED (16 * 1024 * 1024)
#define CHUNK (1024 * 1024)
#define PRESSURE_CHUNKS 384
static unsigned char *shared_map, *private_map;
static atomic_bool stop_reader;
static atomic_uint rounds;
static atomic_bool reader_first_shared_page;

static void *reader(void *unused)
{
    (void)unused;
    assert(shared_map[0] == 0x31);
    atomic_store(&reader_first_shared_page, 1);
    while (!atomic_load(&stop_reader)) {
        int first_round = atomic_load(&rounds) == 0;
        for (unsigned i = 0; i < MAPPED; i += PAGE) {
            assert(shared_map[i] == 0x31 && shared_map[i + PAGE - 1] == 0x31);
            if (first_round && i && i % (256 * PAGE) == 0)
                printf("READER SHARED %u MiB\n", i / (1024 * 1024));
        }
        if (first_round) puts("READER SHARED PASS");
        for (unsigned i = 0; i < MAPPED; i += PAGE) {
            assert(private_map[i] == 0xa7 && private_map[i + PAGE - 1] == 0x31);
            if (first_round && i && i % (256 * PAGE) == 0)
                printf("READER PRIVATE %u MiB\n", i / (1024 * 1024));
        }
        if (first_round) puts("READER PRIVATE PASS");
        atomic_fetch_add(&rounds, 1);
        if (first_round) puts("READER BEFORE YIELD");
        sched_yield();
        if (first_round) puts("READER AFTER YIELD");
    }
    return NULL;
}

static void pressure(int fd, unsigned char *buffer)
{
    assert(ftruncate(fd, 0) == 0 && lseek(fd, 0, SEEK_SET) == 0);
    puts("PRESSURE START");
    for (unsigned i = 0; i < PRESSURE_CHUNKS; i++) {
        memset(buffer, i % 251, CHUNK);
        assert(write(fd, buffer, CHUNK) == CHUNK);
        if ((i + 1) % 8 == 0)
            printf("PRESSURE WRITE %u/%u MiB\n", i + 1, PRESSURE_CHUNKS);
    }
    assert(fsync(fd) == 0);
    puts("PRESSURE FSYNC PASS");
}

static void verify_file(int fd, unsigned char *buffer)
{
    assert(lseek(fd, 0, SEEK_SET) == 0);
    for (unsigned i = 0; i < MAPPED / CHUNK; i++) {
        assert(read(fd, buffer, CHUNK) == CHUNK);
        for (unsigned j = 0; j < CHUNK; j++) {
            unsigned char expected = (i == 0 && j == 0) ? 0x53 : 0x31;
            if (buffer[j] != expected) {
                fprintf(stderr, "FILE MISMATCH offset=%u got=0x%02x expected=0x%02x\n",
                        i * CHUNK + j, buffer[j], expected);
                assert(buffer[j] == expected);
            }
        }
    }
}

static void verify_mapping_contents(void)
{
    for (unsigned i = 0; i < MAPPED; i += PAGE) {
        assert(shared_map[i] == 0x31 && shared_map[i + PAGE - 1] == 0x31);
        assert(private_map[i] == 0xa7 && private_map[i + PAGE - 1] == 0x31);
        if (i && i % (256 * PAGE) == 0)
            printf("MAPPED VERIFY %u MiB\n", i / (1024 * 1024));
    }
    puts("MAPPED PRIVATE COW / SHARED READ PASS");
}

int main(int argc, char **argv)
{
    setbuf(stdout, NULL);
    const char *mode = argc > 1 ? argv[1] : "full";
    assert(strcmp(mode, "single") == 0 || strcmp(mode, "threads") == 0 ||
           strcmp(mode, "full") == 0);
    unsigned char *buffer = malloc(CHUNK);
    assert(buffer);
    int fd = open("/mapped-pressure-data", O_RDONLY);
    if (fd >= 0) {
        unsigned char first;
        assert(read(fd, &first, 1) == 1);
        if (first == 0x53) {
            verify_file(fd, buffer);
            assert(close(fd) == 0);
            free(buffer);
            puts("MAPPED PRESSURE RESTART PASS");
            return 0;
        }
        fprintf(stderr, "REBOOT FOUND UNCOMMITTED SHARED BYTE 0x%02x\n", first);
        assert(first == 0x53);
    } else {
        assert(errno == ENOENT);
    }
    if (fd >= 0)
        assert(close(fd) == 0);
    fd = open("/mapped-pressure-data", O_CREAT | O_EXCL | O_RDWR, 0600);
    assert(fd >= 0);
    memset(buffer, 0x31, CHUNK);
    for (unsigned i = 0; i < MAPPED / CHUNK; i++)
        assert(write(fd, buffer, CHUNK) == CHUNK);
    assert(fsync(fd) == 0);
    puts("MAPS INITIALIZED");
    shared_map = mmap(NULL, MAPPED, PROT_READ, MAP_SHARED, fd, 0);
    private_map = mmap(NULL, MAPPED, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
    assert(shared_map != MAP_FAILED && private_map != MAP_FAILED);
    puts("MAPS CREATED");
    for (unsigned i = 0; i < MAPPED; i += PAGE) {
        assert(private_map[i] == 0x31);
        private_map[i] = 0xa7;
    }
    puts("PRIVATE COW INITIALIZED");
    if (strcmp(mode, "threads") != 0)
        verify_mapping_contents();
    if (strcmp(mode, "single") == 0) {
        assert(munmap(private_map, MAPPED) == 0 && munmap(shared_map, MAPPED) == 0);
        assert(close(fd) == 0);
        assert(unlink("/mapped-pressure-data") == 0);
        free(buffer);
        puts("MAPPED SINGLE-THREAD REPRO PASS");
        return 0;
    }
    pthread_t thread;
    assert(pthread_create(&thread, NULL, reader, NULL) == 0);
    while (!atomic_load(&reader_first_shared_page)) sched_yield();
    puts("READER FIRST SHARED ACCESS PASS");
    struct timespec wait_start, wait_now;
    assert(clock_gettime(CLOCK_MONOTONIC, &wait_start) == 0);
    puts("MAIN WAIT FIRST READER ROUND");
    while (!atomic_load(&rounds)) {
        sched_yield();
        assert(clock_gettime(CLOCK_MONOTONIC, &wait_now) == 0);
        if (wait_now.tv_sec - wait_start.tv_sec >= 60) {
            fprintf(stderr, "READER ROUND WATCHDOG: main observed rounds=%u\n",
                    atomic_load(&rounds));
            return 2;
        }
    }
    puts("READER FIRST FULL PASS");
    if (strcmp(mode, "threads") == 0) {
        atomic_store(&stop_reader, 1);
        assert(pthread_join(thread, NULL) == 0);
        verify_mapping_contents();
        assert(munmap(private_map, MAPPED) == 0 && munmap(shared_map, MAPPED) == 0);
        assert(close(fd) == 0 && unlink("/mapped-pressure-data") == 0);
        free(buffer);
        puts("MAPPED TWO-THREAD REPRO PASS");
        return 0;
    }
    int temporary = open("/mapped-pressure-temporary", O_CREAT | O_EXCL | O_RDWR, 0600);
    assert(temporary >= 0);
    puts("TEMP FILE OPEN PASS");
    pressure(temporary, buffer);
    puts("FIRST PRESSURE PASS");
    atomic_store(&stop_reader, 1);
    assert(pthread_join(thread, NULL) == 0 && atomic_load(&rounds) > 0);
    puts("READER JOIN PASS");
    assert(mprotect(shared_map, PAGE, PROT_READ | PROT_WRITE) == 0);
    shared_map[0] = 0x53;
    assert(msync(shared_map, PAGE, MS_SYNC) == 0 && fsync(fd) == 0);
    unsigned char *shared_observer = mmap(NULL, PAGE, PROT_READ, MAP_SHARED, fd, 0);
    assert(shared_observer != MAP_FAILED && shared_observer[0] == 0x53);
    assert(munmap(shared_observer, PAGE) == 0);
    puts("SHARED WRITE VISIBLE AND SYNCED");
    verify_file(fd, buffer);
    puts("SHARED WRITE FILE READ PASS");
    pressure(temporary, buffer);
    for (unsigned i = 0; i < MAPPED; i++) {
        assert(shared_map[i] == (i == 0 ? 0x53 : 0x31));
        assert(private_map[i] == (i % PAGE == 0 ? 0xa7 : 0x31));
    }
    verify_file(fd, buffer);
    assert(munmap(private_map, MAPPED) == 0 && munmap(shared_map, MAPPED) == 0);
    assert(close(fd) == 0 && close(temporary) == 0);
    assert(unlink("/mapped-pressure-temporary") == 0);
    free(buffer);
    puts("MAPPED PRESSURE PASS");
    return 0;
}
