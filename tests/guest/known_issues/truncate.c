#define _GNU_SOURCE
#include <assert.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

/* Discard dirty cached pages, then extend the same file. Old contents must not
 * reappear, including when an earlier writeback was already queued. */
int main(void)
{
    const off_t length = 32 * 1024 * 1024;
    unsigned char block[4096];
    int fd = open("/truncate-data", O_RDWR | O_CREAT | O_TRUNC, 0600);
    assert(fd >= 0);
    for (unsigned round = 1; round <= 16; round++) {
        memset(block, round, sizeof(block));
        for (off_t offset = 0; offset < length; offset += sizeof(block))
            assert(pwrite(fd, block, sizeof(block), offset) == sizeof(block));
        assert(ftruncate(fd, sizeof(block)) == 0);
        assert(ftruncate(fd, length) == 0);
        assert(fsync(fd) == 0);
        for (off_t offset = 0; offset < length; offset += sizeof(block)) {
            assert(pread(fd, block, sizeof(block), offset) == sizeof(block));
            for (unsigned i = 0; i < sizeof(block); i++)
                assert(block[i] == (offset == 0 ? round : 0));
        }
    }
    assert(close(fd) == 0 && unlink("/truncate-data") == 0);
    puts("TRUNCATE PASS");
    return 0;
}
