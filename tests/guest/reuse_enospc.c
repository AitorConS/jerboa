#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

/* Released storage must become reusable without an explicit sync. Fill the
 * /data volume until ENOSPC, then repeatedly delete or truncate the filler and
 * immediately write the same amount again; no write may fail. Released blocks
 * are only reused once their release is durable, so the allocation has to
 * wait for that rather than fail. Run with --volume-size 64M. */

#define CHUNK (256 * 1024)

static long fill(const char *path, long limit)
{
    static char b[CHUNK];
    memset(b, 0x5c, sizeof(b));
    int fd = open(path, O_CREAT | O_TRUNC | O_WRONLY, 0644);
    assert(fd >= 0);
    long total = 0;
    while (limit < 0 || total < limit) {
        ssize_t n = write(fd, b, sizeof(b));
        if (n < 0) {
            assert(errno == ENOSPC);
            break;
        }
        assert(n > 0);
        total += n;
    }
    close(fd);
    return total;
}

int main(void)
{
    long full = fill("/data/filler", -1);
    assert(full > (32 << 20));
    for (int i = 0; i < 8; i++) {
        if (i & 1) {
            int fd = open("/data/filler", O_WRONLY);
            assert(fd >= 0 && ftruncate(fd, 0) == 0);
            close(fd);
        } else {
            assert(unlink("/data/filler") == 0);
        }
        long again = fill("/data/filler", full - (1 << 20));
        printf("REUSE ENOSPC round %d wrote %ld of %ld\n", i, again, full);
        assert(again >= full - (1 << 20));
    }
    int fd = open("/data/filler", O_WRONLY);
    assert(fd >= 0 && fsync(fd) == 0);
    close(fd);
    printf("REUSE ENOSPC PASS\n");
    return 0;
}
