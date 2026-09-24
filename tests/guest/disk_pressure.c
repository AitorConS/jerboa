#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define CHUNK (1024 * 1024)
#define CHUNKS 1024

/* First boot writes more than guest RAM. Second boot checks persisted bytes,
 * fills the remaining disk, and verifies that ENOSPC leaves existing data intact.
 * Run with a 2 GiB disk and 256 MiB of RAM. */
static void verify(int fd, unsigned char *buffer)
{
    assert(lseek(fd, 0, SEEK_SET) == 0);
    for (int i = 0; i < CHUNKS; i++) {
        assert(read(fd, buffer, CHUNK) == CHUNK);
        for (int j = 0; j < CHUNK; j++)
            assert(buffer[j] == i % 251);
    }
}

int main(void)
{
    unsigned char *buffer = malloc(CHUNK);
    assert(buffer);
    int fd = open("/pressure", O_RDWR);
    if (fd < 0) {
        assert(errno == ENOENT);
        fd = open("/pressure", O_RDWR | O_CREAT | O_EXCL, 0600);
        assert(fd >= 0);
        for (int i = 0; i < CHUNKS; i++) {
            memset(buffer, i % 251, CHUNK);
            assert(write(fd, buffer, CHUNK) == CHUNK);
        }
        assert(fsync(fd) == 0);
        verify(fd, buffer);
        assert(close(fd) == 0);
        puts("DISK WRITE PASS");
    } else {
        verify(fd, buffer);
        puts("DISK RESTART PASS");
        fflush(stdout);
        int full = open("/fill", O_WRONLY | O_CREAT | O_TRUNC, 0600);
        assert(full >= 0);
        memset(buffer, 0x5a, CHUNK);
        int out_of_space = 0;
        for (int i = 0; i < 4096; i++) {
            ssize_t n = write(full, buffer, CHUNK);
            if (n < 0) {
                fprintf(stderr, "fill stopped at %d MiB: %s\n", i, strerror(errno));
                assert(errno == ENOSPC);
                out_of_space = 1;
                break;
            }
        }
        assert(out_of_space);
        if (fsync(full) < 0) assert(errno == ENOSPC);
        assert(close(full) == 0);
        assert(unlink("/fill") == 0);
        verify(fd, buffer);
        assert(close(fd) == 0);
        puts("DISK FULL PASS");
    }
    free(buffer);
    return 0;
}
