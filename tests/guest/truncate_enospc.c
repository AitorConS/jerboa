#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

/* Use a fresh 64 MiB disk. Shrinking a full filesystem must not need data
 * allocation, and regrowing must not reveal the former contents on reboot. */
int main(void)
{
    unsigned char b[4096];
    int done = open("/full-done", O_RDONLY);
    int fd = open("/full-target", O_CREAT | O_RDWR, 0600);
    assert(fd >= 0);
    const off_t size = 1024 * 1024;
    int sparse = open("/full-sparse", O_CREAT | O_RDWR, 0600);
    assert(sparse >= 0);
    if (done < 0) {
        memset(b, 0x69, sizeof(b));
        for (off_t off = 0; off < size; off += sizeof(b))
            assert(pwrite(fd, b, sizeof(b), off) == sizeof(b));
        assert(fsync(fd) == 0);
        assert(ftruncate(sparse, size) == 0 && fsync(sparse) == 0);
        int fill = open("/filler", O_CREAT | O_WRONLY, 0600);
        assert(fill >= 0);
        int full = 0;
        for (unsigned i = 0; i < 32768; i++) {
            ssize_t n = write(fill, b, sizeof(b));
            if (n < 0) { assert(errno == ENOSPC); full = 1; break; }
            assert(n == sizeof(b));
        }
        assert(full);
        if (fsync(fill)) assert(errno == ENOSPC);
        assert(close(fill) == 0);
        assert(ftruncate(sparse, 513) == 0 && ftruncate(sparse, size) == 0);
        assert(fsync(sparse) == 0);
        assert(ftruncate(fd, 4097) == 0);
        assert(ftruncate(fd, size) == 0);
        assert(fsync(fd) == 0);
        assert(unlink("/filler") == 0);
    }
    struct stat st;
    assert(fstat(fd, &st) == 0 && st.st_size == size);
    for (off_t off = 0; off < size; off += sizeof(b)) {
        assert(pread(fd, b, sizeof(b), off) == sizeof(b));
        for (unsigned i = 0; i < sizeof(b); i++) assert(b[i] == (off + i < 4097 ? 0x69 : 0));
    }
    assert(close(fd) == 0);
    for (off_t off = 0; off < size; off += sizeof(b)) {
        assert(pread(sparse, b, sizeof(b), off) == sizeof(b));
        for (unsigned i = 0; i < sizeof(b); i++) assert(b[i] == 0);
    }
    assert(close(sparse) == 0);
    if (done < 0) {
        done = open("/full-done", O_CREAT | O_WRONLY, 0600);
        assert(done >= 0 && fsync(done) == 0);
        puts("TRUNCATE ENOSPC PASS");
    } else puts("TRUNCATE ENOSPC RESTART PASS");
    assert(close(done) == 0);
    return 0;
}
