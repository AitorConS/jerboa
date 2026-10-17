/* Large contiguous writeback must be split into supported virtio requests.
 * Run with 2 GiB RAM, then reboot the same disk to check persistence. */
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define CHUNK (16 * 1024 * 1024)
#define CHUNKS 8

int main(void)
{
    unsigned char *buffer = malloc(CHUNK);
    assert(buffer);
    int fd = open("/large-write", O_RDWR);
    int existed = fd >= 0;
    if (!existed) {
        assert(errno == ENOENT);
        fd = open("/large-write", O_CREAT | O_EXCL | O_RDWR, 0600);
        assert(fd >= 0);
        for (int i = 0; i < CHUNKS; i++) {
            memset(buffer, 0x31 + i, CHUNK);
            assert(write(fd, buffer, CHUNK) == CHUNK);
        }
        assert(fsync(fd) == 0);
    }
    assert(lseek(fd, 0, SEEK_SET) == 0);
    for (int i = 0; i < CHUNKS; i++) {
        assert(read(fd, buffer, CHUNK) == CHUNK);
        for (int j = 0; j < CHUNK; j++)
            assert(buffer[j] == 0x31 + i);
    }
    assert(close(fd) == 0);
    free(buffer);
    puts(existed ? "LARGE WRITE RESTART PASS" : "LARGE WRITE PASS");
    return 0;
}
