#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* Publishing writes to fallocated (uninited) extents needs log space: page
 * map descriptors on v6 volumes, the initialized flag on v5. Fill the /data
 * volume with fallocate until ENOSPC so the log has nowhere to grow, then
 * write at extent starts (page maps) and at random pages, with frequent
 * fsyncs. Writes and fsyncs may fail with ENOSPC or EIO; the guest must not
 * crash or hang, and must stay usable. Run with --volume-size 64M
 * --disk-size 64M: on macOS /data is then the small root disk. */

#define PAGE 4096
#define STEP (1 << 20)

int main(void)
{
    int fd = open("/data/big", O_CREAT | O_RDWR, 0644);
    assert(fd >= 0);
    off_t size = 0;
    for (off_t len = 32 << 20; len >= PAGE; len /= 2) {
        while (fallocate(fd, 0, size, len) == 0)
            size += len;
        assert(errno == ENOSPC);
    }
    printf("fallocated %ld MiB\n", (long)(size >> 20));
    assert(size > (16 << 20));

    static char b[PAGE];
    unsigned seed = 1;
    int write_errors = 0, sync_errors = 0, ok_syncs = 0;
    for (int round = 0; round < 400; round++) {
        memset(b, round & 0xff, sizeof(b));
        /* One page at each step start, then random pages. */
        for (off_t o = 0; o + PAGE <= size; o += STEP)
            if (pwrite(fd, b, PAGE, o + (off_t)(round % 8) * PAGE) != PAGE) {
                assert(errno == ENOSPC || errno == EIO);
                write_errors++;
            }
        for (int i = 0; i < 64; i++) {
            seed = seed * 1103515245u + 12345u;
            off_t o = ((off_t)(seed >> 4) % (size / PAGE)) * PAGE;
            if (pwrite(fd, b, PAGE, o) != PAGE) {
                assert(errno == ENOSPC || errno == EIO);
                write_errors++;
            }
        }
        if (fsync(fd) != 0) {
            assert(errno == ENOSPC || errno == EIO);
            sync_errors++;
        } else {
            ok_syncs++;
        }
    }
    printf("write errors %d, fsync errors %d, fsyncs ok %d\n", write_errors, sync_errors, ok_syncs);
    close(fd);
    /* Still usable: the file can be removed and the space written again. */
    assert(unlink("/data/big") == 0);
    fd = open("/data/after", O_CREAT | O_TRUNC | O_WRONLY, 0644);
    assert(fd >= 0);
    memset(b, 0x7e, sizeof(b));
    for (int i = 0; i < 256; i++)
        assert(write(fd, b, PAGE) == PAGE);
    assert(fsync(fd) == 0);
    close(fd);
    printf("MAP ENOSPC PASS\n");
    return 0;
}
